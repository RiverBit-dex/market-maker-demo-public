package hlexchange

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/riverbit-dex/market-maker-demo-public/internal/binws"
	"github.com/riverbit-dex/market-maker-demo-public/internal/binws/binwstest"
)

const testVault = "0x0102030405060708090a0b0c0d0e0f1011121314"

type cDeposit struct {
	Type string `json:"type" msgpack:"type"`
}

// Vectors from riverbit-chain l1/src/action/rmp_signable.rs, as used by
// riverbit-ws scripts/e2e-bin-ws.py: action {"type":"cDeposit"}, nonce 0x0102030405060708.
func TestConnectionIDVectors(t *testing.T) {
	plain, err := New("https://example.invalid", strings.Repeat("0", 63)+"1", false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := plain.connectionID(cDeposit{"cDeposit"}, 0x0102030405060708)
	if err != nil {
		t.Fatal(err)
	}
	if want := "f95c8d88b9fef7a40a319f22b71053452e5da31126895f2c7c082b272f9b01ec"; hex.EncodeToString(got) != want {
		t.Fatalf("connectionId without vault = %x, want %s", got, want)
	}
	vault, err := NewForVault("https://example.invalid", strings.Repeat("0", 63)+"1", false, testVault)
	if err != nil {
		t.Fatal(err)
	}
	got, err = vault.connectionID(cDeposit{"cDeposit"}, 0x0102030405060708)
	if err != nil {
		t.Fatal(err)
	}
	// Tail = 0x01 || 20-byte vault 0x0102..14.
	if want := "de98d071b0f17c1852d73c01848a297bd79ac8fdb6638fcf38eec5081915be2f"; hex.EncodeToString(got) != want {
		t.Fatalf("connectionId with vault = %x, want %s", got, want)
	}
	// SetPrincipal only changes the queried account, never the signed tail.
	if err := plain.SetPrincipal(testVault); err != nil {
		t.Fatal(err)
	}
	again, _ := plain.connectionID(cDeposit{"cDeposit"}, 0x0102030405060708)
	if hex.EncodeToString(again) != "f95c8d88b9fef7a40a319f22b71053452e5da31126895f2c7c082b272f9b01ec" || plain.AccountAddress() != testVault {
		t.Fatalf("principal changed the signature tail or was not applied")
	}
	if err := vault.SetPrincipal(testVault); err == nil {
		t.Fatal("SetPrincipal on a vault client must fail")
	}
}

func TestPersonalSignRecoversSigner(t *testing.T) {
	client, err := New("https://example.invalid", strings.Repeat("0", 63)+"1", false)
	if err != nil {
		t.Fatal(err)
	}
	login, err := binws.NewLogin("pre-api.riverbit.io", testNow, nil, client.PersonalSign)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := binws.RecoverLoginSigner("pre-api.riverbit.io", login); err != nil || got != client.SignerAddress() {
		t.Fatalf("recovered %s (%v), want %s", got, err, client.SignerAddress())
	}
}

// fakeExchange is a bin-ws endpoint plus an HTTP /info for orderStatus reconciliation.
type fakeExchange struct {
	t        *testing.T
	ws       *binwstest.Server
	info     *httptest.Server
	mu       sync.Mutex
	requests []json.RawMessage // action_json bodies received
	canceled map[string]bool   // cloids reported canceled
}

func newFakeExchange(t *testing.T, client *Client) *fakeExchange {
	f := &fakeExchange{t: t, canceled: map[string]bool{}}
	f.ws = binwstest.New(t, "pre-api.riverbit.io")
	f.ws.User = "0x9999999999999999999999999999999999999999"
	f.ws.Handle = func(c *binwstest.Conn, frame binws.ClientFrame) {
		if frame.Body != binws.ClientAction {
			return
		}
		if frame.ID == 0 {
			t.Errorf("action frame without id")
		}
		f.mu.Lock()
		f.requests = append(f.requests, append(json.RawMessage(nil), frame.Data...))
		f.mu.Unlock()
		payload := f.verify(client, frame.Data)
		_ = c.SendJSON(0, `{"channel":"orderUpdates","data":[]}`) // a push before the reply
		_ = c.SendJSON(frame.ID, fmt.Sprintf(`{"channel":"post","data":{"id":%d,"response":{"type":"action","payload":%s}}}`, frame.ID, payload))
	}
	f.info = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Type string `json:"type"`
			User string `json:"user"`
			OID  any    `json:"oid"`
		}
		if r.URL.Path != "/info" || json.NewDecoder(r.Body).Decode(&q) != nil || q.Type != "orderStatus" {
			t.Errorf("unexpected HTTP request %s", r.URL.Path)
			return
		}
		if q.User != testVault {
			t.Errorf("orderStatus user = %s, want the vault", q.User)
		}
		status := "open"
		orderID := uint64(4242)
		switch oid := q.OID.(type) {
		case float64:
			status = "canceled" // cancel / modify targets by oid
			orderID = uint64(oid)
		case string:
			f.mu.Lock()
			if f.canceled[oid] {
				status = "canceled"
			}
			f.mu.Unlock()
		}
		fmt.Fprintf(w, `{"height":7,"status":"order","order":{"status":%q,"order":{"coin":"RBT","oid":%d}}}`, status, orderID)
	}))
	t.Cleanup(f.info.Close)
	return f
}

// verify checks the wire shape and the vault-bound signature, then returns the node payload.
func (f *fakeExchange) verify(client *Client, body []byte) string {
	t := f.t
	keys, err := topLevelKeys(body)
	if err != nil || strings.Join(keys, ",") != "action,nonce,signature,vaultAddress" {
		t.Errorf("request keys = %v (%v): %s", keys, err, body)
	}
	var req struct {
		Action       json.RawMessage `json:"action"`
		Nonce        uint64          `json:"nonce"`
		Signature    Signature       `json:"signature"`
		VaultAddress string          `json:"vaultAddress"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.VaultAddress != testVault {
		t.Errorf("request = %s (%v)", body, err)
	}
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(req.Action, &head)
	var action any
	switch head.Type {
	case "order":
		action = &Place{}
	case "cancel":
		action = &Cancel{}
	case "cancelByCloid":
		action = &CancelByCloid{}
	case "modify":
		action = &Modify{}
	case "batchModify":
		action = &BatchModify{}
	default:
		t.Errorf("unexpected action type %q", head.Type)
		return `{"status":"err","response":"bad"}`
	}
	if err := json.Unmarshal(req.Action, action); err != nil {
		t.Errorf("decode action: %v", err)
	}
	// The action must round-trip byte-identically: it is signed in this key order.
	if again, _ := json.Marshal(action); !bytes.Equal(again, req.Action) {
		t.Errorf("action JSON not in canonical struct order:\n got %s\nwant %s", req.Action, again)
	}
	digest, err := client.signingDigest(action, req.Nonce)
	if err != nil {
		t.Errorf("digest: %v", err)
	}
	raw, err := signatureBytes(req.Signature)
	if err != nil {
		t.Errorf("signature: %v", err)
	} else if pub, err := crypto.SigToPub(digest, raw); err != nil || strings.ToLower(crypto.PubkeyToAddress(*pub).Hex()) != client.SignerAddress() {
		t.Errorf("signature does not recover the signer with the vault tail")
	}
	switch a := action.(type) {
	case *Place:
		statuses := make([]string, len(a.Orders))
		for i := range a.Orders {
			statuses[i] = fmt.Sprintf(`{"resting":{"oid":%d}}`, 4242+i)
		}
		return `{"status":"ok","response":{"type":"order","data":{"statuses":[` + strings.Join(statuses, ",") + `]}}}`
	case *CancelByCloid:
		f.mu.Lock()
		for _, c := range a.Cancels {
			f.canceled[c.Cloid.String()] = true
		}
		f.mu.Unlock()
		return `{"status":"ok","response":{"type":"cancel","data":{"statuses":["success"]}}}`
	case *Cancel:
		return `{"status":"ok","response":{"type":"cancel","data":{"statuses":["success"]}}}`
	}
	return `{"status":"ok","response":{"type":"default"}}` // modify / batchModify acceptance
}

func topLevelKeys(body []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func TestVaultActionsOverOneBinWSSession(t *testing.T) {
	client, err := NewForVault("http://placeholder", strings.Repeat("0", 63)+"1", false, testVault)
	if err != nil {
		t.Fatal(err)
	}
	client.SignChainID = 12589
	client.StateDir = t.TempDir()
	fake := newFakeExchange(t, client)
	client.URL = fake.info.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := binws.Dial(ctx, binws.Config{URL: fake.ws.URL(), LoginHost: "pre-api.riverbit.io", Sign: client.PersonalSign})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	client.WS = session

	var cloid Cloid
	if err := json.Unmarshal([]byte(`"0x1234567890abcdef1234567890abcdef"`), &cloid); err != nil {
		t.Fatal(err)
	}
	order := Order{Asset: 0, Buy: true, Price: "100.0000", Size: "0.10", Type: OrderType{Limit: Limit{TIF: "Alo"}}}
	actions := []any{
		&Place{Type: "order", Orders: []Order{order, {Asset: 0, Price: "110.0000", Size: "0.10", Type: OrderType{Limit: Limit{TIF: "Alo"}}}}, Grouping: "na"},
		&Cancel{Type: "cancel", Cancels: []CancelItem{{Asset: 0, OID: 4242}}},
		&CancelByCloid{Type: "cancelByCloid", Cancels: []CancelByCloidItem{{Asset: 0, Cloid: cloid}}},
		&Modify{Type: "modify", OID: 4243, Order: order},
		&BatchModify{Type: "batchModify", Modifies: []ModifyItem{{OID: 4244, Order: order}, {OID: 4245, Order: order}}},
	}
	for _, action := range actions {
		if err := client.Exchange(ctx, action); err != nil {
			t.Fatalf("%T over bin-ws: %v", action, err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != len(actions) {
		t.Fatalf("server saw %d actions, want %d", len(fake.requests), len(actions))
	}
	if logins := fake.ws.Logins(); len(logins) != 1 {
		t.Fatalf("one long-lived connection expected, saw %d logins", len(logins))
	}
}

func TestBinWSErrorReplyIsNotARejection(t *testing.T) {
	client, err := New("http://placeholder", strings.Repeat("0", 63)+"1", false)
	if err != nil {
		t.Fatal(err)
	}
	server := binwstest.New(t, "h")
	server.Handle = func(c *binwstest.Conn, f binws.ClientFrame) {
		_ = c.SendJSON(f.ID, fmt.Sprintf(`{"channel":"post","data":{"id":%d,"response":{"type":"error","payload":"429 Too Many Requests"}}}`, f.ID))
	}
	session, err := binws.Dial(context.Background(), binws.Config{URL: server.URL(), LoginHost: "h", Sign: client.PersonalSign})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	client.WS = session
	err = client.Exchange(context.Background(), &Cancel{Type: "cancel", Cancels: []CancelItem{{Asset: 0, OID: 1}}})
	var rej *ActionRejectedError
	if err == nil || !strings.Contains(err.Error(), "429 Too Many Requests") || errorsAs(err, &rej) {
		t.Fatalf("error = %v; want a transport error, not a final rejection", err)
	}
}

var testNow = time.UnixMilli(1760000000000)

func errorsAs(err error, target **ActionRejectedError) bool { return errors.As(err, target) }
