package hlexchange

// The pending file is a write-ahead intent, not proof of submission. An
// interrupted/ambiguous send is never retried automatically, even after restart.
import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var ErrExecutionRejected = errors.New("execution rejected")

// retirePending persists the nonce floor, audits, and removes the journal.
func (c *Client) retirePending(p *pendingRequest, event, detail string) error {
	// Preserve the last durable nonce evidence before removing a legacy
	// journal which may predate the nonce high-water file.
	signerNonces.Lock()
	if p.Nonce > signerNonces.values[c.SignerAddress()] {
		signerNonces.values[c.SignerAddress()] = p.Nonce
	}
	signerNonces.Unlock()
	if _, err := c.durableNonce(); err != nil {
		return fmt.Errorf("pending nonce=%d retained: persist nonce floor: %w", p.Nonce, err)
	}
	if err := c.audit(event, p, detail); err != nil {
		return err
	}
	if err := os.Remove(c.pendingPath()); err != nil {
		return err
	}
	return syncDirectory(c.StateDir)
}

type pendingRequest struct {
	Signer    string `json:"signer,omitempty"`
	Transport string `json:"transport,omitempty"`
	Mainnet   bool   `json:"mainnet,omitempty"`
	Account   string `json:"account"`
	URL       string `json:"url"`
	// Endpoint is retained only to reject journals from the removed native transport.
	Endpoint      string                   `json:"endpoint"`
	Nonce         uint64                   `json:"nonce"`
	Action        json.RawMessage          `json:"action"`
	Lookups       []string                 `json:"cloids,omitempty"`
	CancelOIDs    []uint64                 `json:"cancel_oids,omitempty"`
	CancelCloids  []string                 `json:"cancel_cloids,omitempty"`
	LogOffset     int64                    `json:"log_offset"`
	LogInode      uint64                   `json:"log_inode"`
	BatchReceipt  json.RawMessage          `json:"batch_receipt,omitempty"`
	CancelReceipt json.RawMessage          `json:"cancel_receipt,omitempty"`
	Evidence      map[string]orderEvidence `json:"order_evidence,omitempty"`
}

// Evidence proves an order was observed, not its current live state. Keeping
// it in the original journal lets bounded recovery passes survive restarts.
type orderEvidence struct {
	Status string `json:"status"`
	OID    uint64 `json:"oid"`
	Height uint64 `json:"height"`
}

const lookupPassTimeout = 5 * time.Second

func (c *Client) journalIdentity() string {
	if c.account == "" {
		return c.SignerAddress()
	}
	return c.SignerAddress() + "-" + c.AccountAddress()
}
func (c *Client) transportIdentity() string {
	if c.WS != nil {
		return "binary-ws:" + c.WS.URL()
	}
	return "http"
}
func (c *Client) pendingPath() string {
	return filepath.Join(c.StateDir, c.journalIdentity()+".pending.json")
}
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (c *Client) loadPending() (*pendingRequest, error) {
	if c.account != "" {
		legacy := filepath.Join(c.StateDir, c.SignerAddress()+".pending.json")
		if _, err := os.Stat(legacy); err == nil {
			return nil, fmt.Errorf("master pending intent must be recovered before subaccount use")
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	b, err := os.ReadFile(c.pendingPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p pendingRequest
	if err = json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("invalid pending journal: %w", err)
	}
	if p.Account != c.AccountAddress() || p.URL != c.URL || p.Endpoint != "" || p.Nonce == 0 || !json.Valid(p.Action) {
		return nil, fmt.Errorf("pending journal identity/endpoint mismatch or invalid request")
	}
	// Legacy records lack signer/transport and are only readable for the original
	// master identity and original endpoint. They can never target a subaccount.
	if p.Signer == "" {
		if c.account != "" || p.Account != c.SignerAddress() || c.Mainnet || p.Endpoint != "" || (p.Transport != "" && p.Transport != "http") {
			return nil, fmt.Errorf("legacy pending journal cannot prove subaccount identity")
		}
	} else if p.Signer != c.SignerAddress() || p.Transport != c.transportIdentity() || p.Mainnet != c.Mainnet {
		return nil, fmt.Errorf("pending signer/transport/network mismatch")
	}
	signerNonces.Lock()
	if p.Nonce > signerNonces.values[c.SignerAddress()] {
		signerNonces.values[c.SignerAddress()] = p.Nonce
	}
	signerNonces.Unlock()
	return &p, nil
}
func (c *Client) audit(event string, p *pendingRequest, detail string) error {
	f, err := os.OpenFile(filepath.Join(c.StateDir, c.journalIdentity()+".audit.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = json.NewEncoder(f).Encode(struct {
		Time    string          `json:"time"`
		Event   string          `json:"event"`
		Request *pendingRequest `json:"request"`
		Detail  string          `json:"detail"`
	}{time.Now().UTC().Format(time.RFC3339Nano), event, p, detail}); err != nil {
		return err
	}
	return f.Sync()
}
func logIdentity(path string) (int64, uint64, error) {
	s, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := s.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("cannot identify receipt log")
	}
	return s.Size(), st.Ino, nil
}
func (c *Client) trackedExchange(ctx context.Context, action any) error {
	c.submitMu.Lock()
	defer c.submitMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return err
	}
	if p, err := c.loadPending(); err != nil {
		return err
	} else if p != nil {
		return fmt.Errorf("unresolved submission nonce=%d: recover before another action", p.Nonce)
	}
	nonce, err := c.durableNonce()
	if err != nil {
		return err
	}
	p := &pendingRequest{Signer: c.SignerAddress(), Transport: c.transportIdentity(), Mainnet: c.Mainnet, Account: c.AccountAddress(), URL: c.URL, Nonce: nonce}
	// A caller-supplied cloid is kept (the caller may cancel or look up by it later);
	// only orders without one get a random cloid so execution can still be resolved.
	assign := func(o *Order) error {
		if o.Cloid == "" {
			var b [16]byte
			if _, err := rand.Read(b[:]); err != nil {
				return err
			}
			o.Cloid = "0x" + hex.EncodeToString(b[:])
		}
		p.Lookups = append(p.Lookups, o.Cloid)
		return nil
	}
	switch a := action.(type) {
	case Place:
		a.Orders = append([]Order(nil), a.Orders...)
		for i := range a.Orders {
			if err := assign(&a.Orders[i]); err != nil {
				return err
			}
		}
		action = a
	case *Place:
		if a == nil {
			return fmt.Errorf("nil place action")
		}
		for i := range a.Orders {
			if err := assign(&a.Orders[i]); err != nil {
				return err
			}
		}
	case Modify:
		if err := assign(&a.Order); err != nil {
			return err
		}
		p.CancelOIDs = append(p.CancelOIDs, a.OID)
		action = a
	case *Modify:
		if a == nil {
			return fmt.Errorf("nil modify action")
		}
		if err := assign(&a.Order); err != nil {
			return err
		}
		p.CancelOIDs = append(p.CancelOIDs, a.OID)
	case BatchModify:
		for i := range a.Modifies {
			p.CancelOIDs = append(p.CancelOIDs, a.Modifies[i].OID)
			if err := assign(&a.Modifies[i].Order); err != nil {
				return err
			}
		}
		action = a
	case *BatchModify:
		if a == nil {
			return fmt.Errorf("nil batch modify action")
		}
		for i := range a.Modifies {
			p.CancelOIDs = append(p.CancelOIDs, a.Modifies[i].OID)
			if err := assign(&a.Modifies[i].Order); err != nil {
				return err
			}
		}
	case Cancel:
		for _, x := range a.Cancels {
			p.CancelOIDs = append(p.CancelOIDs, x.OID)
		}
	case *Cancel:
		if a == nil {
			return fmt.Errorf("nil cancel action")
		}
		for _, x := range a.Cancels {
			p.CancelOIDs = append(p.CancelOIDs, x.OID)
		}
	case CancelByCloid:
		for _, x := range a.Cancels {
			p.CancelCloids = append(p.CancelCloids, x.Cloid.String())
		}
	case *CancelByCloid:
		if a == nil {
			return fmt.Errorf("nil cancelByCloid action")
		}
		for _, x := range a.Cancels {
			p.CancelCloids = append(p.CancelCloids, x.Cloid.String())
		}
	default:
		return fmt.Errorf("unsupported tracked action %T", action)
	}
	if len(p.Lookups)+len(p.CancelOIDs)+len(p.CancelCloids) == 0 {
		return fmt.Errorf("empty tracked action")
	}
	p.Action, err = json.Marshal(action)
	if err != nil {
		return err
	}
	if c.ReceiptLog != "" {
		p.LogOffset, p.LogInode, err = logIdentity(c.ReceiptLog)
		if err != nil {
			return err
		}
	}
	f, err := os.OpenFile(c.pendingPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(p)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = syncDirectory(c.StateDir); err != nil {
		return err
	}
	if err = c.audit("prepared", p, ""); err != nil {
		return err
	}
	log.Printf("hl request prepared account=%s nonce=%d action=%s", p.Account, p.Nonce, p.Action)
	err = c.exchangeNonce(ctx, action, p.Nonce)
	var cancellation *cancelReceipt
	if errors.As(err, &cancellation) {
		p.CancelReceipt = append(json.RawMessage(nil), cancellation.Raw...)
		if e := c.savePending(p); e != nil {
			return fmt.Errorf("pending retained: save cancel receipt: %w", e)
		}
		err = nil
	}
	var batch *batchReceipt
	if errors.As(err, &batch) {
		if e := c.saveBatchReceipt(p, batch); e != nil {
			return fmt.Errorf("pending retained: save batch receipt: %w", e)
		}
		err = nil
	}
	var rej *ActionRejectedError
	if errors.As(err, &rej) {
		// The execution layer answered with a deterministic rejection: nothing
		// ran, so the intent is final. Retire it; the next attempt gets a new nonce.
		c.LastOrderIDs, c.LastStatuses = nil, nil
		if e := c.retirePending(p, "rejected_by_execution", rej.Error()); e != nil {
			return e
		}
		log.Printf("hl request account=%s nonce=%d rejected by execution code=%d; pending cleared", p.Account, p.Nonce, rej.Code)
		out := fmt.Errorf("%w nonce=%d: %s", ErrExecutionRejected, p.Nonce, rej.Error())
		if rej.Code == CodeAccountNotFound {
			out = fmt.Errorf("%w: %w", out, ErrAccountNotActivated)
		}
		return out
	}
	detail := "transport returned; execution unconfirmed"
	if err != nil {
		detail = "transport failed or ambiguous: " + err.Error()
	}
	if e := c.audit("sent_or_uncertain", p, detail); e != nil {
		return e
	}
	log.Printf("hl request account=%s nonce=%d %s", p.Account, p.Nonce, detail)
	return c.resolve(ctx, p)
}

// RecoverPending only observes the old request. It never submits or replays it.
// Call before readiness checks and before any shutdown cancellation.
func (c *Client) RecoverPending(ctx context.Context) error {
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
	if c.StateDir == "" {
		return nil
	}
	c.submitMu.Lock()
	defer c.submitMu.Unlock()
	p, err := c.loadPending()
	if err != nil || p == nil {
		return err
	}
	log.Printf("hl recovering pending account=%s nonce=%d", p.Account, p.Nonce)
	return c.resolve(ctx, p)
}

func (c *Client) lookup(ctx context.Context, oid any) (string, uint64, error) {
	return c.lookupState(ctx, oid, false)
}

// A legResult can prove an order was placed/filled, but cannot prove it is no
// longer live: a filled leg may have left a resting remainder. Cancellation
// reconciliation therefore requires the actual current order object.
func (c *Client) lookupState(ctx context.Context, oid any, requireOrder bool) (string, uint64, error) {
	var out struct {
		Height uint64          `json:"height"`
		Status string          `json:"status"`
		Leg    json.RawMessage `json:"leg"`
		Order  struct {
			Status string    `json:"status"`
			Order  OpenOrder `json:"order"`
		} `json:"order"`
	}
	err := c.orderQuery(ctx, map[string]any{"type": "orderStatus", "user": c.AccountAddress(), "oid": oid}, &out)
	if err != nil {
		return "", 0, err
	}
	if c.RequireExecutionHeight && out.Height == 0 {
		return "", 0, fmt.Errorf("orderStatus lacks committed height")
	}
	if c.RequireExecutionHeight && out.Height < c.LastExecutionHeight {
		return "", 0, fmt.Errorf("orderStatus height %d is behind confirmed height %d", out.Height, c.LastExecutionHeight)
	}
	if out.Status == "unknownOid" {
		return "", 0, nil
	}
	finish := func(status string, oid uint64) (string, uint64, error) {
		if err := c.observeHeight(out.Height); err != nil {
			return "", 0, err
		}
		return status, oid, nil
	}
	if out.Status == "legResult" {
		if requireOrder {
			return "", 0, fmt.Errorf("orderStatus lacks current order state")
		}
		var leg struct {
			Resting *struct {
				OID uint64 `json:"oid"`
			} `json:"resting"`
			Filled *struct {
				OID uint64 `json:"oid"`
			} `json:"filled"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(out.Leg, &leg); err != nil {
			return "", 0, fmt.Errorf("invalid legResult: %w", err)
		}
		if leg.Error != "" {
			return finish("rejected", 0)
		}
		if leg.Resting != nil && leg.Resting.OID != 0 {
			return finish("open", leg.Resting.OID)
		}
		if leg.Filled != nil && leg.Filled.OID != 0 {
			return finish("filled", leg.Filled.OID)
		}
		return "", 0, fmt.Errorf("nonterminal or invalid legResult")
	}
	if out.Status != "order" || !knownOrderState(out.Order.Status) || out.Order.Order.OID == 0 {
		return "", 0, fmt.Errorf("invalid orderStatus response")
	}
	if id, ok := oid.(uint64); ok && id != out.Order.Order.OID {
		return "", 0, fmt.Errorf("orderStatus returned a different oid")
	}
	return finish(out.Order.Status, out.Order.Order.OID)
}

// Read bounded chunks and preserve partial lines. Rotation/truncation cannot be
// treated as success. The original offset persists, so restart rescans safely.
func (c *Client) receipt(p *pendingRequest, offset *int64) (bool, string, error) {
	if c.ReceiptLog == "" || p.LogInode == 0 {
		return false, "", nil
	}
	size, ino, err := logIdentity(c.ReceiptLog)
	if err != nil {
		return false, "", err
	}
	if ino != p.LogInode || size < *offset {
		return false, "", fmt.Errorf("receipt log rotated/truncated with pending nonce=%d", p.Nonce)
	}
	f, err := os.Open(c.ReceiptLog)
	if err != nil {
		return false, "", err
	}
	defer f.Close()
	if _, err = f.Seek(*offset, io.SeekStart); err != nil {
		return false, "", err
	}
	b, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil {
		return false, "", err
	}
	end := strings.LastIndexByte(string(b), '\n')
	if end < 0 {
		return false, "", nil
	}
	*offset += int64(end + 1)
	needle := fmt.Sprintf("[record_nonces: Some([%d])]", p.Nonce)
	for _, line := range strings.Split(string(b[:end]), "\n") {
		if strings.Contains(line, "@@ action outcome @@") && strings.Contains(line, needle) {
			if strings.Contains(line, "[codes: [None]]") {
				return true, "execution observed; inspect per-order status", nil
			}
			return true, "execution rejected: " + line, nil
		}
	}
	return false, "", nil
}
func (c *Client) resolve(ctx context.Context, p *pendingRequest) error {
	c.LastOrderIDs, c.LastStatuses = nil, nil
	var cancelErrors []string
	if len(p.CancelReceipt) > 0 {
		count, err := pendingCancelCount(p)
		if err != nil {
			return err
		}
		cancelErrors, err = parseCancelReceipt(p.CancelReceipt, count)
		if err != nil {
			return err
		}
	}
	var receiptLegs []batchLeg
	if len(p.BatchReceipt) > 0 {
		var batch BatchModify
		if json.Unmarshal(p.Action, &batch) != nil || batch.Type != "batchModify" || len(batch.Modifies) != len(p.Lookups) || len(p.CancelOIDs) != len(p.Lookups) {
			return fmt.Errorf("pending retained: batch receipt/action mismatch")
		}
		for i, m := range batch.Modifies {
			if m.Order.Cloid != p.Lookups[i] || m.OID != p.CancelOIDs[i] {
				return fmt.Errorf("pending retained: batch receipt/lookup mismatch")
			}
		}
		var err error
		receiptLegs, err = parseBatchReceipt(p.BatchReceipt, len(p.Lookups))
		if err != nil {
			return fmt.Errorf("pending retained: %w", err)
		}
	}
	// Only evidence for this journal's queries can participate in recovery.
	allowed := make(map[string]bool)
	for _, cloid := range p.Lookups {
		allowed["order:"+cloid] = true
	}
	for _, oid := range p.CancelOIDs {
		allowed["cancel:"+strconv.FormatUint(oid, 10)] = true
	}
	for _, cloid := range p.CancelCloids {
		allowed["cancel-cloid:"+cloid] = true
	}
	for key, evidence := range p.Evidence {
		if !allowed[key] || !knownOrderState(evidence.Status) || evidence.Height == 0 || (evidence.OID == 0 && evidence.Status != "rejected") || (strings.HasPrefix(key, "cancel") && !inactiveOrder(evidence.Status)) {
			return fmt.Errorf("pending retained: invalid stored order evidence")
		}
		if err := c.observeHeight(evidence.Height); err != nil {
			return err
		}
	}
	if p.Evidence == nil {
		p.Evidence = make(map[string]orderEvidence)
	}
	offset := p.LogOffset
	nextLog := time.Time{}
	receiptHintSeen := false
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("pending nonce=%d retained: %w", p.Nonce, err)
		}
		done := len(p.Lookups)+len(p.CancelOIDs)+len(p.CancelCloids) > 0
		// An old filled/canceled target does not establish that this cancel
		// request ended. A late cancelByCloid could affect a reused cloid.
		if len(p.Lookups) == 0 && len(cancelErrors) == 0 {
			done = false
		}
		var statuses []string
		var ids []uint64
		var states []string
		rejected := false
		lookupFailed := false
		lookupCtx, stopLookups := context.WithTimeout(ctx, lookupPassTimeout)
		dirty := false
		remember := func(key, status string, oid uint64) {
			// Only height-checked reads may become durable evidence for a
			// later recovery run.
			if c.RequireExecutionHeight {
				p.Evidence[key] = orderEvidence{Status: status, OID: oid, Height: c.LastExecutionHeight}
				dirty = true
			}
		}
		for i, cloid := range p.Lookups {
			if len(receiptLegs) > 0 && receiptLegs[i].Error != "" {
				statuses = append(statuses, fmt.Sprintf("cloid=%s rejected: %s", cloid, receiptLegs[i].Error))
				rejected = true
				continue
			}
			var key any = cloid
			if len(receiptLegs) > 0 {
				key = receiptLegs[i].OID
			}
			evidenceKey := "order:" + cloid
			evidence, known := p.Evidence[evidenceKey]
			var err error
			if !known {
				evidence.Status, evidence.OID, err = c.lookup(lookupCtx, key)
			}
			s, oid := evidence.Status, evidence.OID
			// A reused cloid can still name the old, canceled order while the
			// replacement projection lags. That is not the new order's result.
			if len(p.CancelOIDs) == len(p.Lookups) && oid == p.CancelOIDs[i] {
				s = ""
			}
			if len(receiptLegs) > 0 && oid != receiptLegs[i].OID {
				s = ""
			}
			if err != nil {
				lookupFailed = true
			}
			if err != nil || s == "" {
				done = false
			} else if !known {
				remember(evidenceKey, s, oid)
			}
			statuses = append(statuses, fmt.Sprintf("cloid=%s oid=%d status=%s", cloid, oid, s))
			if oid != 0 && s != "" {
				ids = append(ids, oid)
				states = append(states, s)
			}
			if strings.Contains(strings.ToLower(s), "reject") {
				rejected = true
			}
			if lookupCtx.Err() != nil {
				done, lookupFailed = false, true
				break
			}
		}
		for i, oid := range p.CancelOIDs {
			if len(cancelErrors) > 0 && cancelErrors[i] != "" {
				statuses = append(statuses, fmt.Sprintf("cancel oid=%d rejected: %s", oid, cancelErrors[i]))
				rejected = true
				continue
			}
			// A rejected modify leaves its old order untouched. Do not wait
			// for that order to be canceled by a failed replacement.
			if len(receiptLegs) > 0 && receiptLegs[i].Error != "" {
				continue
			}
			evidenceKey := "cancel:" + strconv.FormatUint(oid, 10)
			evidence, known := p.Evidence[evidenceKey]
			var err error
			if !known {
				evidence.Status, evidence.OID, err = c.lookupState(lookupCtx, oid, true)
			}
			s := evidence.Status
			if err != nil {
				lookupFailed = true
			}
			if err != nil || !inactiveOrder(s) {
				done = false
			} else if !known {
				remember(evidenceKey, s, evidence.OID)
			}
			statuses = append(statuses, fmt.Sprintf("oid=%d status=%s", oid, s))
			if lookupCtx.Err() != nil {
				done, lookupFailed = false, true
				break
			}
		}
		for i, cloid := range p.CancelCloids {
			if len(cancelErrors) > 0 && cancelErrors[i] != "" {
				statuses = append(statuses, fmt.Sprintf("cancel cloid=%s rejected: %s", cloid, cancelErrors[i]))
				rejected = true
				continue
			}
			evidenceKey := "cancel-cloid:" + cloid
			evidence, known := p.Evidence[evidenceKey]
			var err error
			if !known {
				evidence.Status, evidence.OID, err = c.lookupState(lookupCtx, cloid, true)
			}
			s := evidence.Status
			if err != nil {
				lookupFailed = true
			}
			if err != nil || !inactiveOrder(s) {
				done = false
			} else if !known {
				remember(evidenceKey, s, evidence.OID)
			}
			statuses = append(statuses, fmt.Sprintf("cancel_cloid=%s status=%s", cloid, s))
			if lookupCtx.Err() != nil {
				done, lookupFailed = false, true
				break
			}
		}
		stopLookups()
		if dirty {
			if err := c.savePending(p); err != nil {
				return fmt.Errorf("pending retained: save order evidence: %w", err)
			}
		}
		detail := strings.Join(statuses, "; ")
		if !done && !receiptHintSeen {
			observed, receipt, err := c.receipt(p, &offset)
			if err != nil {
				return err
			}
			if observed {
				// Nonces are per account. The node log does not include the
				// signer on this line, so a nonce match is diagnostic only.
				receiptHintSeen = true
				if err := c.audit("nonce_log_hint", p, receipt); err != nil {
					return err
				}
				log.Printf("hl nonce log hint account=%s nonce=%d; still awaiting account/order confirmation: %s", p.Account, p.Nonce, receipt)
			}
		}
		if done {
			if err := c.retirePending(p, "execution_observed", detail); err != nil {
				return err
			}
			log.Printf("hl execution observed account=%s nonce=%d %s", p.Account, p.Nonce, detail)
			c.LastOrderIDs, c.LastStatuses = ids, states
			if rejected {
				return fmt.Errorf("%w nonce=%d: %s", ErrExecutionRejected, p.Nonce, detail)
			}
			return nil
		}
		// Elapsed time and unknownOid never prove rejection. Retain the
		// journal until positive evidence resolves it or the caller cancels.
		if time.Now().After(nextLog) {
			log.Printf("hl awaiting execution account=%s nonce=%d lookup_failed=%t (no resubmission)", p.Account, p.Nonce, lookupFailed)
			nextLog = time.Now().Add(30 * time.Second)
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func inactiveOrder(status string) bool {
	switch status {
	case "filled", "canceled", "rejected", "tickRejected", "minTradeNtlRejected",
		"reduceOnlyRejected", "badAloPxRejected", "iocCancelRejected", "perpMarginRejected",
		"oracleRejected", "insufficientSpotBalanceRejected", "selfTradeCanceled",
		"reduceOnlyCanceled", "siblingFilledCanceled", "scheduledCancel", "vaultWithdrawalCanceled":
		return true
	}
	return false
}

func knownOrderState(status string) bool {
	return status == "open" || status == "triggered" || inactiveOrder(status)
}

// The canonical StateDir must be shared by every process using this signer.
// Nonblocking ownership prevents a second process from sending while recovery
// is unresolved. The runner is responsible for canonical directory selection.
func (c *Client) lockDurableSigner() (func(), error) {
	if c.StateDir == "" {
		if c.account != "" {
			return nil, fmt.Errorf("subaccount requires canonical durable StateDir")
		}
		return func() {}, nil
	}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(c.StateDir, c.SignerAddress()+".signer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("signer is owned by another process: %w", err)
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
func (c *Client) durableNonce() (uint64, error) {
	if c.StateDir == "" {
		return c.nextNonce(), nil
	}
	path := filepath.Join(c.StateDir, c.SignerAddress()+".nonce")
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if len(b) > 0 {
		floor, e := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if e != nil {
			return 0, fmt.Errorf("invalid durable nonce floor: %w", e)
		}
		if floor == ^uint64(0) {
			return 0, fmt.Errorf("nonce exhausted")
		}
		signerNonces.Lock()
		if floor > signerNonces.values[c.SignerAddress()] {
			signerNonces.values[c.SignerAddress()] = floor
		}
		signerNonces.Unlock()
	}
	n := c.nextNonce()
	f, err := os.CreateTemp(c.StateDir, ".nonce-")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.WriteString(strconv.FormatUint(n, 10)); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return 0, err
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if err = os.Rename(tmp, path); err != nil {
		return 0, err
	}
	if err = syncDirectory(c.StateDir); err != nil {
		return 0, err
	}
	return n, nil
}
