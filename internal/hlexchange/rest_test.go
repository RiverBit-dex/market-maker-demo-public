package hlexchange

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riverbit-dex/market-maker-demo-public/internal/binws"
	"github.com/riverbit-dex/market-maker-demo-public/internal/binws/binwstest"
)

// fakeREST is an HTTP gateway with POST /exchange and POST /info (orderStatus).
type fakeREST struct {
	*httptest.Server
	mu        sync.Mutex
	exchanges [][]byte // raw /exchange bodies, in order
	cloids    []string // orderStatus oids that were cloids
	// exchange answers one /exchange body; it may block or fail.
	exchange func(w http.ResponseWriter, r *http.Request, body []byte)
	// pendingLookups makes the first N orderStatus answers unknownOid.
	pendingLookups int
}

func newFakeREST(t *testing.T, account string) *fakeREST {
	f := &fakeREST{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/exchange":
			f.mu.Lock()
			f.exchanges = append(f.exchanges, body)
			f.mu.Unlock()
			f.exchange(w, r, body)
		case "/info":
			var q struct {
				Type string `json:"type"`
				User string `json:"user"`
				OID  any    `json:"oid"`
			}
			if json.Unmarshal(body, &q) != nil || q.Type != "orderStatus" {
				t.Errorf("unexpected /info %s", body)
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			if q.User != account {
				t.Errorf("orderStatus user = %s, want %s", q.User, account)
			}
			f.mu.Lock()
			if s, ok := q.OID.(string); ok {
				f.cloids = append(f.cloids, s)
			}
			unknown := f.pendingLookups > 0
			if unknown {
				f.pendingLookups--
			}
			f.mu.Unlock()
			if unknown {
				fmt.Fprint(w, `{"height":8,"status":"unknownOid"}`)
				return
			}
			fmt.Fprint(w, `{"height":9,"status":"order","order":{"status":"open","order":{"coin":"RBT","oid":4242}}}`)
		default:
			t.Errorf("unexpected HTTP path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeREST) exchangeBodies() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.exchanges...)
}

func restVaultClient(t *testing.T) *Client {
	t.Helper()
	client, err := NewForVault("http://placeholder", strings.Repeat("0", 63)+"1", false, testVault)
	if err != nil {
		t.Fatal(err)
	}
	client.SignChainID = 12589
	client.StateDir = t.TempDir()
	return client
}

func alo(price string) Order {
	return Order{Asset: 0, Buy: true, Price: price, Size: "0.10", Type: OrderType{Limit: Limit{TIF: "Alo"}}}
}

// REST mode: the signed request is POSTed to /exchange (no bin-ws at all) and
// the placed order is confirmed by orderStatus over HTTP /info.
func TestPlaceOverRESTReconcilesWithOrderStatus(t *testing.T) {
	client := restVaultClient(t)
	verifier := &fakeExchange{t: t, canceled: map[string]bool{}}
	rest := newFakeREST(t, testVault)
	rest.exchange = func(w http.ResponseWriter, _ *http.Request, body []byte) {
		fmt.Fprint(w, verifier.verify(client, body)) // key order + vault-bound signature
	}
	client.URL = rest.URL

	if err := client.Exchange(context.Background(), &Place{Type: "order", Orders: []Order{alo("100.0000")}, Grouping: "na"}); err != nil {
		t.Fatal(err)
	}
	bodies := rest.exchangeBodies()
	if len(bodies) != 1 {
		t.Fatalf("/exchange received %d requests, want 1", len(bodies))
	}
	var sent struct {
		Action Place `json:"action"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil || len(sent.Action.Orders) != 1 || sent.Action.Orders[0].Cloid == "" {
		t.Fatalf("sent %s (%v)", bodies[0], err)
	}
	rest.mu.Lock()
	cloids := append([]string(nil), rest.cloids...)
	rest.mu.Unlock()
	if len(cloids) == 0 || cloids[0] != sent.Action.Orders[0].Cloid {
		t.Fatalf("orderStatus looked up %v, want the submitted cloid %s", cloids, sent.Action.Orders[0].Cloid)
	}
	if len(client.LastOrderIDs) != 1 || client.LastOrderIDs[0] != 4242 || client.LastExecutionHeight != 9 {
		t.Fatalf("resolved ids=%v height=%d", client.LastOrderIDs, client.LastExecutionHeight)
	}
	if _, err := os.Stat(client.pendingPath()); !os.IsNotExist(err) {
		t.Fatalf("pending journal still present after confirmation: %v", err)
	}
	if client.transportIdentity() != "http" {
		t.Fatalf("transport identity = %q", client.transportIdentity())
	}
}

// A non-200 answer or a timeout from /exchange is an unknown outcome: the
// intent stays journaled, orderStatus is polled until it shows the order, and
// the request is never sent a second time.
func TestRESTUnknownOutcomeReconcilesWithoutResend(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request, body []byte){
		"HTTP 502": func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
		"timeout": func(w http.ResponseWriter, r *http.Request, _ []byte) {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		},
	}
	for name, exchange := range cases {
		t.Run(name, func(t *testing.T) {
			client := restVaultClient(t)
			client.HTTP = &http.Client{Timeout: 100 * time.Millisecond}
			rest := newFakeREST(t, testVault)
			rest.exchange = exchange
			rest.pendingLookups = 2 // not visible yet on the first polls
			client.URL = rest.URL

			if err := client.Exchange(context.Background(), &Place{Type: "order", Orders: []Order{alo("100.0000")}, Grouping: "na"}); err != nil {
				t.Fatalf("unknown outcome must resolve through orderStatus: %v", err)
			}
			if n := len(rest.exchangeBodies()); n != 1 {
				t.Fatalf("/exchange received %d requests, want exactly 1 (no resend)", n)
			}
			if len(client.LastOrderIDs) != 1 || client.LastOrderIDs[0] != 4242 {
				t.Fatalf("resolved ids=%v", client.LastOrderIDs)
			}
			audit, err := os.ReadFile(filepath.Join(client.StateDir, client.journalIdentity()+".audit.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(audit, []byte(`"event":"sent_or_uncertain"`)) || !bytes.Contains(audit, []byte("transport failed or ambiguous: REST exchange")) {
				t.Fatalf("audit does not record an uncertain REST send:\n%s", audit)
			}
			if !bytes.Contains(audit, []byte(`"event":"execution_observed"`)) {
				t.Fatalf("audit does not record the reconciliation:\n%s", audit)
			}
		})
	}
}

// The REST body and the bin-ws action_json are the same bytes for the same
// signed request.
func TestRESTAndBinWSSendIdenticalBytes(t *testing.T) {
	client := restVaultClient(t)
	client.StateDir = "" // This test exercises wire serialization, not durable recovery.
	action := &Cancel{Type: "cancel", Cancels: []CancelItem{{Asset: 0, OID: 7}}}
	const nonce = 1760000000123
	sig, err := client.Sign(action, nonce)
	if err != nil {
		t.Fatal(err)
	}
	const okCancel = `{"status":"ok","response":{"type":"cancel","data":{"statuses":["success"]}}}`

	rest := newFakeREST(t, testVault)
	rest.exchange = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, okCancel) }
	client.URL = rest.URL
	if err := client.submit(context.Background(), action, nonce, sig); err != nil {
		t.Fatal(err)
	}

	var wsMu sync.Mutex
	var wsBody []byte
	server := binwstest.New(t, "h")
	server.Handle = func(c *binwstest.Conn, f binws.ClientFrame) {
		wsMu.Lock()
		wsBody = append([]byte(nil), f.Data...)
		wsMu.Unlock()
		_ = c.SendJSON(f.ID, fmt.Sprintf(`{"channel":"post","data":{"id":%d,"response":{"type":"action","payload":%s}}}`, f.ID, okCancel))
	}
	session, err := binws.Dial(context.Background(), binws.Config{URL: server.URL(), LoginHost: "h", Sign: client.PersonalSign})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	client.WS = session
	if err := client.submit(context.Background(), action, nonce, sig); err != nil {
		t.Fatal(err)
	}
	restBody := rest.exchangeBodies()[0]
	wsMu.Lock()
	defer wsMu.Unlock()
	if !bytes.Equal(restBody, wsBody) {
		t.Fatalf("REST body differs from action_json:\nREST %s\n ws  %s", restBody, wsBody)
	}
	if keys, _ := topLevelKeys(restBody); strings.Join(keys, ",") != "action,nonce,signature,vaultAddress" {
		t.Fatalf("keys = %v", keys)
	}
}

// ws mode: the action goes out as action_json and orderStatus / openOrders go
// out as info_json on the same connection, matched by id. HTTP is not used.
func TestBinWSReconciliationUsesInfoJSON(t *testing.T) {
	client := restVaultClient(t)
	verifier := &fakeExchange{t: t, canceled: map[string]bool{}}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("ws mode reconciliation must not use HTTP, got %s", r.URL.Path)
	}))
	defer gateway.Close()
	client.URL = gateway.URL

	var mu sync.Mutex
	var ids []uint64
	var infos []string
	server := binwstest.New(t, "pre-api.riverbit.io")
	server.Handle = func(c *binwstest.Conn, f binws.ClientFrame) {
		mu.Lock()
		ids = append(ids, f.ID)
		mu.Unlock()
		switch f.Body {
		case binws.ClientAction:
			payload := verifier.verify(client, f.Data)
			_ = c.SendJSON(f.ID, fmt.Sprintf(`{"channel":"post","data":{"id":%d,"response":{"type":"action","payload":%s}}}`, f.ID, payload))
		case binws.ClientInfo:
			var q struct {
				Type string `json:"type"`
				User string `json:"user"`
			}
			_ = json.Unmarshal(f.Data, &q)
			mu.Lock()
			infos = append(infos, q.Type)
			mu.Unlock()
			if q.User != testVault {
				t.Errorf("%s user = %s, want the vault", q.Type, q.User)
			}
			data := `[{"coin":"RBT","oid":4242,"side":"B","limitPx":"100.0","sz":"0.1","cloid":""}]`
			if q.Type == "orderStatus" {
				data = `{"height":11,"status":"order","order":{"status":"open","order":{"coin":"RBT","oid":4242}}}`
			}
			// An unrelated push first: replies are matched by id, not by order.
			_ = c.SendJSON(0, `{"channel":"orderUpdates","data":[]}`)
			_ = c.SendJSON(f.ID, fmt.Sprintf(`{"channel":"post","data":{"id":%d,"response":{"type":"info","payload":{"type":%q,"data":%s}}}}`, f.ID, q.Type, data))
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := binws.Dial(ctx, binws.Config{URL: server.URL(), LoginHost: "pre-api.riverbit.io", Sign: client.PersonalSign})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	client.WS, client.Queries = session, session

	if err := client.Exchange(ctx, &Place{Type: "order", Orders: []Order{alo("100.0000")}, Grouping: "na"}); err != nil {
		t.Fatal(err)
	}
	if len(client.LastOrderIDs) != 1 || client.LastOrderIDs[0] != 4242 || client.LastExecutionHeight != 11 {
		t.Fatalf("resolved ids=%v height=%d", client.LastOrderIDs, client.LastExecutionHeight)
	}
	orders, err := client.OpenOrders(ctx)
	if err != nil || len(orders) != 1 || orders[0].OID != 4242 {
		t.Fatalf("openOrders = %+v, %v", orders, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(infos) < 2 || infos[0] != "orderStatus" || infos[len(infos)-1] != "openOrders" {
		t.Fatalf("info_json queries = %v", infos)
	}
	seen := map[uint64]bool{}
	for _, id := range ids {
		if id == 0 || seen[id] {
			t.Fatalf("post frame ids must be unique and non-zero: %v", ids)
		}
		seen[id] = true
	}
	if len(server.Logins()) != 1 {
		t.Fatalf("logins = %d, want one connection", len(server.Logins()))
	}
}

// A payload that is not {"type":<query type>,"data":...} is not accepted as an answer.
func TestBinWSInfoRejectsMismatchedPayload(t *testing.T) {
	client := restVaultClient(t)
	server := binwstest.New(t, "h")
	server.Handle = func(c *binwstest.Conn, f binws.ClientFrame) {
		_ = c.SendJSON(f.ID, fmt.Sprintf(`{"channel":"post","data":{"id":%d,"response":{"type":"info","payload":{"type":"allMids","data":[]}}}}`, f.ID))
	}
	session, err := binws.Dial(context.Background(), binws.Config{URL: server.URL(), LoginHost: "h", Sign: client.PersonalSign})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	client.Queries = session
	if _, err := client.OpenOrders(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected payload") {
		t.Fatalf("err = %v", err)
	}
}

// A cloid given in the action is submitted as-is (the caller cancels or looks
// up by it later); an order without one gets a generated cloid. Both are used
// for the orderStatus reconciliation.
func TestPlaceKeepsCallerCloidAndFillsMissingOnes(t *testing.T) {
	client := restVaultClient(t)
	verifier := &fakeExchange{t: t, canceled: map[string]bool{}}
	rest := newFakeREST(t, testVault)
	rest.exchange = func(w http.ResponseWriter, _ *http.Request, body []byte) {
		fmt.Fprint(w, verifier.verify(client, body))
	}
	client.URL = rest.URL

	const mine = "0x5753464c4f5700000000000000000a01"
	given := alo("100.0000")
	given.Cloid = mine
	if err := client.Exchange(context.Background(), &Place{Type: "order", Orders: []Order{given, alo("99.0000")}, Grouping: "na"}); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Action Place `json:"action"`
	}
	if err := json.Unmarshal(rest.exchangeBodies()[0], &sent); err != nil {
		t.Fatal(err)
	}
	if got := sent.Action.Orders[0].Cloid; got != mine {
		t.Fatalf("caller cloid rewritten: sent %s, want %s", got, mine)
	}
	generated := sent.Action.Orders[1].Cloid
	if len(generated) != 34 || generated == mine {
		t.Fatalf("missing cloid not generated: %q", generated)
	}
	rest.mu.Lock()
	lookups := strings.Join(rest.cloids, ",")
	rest.mu.Unlock()
	if !strings.Contains(lookups, mine) || !strings.Contains(lookups, generated) {
		t.Fatalf("orderStatus lookups %s must cover %s and %s", lookups, mine, generated)
	}
}
