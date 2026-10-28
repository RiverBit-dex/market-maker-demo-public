// Package hlexchange implements the Hyperliquid exchange wire protocol.
// Struct field order is significant: the same action is MessagePack-hashed and
// JSON-submitted. Do not replace signed structs with unordered maps.
package hlexchange

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/vmihailenco/msgpack/v5"
)

type Limit struct {
	TIF string `json:"tif" msgpack:"tif"`
}
type OrderType struct {
	Limit Limit `json:"limit" msgpack:"limit"`
}
type Order struct {
	Asset      int       `json:"a" msgpack:"a"`
	Buy        bool      `json:"b" msgpack:"b"`
	Price      string    `json:"p" msgpack:"p"`
	Size       string    `json:"s" msgpack:"s"`
	ReduceOnly bool      `json:"r" msgpack:"r"`
	Type       OrderType `json:"t" msgpack:"t"`
	Cloid      string    `json:"c,omitempty" msgpack:"c,omitempty"`
}
type Place struct {
	Type     string  `json:"type" msgpack:"type"`
	Orders   []Order `json:"orders" msgpack:"orders"`
	Grouping string  `json:"grouping" msgpack:"grouping"`
}
type CancelItem struct {
	Asset int    `json:"a" msgpack:"a"`
	OID   uint64 `json:"o" msgpack:"o"`
}
type Cancel struct {
	Type    string       `json:"type" msgpack:"type"`
	Cancels []CancelItem `json:"cancels" msgpack:"cancels"`
}
type CancelByCloidItem struct {
	Asset int   `json:"asset" msgpack:"asset"`
	Cloid Cloid `json:"cloid" msgpack:"cloid"`
}
type CancelByCloid struct {
	Type    string              `json:"type" msgpack:"type"`
	Cancels []CancelByCloidItem `json:"cancels" msgpack:"cancels"`
	Fast    *bool               `json:"f,omitempty" msgpack:"f,omitempty"`
}
type Modify struct {
	Type  string `json:"type" msgpack:"type"`
	OID   uint64 `json:"oid" msgpack:"oid"`
	Order Order  `json:"order" msgpack:"order"`
}
type BatchModify struct {
	Type     string       `json:"type" msgpack:"type"`
	Modifies []ModifyItem `json:"modifies" msgpack:"modifies"`
}
type ModifyItem struct {
	OID   uint64 `json:"oid" msgpack:"oid"`
	Order Order  `json:"order" msgpack:"order"`
}
type Signature struct {
	R string `json:"r"`
	S string `json:"s"`
	V byte   `json:"v"`
}
type OpenOrder struct {
	Coin  string `json:"coin"`
	OID   uint64 `json:"oid"`
	Side  string `json:"side"`
	Price string `json:"limitPx"`
	Size  string `json:"sz"`
	Cloid string `json:"cloid"`
}
type Client struct {
	URL string
	// WS, when set, carries signed exchange actions over a logged-in bin-ws
	// session (action_json) instead of HTTP POST <URL>/exchange. Both paths
	// send the same signed JSON bytes.
	WS ActionTransport
	// Queries, when set, carries the order reconciliation queries (orderStatus
	// and openOrders) over bin-ws info_json instead of HTTP POST <URL>/info.
	// Every other query (chainIdentity, metaAndAssetCtxs, userRole, vault and
	// subaccount checks) always uses HTTP /info.
	Queries InfoTransport
	HTTP    *http.Client
	key     *ecdsa.PrivateKey
	Mainnet bool
	// SignChainID is the EIP-712 domain chainId of the L1 phantom-agent signature.
	// Zero keeps the Hyperliquid value (1337); chains with their own signing domain
	// (RiverBit pre-api = 12589) must set it, otherwise the recovered signer is
	// a random address and every action is rejected with code 91.
	SignChainID uint64
	account     string
	// principal is the master account an API wallet acts for when no vault or
	// subaccount is targeted. It only changes which account is queried; signing
	// still uses the 0x00 connectionId tail.
	principal string
	// VaultOperator marks account as a registered vault that this signer trades
	// for as the vault leader or the leader's agent, not as a master owning a
	// subaccount. VerifySubAccount then checks vaultRegistry, vaultDetails and
	// userRole instead.
	VaultOperator          bool
	StateDir               string        // Dedicated durable pending requests and public audit records.
	ReceiptLog             string        // Optional local devnet execution log, read-only.
	MaxStateAge            time.Duration // Zero uses the runner's normal 60-second limit.
	LastOrderIDs           []uint64      // Order IDs observed for the most recently resolved action.
	LastExecutionHeight    uint64        // Authoritative orderStatus projection height.
	RequireExecutionHeight bool          // Fail closed when read-your-writes evidence is unavailable.
	LastStatuses           []string
	submitMu               sync.Mutex
	mu                     sync.Mutex
	nonce                  uint64
}

// NewForVault configures a Guild (or other registered) vault target. The caller
// must call VerifySubAccount before submitting any action.
func NewForVault(url, keyHex string, mainnet bool, vault string) (*Client, error) {
	target, err := normalizeAddress(vault)
	if err != nil {
		return nil, err
	}
	c, err := New(url, keyHex, mainnet)
	if err != nil {
		return nil, err
	}
	c.account, c.VaultOperator = target, true
	return c, nil
}

func New(url, keyHex string, mainnet bool) (*Client, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(keyHex), "0x"))
	if err != nil {
		return nil, err
	}
	return &Client{URL: strings.TrimRight(url, "/"), HTTP: &http.Client{Timeout: 15 * time.Second}, key: key, Mainnet: mainnet}, nil
}

// Address retains the historical signer identity. Trading queries use AccountAddress.
func (c *Client) Address() string { return c.SignerAddress() }
func (c *Client) SignerAddress() string {
	return strings.ToLower(crypto.PubkeyToAddress(c.key.PublicKey).Hex())
}
func (c *Client) AccountAddress() string {
	if c.account != "" {
		return c.account
	}
	if c.principal != "" {
		return c.principal
	}
	return c.SignerAddress()
}

// SetPrincipal records the master account that an API wallet trades for (for
// example the user resolved by the bin-ws login). Only for clients without a
// vault or subaccount target; it does not change signatures.
func (c *Client) SetPrincipal(address string) error {
	if c.account != "" {
		return fmt.Errorf("principal cannot be set on a vault or subaccount client")
	}
	target, err := normalizeAddress(address)
	if err != nil {
		return err
	}
	c.principal = target
	return nil
}
func normalizeAddress(address string) (string, error) {
	if len(address) != 42 || !strings.HasPrefix(address, "0x") {
		return "", fmt.Errorf("invalid account address")
	}
	b, err := hex.DecodeString(address[2:])
	if err != nil || len(b) != 20 || bytes.Equal(b, make([]byte, 20)) {
		return "", fmt.Errorf("invalid account address")
	}
	return strings.ToLower(address), nil
}

// isDefaultAck accepts exactly {"type":"default"}; any additional field is rejected.
func isDefaultAck(response json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(response, &fields) != nil || len(fields) != 1 {
		return false
	}
	var typ string
	return json.Unmarshal(fields["type"], &typ) == nil && typ == "default"
}

// NewForSubAccount fixes the target for the lifetime of a client; no setter can
// silently change the identity of an outstanding durable intent.
func NewForSubAccount(url, keyHex string, mainnet bool, account string) (*Client, error) {
	target, err := normalizeAddress(account)
	if err != nil {
		return nil, err
	}
	c, err := New(url, keyHex, mainnet)
	if err != nil {
		return nil, err
	}
	if target == c.SignerAddress() {
		return nil, fmt.Errorf("subaccount must differ from signer")
	}
	c.account = target
	return c, nil
}

var signerOperations sync.Map

func (c *Client) signerOperationLock() *sync.Mutex {
	value, _ := signerOperations.LoadOrStore(c.SignerAddress(), new(sync.Mutex))
	return value.(*sync.Mutex)
}

var signerNonces = struct {
	sync.Mutex
	values map[string]uint64
}{values: make(map[string]uint64)}

// Nonces are shared by all clients with the same key in this process. Separate
// processes must use a single signer owner; this is not a distributed allocator.
func (c *Client) nextNonce() uint64 {
	signerNonces.Lock()
	defer signerNonces.Unlock()
	identity := "test"
	if c.key != nil {
		identity = c.SignerAddress()
	}
	n := uint64(time.Now().UnixMilli())
	if n <= signerNonces.values[identity] {
		n = signerNonces.values[identity] + 1
	}
	if n <= c.nonce {
		n = c.nonce + 1
	}
	c.nonce = n
	signerNonces.values[identity] = n
	return n
}
func word(n uint64) []byte        { b := make([]byte, 32); binary.BigEndian.PutUint64(b[24:], n); return b }
func hash(parts ...[]byte) []byte { return crypto.Keccak256(parts...) }

// DefaultL1SignChainID is the Hyperliquid L1 signing domain chain id.
const DefaultL1SignChainID uint64 = 1337

func (c *Client) signChainID() uint64 {
	if c.SignChainID == 0 {
		return DefaultL1SignChainID
	}
	return c.SignChainID
}

func domain(name string, chain uint64) []byte {
	return hash(hash([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)")), hash([]byte(name)), hash([]byte("1")), word(chain), make([]byte, 32))
}
func (c *Client) signDigest(d []byte) (Signature, error) {
	s, err := crypto.Sign(d, c.key)
	if err != nil {
		return Signature{}, err
	}
	return Signature{fmt.Sprintf("0x%x", new(big.Int).SetBytes(s[:32])), fmt.Sprintf("0x%x", new(big.Int).SetBytes(s[32:64])), s[64] + 27}, nil
}

// connectionID is keccak256(msgpack(action) || nonce u64 big-endian || tail),
// where tail is 0x00 without a target account, or 0x01 followed by the 20-byte
// vault (or subaccount) address when the request carries vaultAddress.
func (c *Client) connectionID(action any, nonce uint64) ([]byte, error) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.UseCompactInts(true)
	if err := enc.Encode(action); err != nil {
		return nil, err
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, nonce)
	buf.Write(b)
	if c.account == "" {
		buf.WriteByte(0)
	} else {
		target, err := hex.DecodeString(c.account[2:])
		if err != nil || len(target) != 20 {
			return nil, fmt.Errorf("invalid vault address for connectionId")
		}
		buf.WriteByte(1)
		buf.Write(target)
	}
	return hash(buf.Bytes()), nil
}

func (c *Client) signingDigest(action any, nonce uint64) ([]byte, error) {
	connectionID, err := c.connectionID(action, nonce)
	if err != nil {
		return nil, err
	}
	source := "b"
	if c.Mainnet {
		source = "a"
	}
	agent := hash(hash([]byte("Agent(string source,bytes32 connectionId)")), hash([]byte(source)), connectionID)
	return hash([]byte{0x19, 0x01}, domain("Exchange", c.signChainID()), agent), nil
}
func (c *Client) Sign(action any, nonce uint64) (Signature, error) {
	digest, err := c.signingDigest(action, nonce)
	if err != nil {
		return Signature{}, err
	}
	return c.signDigest(digest)
}
func signatureBytes(s Signature) ([]byte, error) {
	r, ok := new(big.Int).SetString(strings.TrimPrefix(s.R, "0x"), 16)
	if !ok || r.Sign() <= 0 || r.BitLen() > 256 {
		return nil, fmt.Errorf("invalid signature r")
	}
	v, ok := new(big.Int).SetString(strings.TrimPrefix(s.S, "0x"), 16)
	if !ok || v.Sign() <= 0 || v.BitLen() > 256 {
		return nil, fmt.Errorf("invalid signature s")
	}
	if s.V != 27 && s.V != 28 {
		return nil, fmt.Errorf("invalid signature v")
	}
	out := make([]byte, crypto.SignatureLength)
	r.FillBytes(out[:32])
	v.FillBytes(out[32:64])
	out[64] = s.V - 27
	return out, nil
}
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.postRaw(ctx, path, data, out)
}

// postRaw POSTs data unchanged to <URL><path>. Any non-200 answer is an error
// (for /exchange that means the outcome is unknown, not rejected).
func (c *Client) postRaw(ctx context.Context, path string, data []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s HTTP %d: %.500s", path, resp.StatusCode, raw)
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s invalid JSON: %w", path, err)
	}
	return nil
}

// retryRateLimited repeats call with capped exponential backoff while
// limited(err) reports a 429.
func retryRateLimited(ctx context.Context, call func() error, limited func(error) bool) error {
	delay := 250 * time.Millisecond
	for {
		err := call()
		if err == nil || !limited(err) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < 8*time.Second {
			delay *= 2
			if delay > 8*time.Second {
				delay = 8 * time.Second
			}
		}
	}
}

// Info always queries HTTP POST <URL>/info.
func (c *Client) Info(ctx context.Context, body, out any) error {
	return retryRateLimited(ctx, func() error { return c.Post(ctx, "/info", body, out) },
		func(err error) bool { return strings.Contains(err.Error(), "/info HTTP 429:") })
}

// orderQuery serves the reconciliation queries (orderStatus, openOrders):
// over bin-ws info_json when Queries is set, otherwise over HTTP /info.
func (c *Client) orderQuery(ctx context.Context, body, out any) error {
	if c.Queries != nil {
		return c.wsInfo(ctx, body, out)
	}
	return c.Info(ctx, body, out)
}

func (c *Client) OpenOrders(ctx context.Context) ([]OpenOrder, error) {
	var orders []OpenOrder
	err := c.orderQuery(ctx, map[string]any{"type": "openOrders", "user": c.AccountAddress()}, &orders)
	return orders, err
}

// Exchange checks every status: HTTP 200 and even status=ok can contain errors.
// Callers must still reconcile openOrders after every mutation or timeout.
func (c *Client) Exchange(ctx context.Context, action any) error {
	lock := c.signerOperationLock()
	lock.Lock()
	defer lock.Unlock()
	release, err := c.lockDurableSigner()
	if err != nil {
		return err
	}
	defer release()
	if err := c.RestoreExecutionHeight(); err != nil {
		return err
	}
	if c.StateDir != "" {
		return c.trackedExchange(ctx, action)
	}
	n, err := c.durableNonce()
	if err != nil {
		return err
	}
	return c.exchangeNonce(ctx, action, n)
}
func (c *Client) exchangeNonce(ctx context.Context, action any, n uint64) error {

	digest, err := c.signingDigest(action, n)
	if err != nil {
		return err
	}
	s, err := c.signDigest(digest)
	if err != nil {
		return err
	}
	rawSignature, err := signatureBytes(s)
	if err != nil {
		return err
	}
	publicKey, err := crypto.SigToPub(digest, rawSignature)
	if err != nil {
		return fmt.Errorf("recover signer from action signature: %w", err)
	}
	recovered := strings.ToLower(crypto.PubkeyToAddress(*publicKey).Hex())
	if recovered != c.SignerAddress() {
		return fmt.Errorf("action signature signer mismatch: recovered %s", recovered)
	}
	log.Printf("hl action signature verified signer=%s nonce=%d digest=0x%x", recovered, n, digest)
	return c.submit(ctx, action, n, s)
}
func (c *Client) submit(ctx context.Context, action any, n uint64, s Signature) error {
	var result struct {
		Status          string          `json:"status"`
		Response        json.RawMessage `json:"response"`
		CommittedResult json.RawMessage `json:"committedResult"`
	}
	envelope := struct {
		Action       any       `json:"action"`
		Nonce        uint64    `json:"nonce"`
		Signature    Signature `json:"signature"`
		VaultAddress string    `json:"vaultAddress,omitempty"`
	}{action, n, s, c.account}
	// One serialization for both transports: REST POST /exchange and bin-ws
	// action_json carry exactly these bytes (struct order = signing key order).
	body, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if c.WS != nil {
		err = c.postBinary(ctx, body, &result)
	} else {
		err = c.postREST(ctx, body, &result)
	}
	if err != nil {
		return err
	}
	// Preserve complete per-leg results before an individual error discards
	// them. A rejected replacement has no new cloid to discover via /info.
	if c.StateDir != "" {
		count := 0
		switch batch := action.(type) {
		case BatchModify:
			count = len(batch.Modifies)
		case *BatchModify:
			count = len(batch.Modifies)
		}
		raw := result.Response
		if result.Status == "err" {
			raw = result.CommittedResult
		}
		if count > 0 && (result.Status == "ok" || result.Status == "err") {
			if _, e := parseBatchReceipt(raw, count); e == nil {
				return &batchReceipt{Raw: raw}
			}
		}
		if count := cancelCount(action); count > 0 && (result.Status == "ok" || result.Status == "err") {
			if _, e := parseCancelReceipt(raw, count); e == nil {
				return &cancelReceipt{Raw: raw}
			}
		}
	}
	if result.Status != "ok" {
		if code, ok := finalRejection(result.Status, result.Response, result.CommittedResult); ok {
			return &ActionRejectedError{Status: result.Status, Response: result.Response, Code: code}
		}
		return fmt.Errorf("exchange: %s %s", result.Status, result.Response)
	}
	var response struct {
		Data struct {
			Statuses []json.RawMessage `json:"statuses"`
		} `json:"data"`
	}
	if err = json.Unmarshal(result.Response, &response); err != nil {
		return fmt.Errorf("exchange response: %s", result.Response)
	}
	if len(response.Data.Statuses) == 0 {
		// An accepted on-chain modify returns only {"type":"default"}; rejections carry
		// data.statuses. Acceptance is not final: callers must confirm via /info.
		// Missing statuses for other actions remains an error.
		if isModifyAction(action) && isDefaultAck(result.Response) {
			return nil
		}
		return fmt.Errorf("exchange missing statuses: %s", result.Response)
	}
	for _, raw := range response.Data.Statuses {
		if string(raw) == `"success"` {
			continue
		}
		var status struct {
			Error   string `json:"error"`
			Resting *struct {
				OID uint64 `json:"oid"`
			} `json:"resting"`
			Filled *struct {
				OID uint64 `json:"oid"`
			} `json:"filled"`
		}
		if err := json.Unmarshal(raw, &status); err != nil {
			return fmt.Errorf("exchange unknown status: %s", raw)
		}
		if status.Error != "" {
			return fmt.Errorf("exchange: %s", status.Error)
		}
		if status.Resting == nil && status.Filled == nil {
			return fmt.Errorf("exchange unconfirmed status: %s", raw)
		}
	}
	return nil
}

func isModifyAction(action any) bool {
	switch action.(type) {
	case Modify, *Modify, BatchModify, *BatchModify:
		return true
	default:
		return false
	}
}

// CodeAccountNotFound is the execution-layer code for "User or API Wallet does
// not exist": the signer/account has never been activated on chain.
const CodeAccountNotFound = 91

// ErrAccountNotActivated marks a deterministic rejection with code 91. The
// account must be funded (faucet) before any action can execute.
var ErrAccountNotActivated = errors.New("account not activated on chain (code 91: User or API Wallet does not exist); faucet required")

var executionRejectionRE = regexp.MustCompile(`action rejected by execution \(code (\d+)\)`)

// ActionRejectedError is a gateway reply status="err" carrying the execution
// layer's explicit rejection. Nothing executed, so the request is final.
type ActionRejectedError struct {
	Status   string
	Response json.RawMessage
	Code     int
}

func (e *ActionRejectedError) Error() string {
	return fmt.Sprintf("exchange: %s %s", e.Status, e.Response)
}

// ambiguousErrHints are node diagnostics for status=err replies that may still
// carry effects: the action produced results, or a phase is only waiting. They
// tell the caller to query the order status, so the request is not final.
var ambiguousErrHints = []string{"query the order status", "waiting for its trigger", "not verified", "already pending"}

// finalRejection reports whether a status=err reply means nothing executed. HTTP
// failures never get here (Post returns them as errors). A reply with a
// committedResult with possible effects, or text asking to query before retrying, may
// have effects and stays ambiguous; every other plain err (for example "Attempted
// to modify to invalid new order") is the node refusing the action.
func finalRejection(status string, response, committed json.RawMessage) (int, bool) {
	if status != "err" {
		return 0, false
	}
	if len(committed) > 0 && string(committed) != "null" {
		return 0, committedOrderRejected(committed)
	}
	if code, ok := executionRejectionCode(status, response); ok {
		return code, true
	}
	var text string
	if json.Unmarshal(response, &text) != nil || text == "" {
		return 0, false
	}
	for _, hint := range ambiguousErrHints {
		if strings.Contains(strings.ToLower(text), hint) {
			return 0, false
		}
	}
	return 0, true
}

// The Rust node wraps rejected modify legs in committedResult too. A nonempty
// order result containing only plain errors proves terminal rejection. Mixed
// outcomes, localCommitted hints, waiting states and unknown fields do not.
func committedOrderRejected(raw json.RawMessage) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || len(envelope) != 2 || string(envelope["type"]) != `"order"` {
		return false
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(envelope["data"], &data) != nil || len(data) != 1 {
		return false
	}
	var statuses []map[string]json.RawMessage
	if json.Unmarshal(data["statuses"], &statuses) != nil || len(statuses) == 0 {
		return false
	}
	for _, leg := range statuses {
		if len(leg) != 1 {
			return false
		}
		if _, ok := finalRejection("err", leg["error"], nil); !ok {
			return false
		}
	}
	return true
}

// Only the exact deterministic phrase carries an execution code.
func executionRejectionCode(status string, response json.RawMessage) (int, bool) {
	if status != "err" {
		return 0, false
	}
	m := executionRejectionRE.FindSubmatch(response)
	if m == nil {
		return 0, false
	}
	code, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0, false
	}
	return code, true
}

// DevnetFaucet is the Rust local-devnet extension, never a production funding path.
func (c *Client) DevnetFaucet(ctx context.Context) error {
	return c.DevnetFaucetForChain(ctx, 998)
}

// DevnetFaucetForChain submits the test faucet action using the target chain's
// EIP-712 signature chain ID. Rust testnets can use a non-local chain ID while
// retaining the same RiverBit action schema.
func (c *Client) DevnetFaucetForChain(ctx context.Context, chainID uint64) error {
	if c.account != "" {
		return fmt.Errorf("devnet faucet requires a master account client")
	}
	n := c.nextNonce()
	a := map[string]any{"type": "devnetFaucet", "signatureChainId": fmt.Sprintf("0x%x", chainID), "riverbitChain": "Local", "nonce": n}
	sh := hash(hash([]byte("RiverBitTransaction:DevnetFaucet(string riverbitChain,uint64 nonce)")), hash([]byte("Local")), word(n))
	s, err := c.signDigest(hash([]byte{0x19, 0x01}, domain("RiverBitSignTransaction", chainID), sh))
	if err != nil {
		return err
	}
	var out json.RawMessage
	err = c.Post(ctx, "/exchange", map[string]any{"action": a, "nonce": n, "signature": s}, &out)
	if err != nil {
		return err
	}
	// Return the response to operators if the local extension refuses credit.
	var response struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return err
	}
	if response.Status != "credited" {
		return fmt.Errorf("devnet faucet: %s", out)
	}
	return nil
}

// VerifySubAccount checks the published chain ownership relation before readiness.
// An empty list, malformed relation, or unavailable API fails closed.
func (c *Client) VerifySubAccount(ctx context.Context) error {
	if c.account == "" {
		return nil
	}
	if c.VaultOperator {
		return c.verifyVaultOperator(ctx)
	}
	var accounts []struct {
		Name   string `json:"name"`
		User   string `json:"subAccountUser"`
		Master string `json:"master"`
	}
	if err := c.Info(ctx, map[string]any{"type": "subAccounts", "user": c.SignerAddress()}, &accounts); err != nil {
		return err
	}
	found := false
	for _, a := range accounts {
		user, err := normalizeAddress(a.User)
		if err != nil {
			return fmt.Errorf("invalid subaccount relation")
		}
		master, err := normalizeAddress(a.Master)
		if err != nil || master != c.SignerAddress() {
			return fmt.Errorf("subaccount master mismatch")
		}
		if user == c.account {
			if found {
				return fmt.Errorf("duplicate subaccount relation")
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("subaccount is not registered to signer")
	}
	return nil
}

// verifyVaultOperator fails closed unless the target is an active registered
// vault whose leader is the signer or has approved the signer as its agent.
// Every registered class (protocol / guild / priority / user) trades only
// through the leader's API; platform admins do not execute for vaults.
func (c *Client) verifyVaultOperator(ctx context.Context) error {
	var vault struct {
		Address string `json:"vaultAddress"`
		Status  string `json:"status"`
		Class   string `json:"class"`
	}
	if err := c.Info(ctx, map[string]any{"type": "vaultRegistry", "vault": c.account}, &vault); err != nil {
		return err
	}
	addr, err := normalizeAddress(vault.Address)
	if err != nil || addr != c.account {
		return fmt.Errorf("vault registry address mismatch")
	}
	if vault.Status != "normal" {
		return fmt.Errorf("vault is not normal: %q", vault.Status)
	}
	switch vault.Class {
	case "protocol", "priority", "guild", "user":
		return c.verifyVaultLeader(ctx)
	default:
		return fmt.Errorf("vault class %q has no known executor", vault.Class)
	}
}

func (c *Client) verifyVaultLeader(ctx context.Context) error {
	var details struct {
		Leader string `json:"leader"`
	}
	if err := c.Info(ctx, map[string]any{"type": "vaultDetails", "vaultAddress": c.account}, &details); err != nil {
		return err
	}
	leader, err := normalizeAddress(details.Leader)
	if err != nil {
		return fmt.Errorf("invalid vault leader")
	}
	if leader == c.SignerAddress() {
		return nil
	}
	var role struct {
		Role string `json:"role"`
		Data struct {
			User string `json:"user"`
		} `json:"data"`
	}
	if err := c.Info(ctx, map[string]any{"type": "userRole", "user": c.SignerAddress()}, &role); err != nil {
		return err
	}
	if role.Role == "agent" {
		if principal, err := normalizeAddress(role.Data.User); err == nil && principal == leader {
			return nil
		}
	}
	return fmt.Errorf("signer %s is neither the vault leader nor its agent", c.SignerAddress())
}
