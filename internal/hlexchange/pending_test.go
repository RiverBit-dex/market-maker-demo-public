package hlexchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/riverbit-dex/market-maker-demo-public/internal/binws"
	"github.com/riverbit-dex/market-maker-demo-public/internal/binws/binwstest"
)

// The second replacement is rejected and its old order is still open. The
// successful replacement is only visible after restart. Neither the rejected
// cloid nor cancellation of its old order can ever appear in /info.
func TestMixedBatchReceiptSurvivesRestart(t *testing.T) {
	for _, transport := range []string{"rest", "ws"} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/committed=%t", transport, committed), func(t *testing.T) {
				c := restVaultClient(t)
				c.RequireExecutionHeight = true
				var sends atomic.Int32
				var visible atomic.Bool
				const receipt = `{"type":"order","data":{"statuses":[{"resting":{"oid":101}},{"error":"Order was never placed, already canceled, or filled."}]}}`
				exchange := func() string {
					sends.Add(1)
					if committed {
						return `{"status":"err","response":"query the order status","committedResult":` + receipt + `}`
					}
					return `{"status":"ok","response":` + receipt + `}`
				}
				query := func(body []byte) string {
					var q struct {
						OID uint64 `json:"oid"`
					}
					if err := json.Unmarshal(body, &q); err != nil {
						t.Errorf("receipt must reconcile the new OID, not a reusable cloid: %s", body)
					}
					if q.OID != 101 && q.OID != 11 {
						t.Errorf("must not wait for the rejected leg: %s", body)
					}
					if !visible.Load() {
						return `{"height":9,"status":"unknownOid"}`
					}
					status := "open"
					if q.OID == 11 {
						status = "canceled"
					}
					return fmt.Sprintf(`{"height":10,"status":"order","order":{"status":%q,"order":{"oid":%d}}}`, status, q.OID)
				}
				httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/exchange" {
						fmt.Fprint(w, exchange())
						return
					}
					var body json.RawMessage
					_ = json.NewDecoder(r.Body).Decode(&body)
					fmt.Fprint(w, query(body))
				}))
				defer httpServer.Close()
				c.URL = httpServer.URL
				if transport == "ws" {
					server := binwstest.New(t, "h")
					server.Handle = func(conn *binwstest.Conn, f binws.ClientFrame) {
						typ, payload := "info", ""
						if f.Body == binws.ClientAction {
							typ, payload = "action", exchange()
						} else {
							payload = `{"type":"orderStatus","data":` + query(f.Data) + `}`
						}
						_ = conn.SendJSON(f.ID, fmt.Sprintf(`{"channel":"post","data":{"id":%d,"response":{"type":%q,"payload":%s}}}`, f.ID, typ, payload))
					}
					session, err := binws.Dial(context.Background(), binws.Config{URL: server.URL(), LoginHost: "h", Sign: c.PersonalSign})
					if err != nil {
						t.Fatal(err)
					}
					defer session.Close()
					c.WS, c.Queries = session, session
				}
				batch := BatchModify{Type: "batchModify", Modifies: []ModifyItem{{OID: 11, Order: alo("100")}, {OID: 12, Order: alo("99")}}}
				var action any = batch
				if committed { // Exercise both exported action forms.
					action = &batch
				}
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				err := c.Exchange(ctx, action)
				cancel()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("first reconciliation = %v", err)
				}
				pending, err := c.loadPending()
				if err != nil || pending == nil || string(pending.BatchReceipt) != receipt {
					t.Fatalf("durable receipt = %+v, %v", pending, err)
				}
				fresh := restVaultClient(t)
				fresh.URL, fresh.StateDir, fresh.WS, fresh.Queries = c.URL, c.StateDir, c.WS, c.Queries
				fresh.RequireExecutionHeight = true
				visible.Store(true)
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				err = fresh.RecoverPending(ctx)
				cancel()
				if !errors.Is(err, ErrExecutionRejected) || !strings.Contains(err.Error(), "Order was never placed") {
					t.Fatalf("partial batch must report rejection: %v", err)
				}
				if sends.Load() != 1 || len(fresh.LastOrderIDs) != 1 || fresh.LastOrderIDs[0] != 101 || fresh.LastExecutionHeight != 10 {
					t.Fatalf("sends=%d ids=%v height=%d", sends.Load(), fresh.LastOrderIDs, fresh.LastExecutionHeight)
				}
				if _, err := os.Stat(c.pendingPath()); !os.IsNotExist(err) {
					t.Fatalf("resolved pending remains: %v", err)
				}
			})
		}
	}
}

// Like offchain #107, positive evidence must not be discarded when another
// leg is missing. The CLI has bounded runs, so retain it across restart too.
func TestRecoveryKeepsPositiveEvidenceAcrossRestart(t *testing.T) {
	c := restVaultClient(t)
	c.RequireExecutionHeight = true
	var sends, firstReads atomic.Int32
	var visible atomic.Bool
	first, second := "0x"+strings.Repeat("1", 32), "0x"+strings.Repeat("2", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/exchange" {
			sends.Add(1)
			http.Error(w, "response lost", http.StatusBadGateway)
			return
		}
		var q struct {
			OID string `json:"oid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		if q.OID == first {
			if firstReads.Add(1) != 1 {
				t.Error("already-proved leg queried again after restart")
				fmt.Fprint(w, `{"height":11,"status":"unknownOid"}`)
				return
			}
			fmt.Fprint(w, `{"height":10,"status":"order","order":{"status":"filled","order":{"oid":101}}}`)
			return
		}
		if q.OID != second {
			t.Errorf("unexpected query %s", q.OID)
		}
		if !visible.Load() {
			fmt.Fprint(w, `{"height":11,"status":"unknownOid"}`)
			return
		}
		fmt.Fprint(w, `{"height":11,"status":"order","order":{"status":"filled","order":{"oid":102}}}`)
	}))
	defer server.Close()
	c.URL = server.URL
	a, b := alo("100"), alo("99")
	a.Cloid, b.Cloid = first, second
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	err := c.Exchange(ctx, Place{Type: "order", Orders: []Order{a, b}, Grouping: "na"})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	p, err := c.loadPending()
	if err != nil || p == nil || p.Evidence["order:"+first].OID != 101 || len(p.Evidence) != 1 {
		t.Fatalf("durable positive evidence missing: %+v, %v", p, err)
	}
	visible.Store(true)
	fresh := restVaultClient(t)
	fresh.URL, fresh.StateDir, fresh.RequireExecutionHeight = c.URL, c.StateDir, true
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := fresh.RecoverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 1 || firstReads.Load() != 1 || len(fresh.LastOrderIDs) != 2 || fresh.LastExecutionHeight != 11 {
		t.Fatalf("sends=%d first reads=%d ids=%v height=%d", sends.Load(), firstReads.Load(), fresh.LastOrderIDs, fresh.LastExecutionHeight)
	}
}

func TestCancelRequiresKnownCurrentState(t *testing.T) {
	for _, response := range []string{
		`{"height":10,"status":"legResult","leg":{"filled":{"oid":11,"totalSz":"1"}}}`,
		`{"height":10,"status":"order","order":{"status":"newUnknownState","order":{"oid":11}}}`,
		`{"height":10,"status":"order","order":{"status":"canceled","order":{"oid":12}}}`,
	} {
		t.Run(response, func(t *testing.T) {
			c := restVaultClient(t)
			c.RequireExecutionHeight = true
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/exchange" {
					fmt.Fprint(w, `{"status":"ok","response":{"type":"cancel","data":{"statuses":["success"]}}}`)
					return
				}
				fmt.Fprint(w, response)
			}))
			defer server.Close()
			c.URL = server.URL
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := c.Exchange(ctx, Cancel{Type: "cancel", Cancels: []CancelItem{{OID: 11}}})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("false cancellation confirmation: %v", err)
			}
			p, err := c.loadPending()
			if err != nil || p == nil || len(p.Evidence) != 0 {
				t.Fatalf("invalid evidence persisted: %+v %v", p, err)
			}
		})
	}
}

func TestInactiveTargetDoesNotProveLostCancelCompleted(t *testing.T) {
	for _, byCloid := range []bool{false, true} {
		for _, pendingReply := range []bool{false, true} {
			t.Run(fmt.Sprintf("cloid=%t/pendingReply=%t", byCloid, pendingReply), func(t *testing.T) {
				c := restVaultClient(t)
				c.RequireExecutionHeight = true
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/exchange" {
						if pendingReply {
							fmt.Fprint(w, `{"status":"err","response":"action already pending; it was not submitted again"}`)
						} else {
							http.Error(w, "lost reply", http.StatusBadGateway)
						}
						return
					}
					fmt.Fprint(w, `{"height":10,"status":"order","order":{"status":"filled","order":{"oid":11}}}`)
				}))
				defer server.Close()
				c.URL = server.URL
				var action any = Cancel{Type: "cancel", Cancels: []CancelItem{{OID: 11}}}
				if byCloid {
					var cloid Cloid
					if err := json.Unmarshal([]byte(`"0x11111111111111111111111111111111"`), &cloid); err != nil {
						t.Fatal(err)
					}
					action = CancelByCloid{Type: "cancelByCloid", Cancels: []CancelByCloidItem{{Cloid: cloid}}}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				err := c.Exchange(ctx, action)
				if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrExecutionRejected) {
					t.Fatalf("lost cancel outcome became terminal: %v", err)
				}
				if p, err := c.loadPending(); err != nil || p == nil {
					t.Fatalf("lost cancel journal: %v", err)
				}
			})
		}
	}
}

func TestCancelReceiptSurvivesRestartAndKeepsPartialFailure(t *testing.T) {
	c := restVaultClient(t)
	c.RequireExecutionHeight = true
	var visible atomic.Bool
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/exchange" {
			sends.Add(1)
			fmt.Fprint(w, `{"status":"ok","response":{"type":"cancel","data":{"statuses":["success",{"error":"Order was never placed, already canceled, or filled."}]}}}`)
			return
		}
		var q struct {
			OID uint64 `json:"oid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		if q.OID != 11 {
			t.Errorf("must not wait for rejected cancel target: %d", q.OID)
		}
		if !visible.Load() {
			fmt.Fprint(w, `{"height":10,"status":"unknownOid"}`)
			return
		}
		fmt.Fprint(w, `{"height":10,"status":"order","order":{"status":"canceled","order":{"oid":11}}}`)
	}))
	defer server.Close()
	c.URL = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := c.Exchange(ctx, &Cancel{Type: "cancel", Cancels: []CancelItem{{OID: 11}, {OID: 12}}})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	p, err := c.loadPending()
	if err != nil || p == nil || len(p.CancelReceipt) == 0 {
		t.Fatalf("missing cancel receipt: %+v %v", p, err)
	}
	visible.Store(true)
	fresh := restVaultClient(t)
	fresh.URL, fresh.StateDir, fresh.RequireExecutionHeight = c.URL, c.StateDir, true
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = fresh.RecoverPending(ctx)
	if !errors.Is(err, ErrExecutionRejected) || sends.Load() != 1 {
		t.Fatalf("recovery=%v sends=%d", err, sends.Load())
	}
	if _, err := os.Stat(c.pendingPath()); !os.IsNotExist(err) {
		t.Fatalf("resolved pending remains: %v", err)
	}
}

func TestBoundedLookupPassKeepsProgress(t *testing.T) {
	c := restVaultClient(t)
	c.RequireExecutionHeight = true
	var mu sync.Mutex
	reads := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/exchange" {
			http.Error(w, "reply unavailable", http.StatusBadGateway)
			return
		}
		var q struct {
			OID string `json:"oid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		mu.Lock()
		reads[q.OID]++
		mu.Unlock()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
		}
		fmt.Fprint(w, `{"height":10,"status":"order","order":{"status":"filled","order":{"oid":101}}}`)
	}))
	defer server.Close()
	c.URL = server.URL
	orders := []Order{alo("100"), alo("99"), alo("98")}
	for i := range orders {
		orders[i].Cloid = fmt.Sprintf("0x%032x", i+1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Exchange(ctx, Place{Type: "order", Orders: orders, Grouping: "na"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if reads[orders[0].Cloid] != 1 || reads[orders[1].Cloid] != 1 || reads[orders[2].Cloid] != 2 {
		t.Fatalf("bounded pass lost progress: %v", reads)
	}
}

func TestUnknownOutcomeNeverExpiresIntoRejection(t *testing.T) {
	c := restVaultClient(t)
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/exchange" {
			sends.Add(1)
			http.Error(w, "reply lost", http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"height":10,"status":"unknownOid"}`)
	}))
	defer server.Close()
	c.URL = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := c.Exchange(ctx, Place{Type: "order", Orders: []Order{alo("100")}, Grouping: "na"})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(c.pendingPath(), old, old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	err = c.RecoverPending(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrExecutionRejected) {
		t.Fatalf("unknown must stay unknown: %v", err)
	}
	if _, err := os.Stat(c.pendingPath()); err != nil {
		t.Fatalf("unknown pending was lost: %v", err)
	}
	if err := c.Exchange(context.Background(), Cancel{Type: "cancel", Cancels: []CancelItem{{OID: 11}}}); err == nil || sends.Load() != 1 {
		t.Fatalf("new submission must remain blocked: %v; sends=%d", err, sends.Load())
	}
}

func TestModifyReusedCloidDoesNotResolveToOldOrder(t *testing.T) {
	c := restVaultClient(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/exchange" {
			fmt.Fprint(w, `{"status":"ok","response":{"type":"default"}}`)
			return
		}
		fmt.Fprint(w, `{"height":10,"status":"order","order":{"status":"canceled","order":{"oid":11}}}`)
	}))
	defer server.Close()
	c.URL = server.URL
	order := alo("100")
	order.Cloid = "0x11111111111111111111111111111111"
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Exchange(ctx, Modify{Type: "modify", OID: 11, Order: order})
	if !errors.Is(err, context.DeadlineExceeded) || len(c.LastOrderIDs) != 0 {
		t.Fatalf("old order reported as replacement: ids=%v err=%v", c.LastOrderIDs, err)
	}
}

func TestLookupRejectsProjectionBelowDurableHeight(t *testing.T) {
	c := restVaultClient(t)
	c.RequireExecutionHeight = true
	if err := c.observeHeight(12); err != nil {
		t.Fatal(err)
	}
	fresh := restVaultClient(t)
	fresh.StateDir, fresh.RequireExecutionHeight = c.StateDir, true
	if err := fresh.RestoreExecutionHeight(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"height":11,"status":"order","order":{"status":"canceled","order":{"oid":11}}}`)
	}))
	defer server.Close()
	fresh.URL = server.URL
	if _, _, err := fresh.lookup(context.Background(), uint64(11)); err == nil || !strings.Contains(err.Error(), "behind confirmed height") {
		t.Fatalf("stale query accepted: %v", err)
	}
}

func TestBatchReceiptRejectsIncompleteOrAmbiguousEvidence(t *testing.T) {
	for _, statuses := range []string{
		`[]`, `[{"resting":{"oid":1}}]`,
		`[{"resting":{"oid":0}},{"error":"bad"}]`,
		`[{"resting":{"oid":1}},{"error":"query the order status before retrying"}]`,
		`[{"resting":{"oid":1}},{"error":"incomplete","localCommitted":{}}]`,
		`[{"resting":{"oid":1}},"waitingForTrigger"]`,
	} {
		raw := json.RawMessage(`{"type":"order","data":{"statuses":` + statuses + `}}`)
		if _, err := parseBatchReceipt(raw, 2); err == nil {
			t.Errorf("accepted ambiguous receipt: %s", raw)
		}
	}
}

// Removing a transport must not turn its unknown submissions into REST requests.
func TestRecoverRejectsRemovedNativeTransport(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, transport, wantError string
	}{
		{"endpoint", "127.0.0.1:9090", "http", "identity/endpoint mismatch"},
		{"transport", "", "native:/local/devnet", "transport/network mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := restVaultClient(t)
			pending := pendingRequest{
				Signer: client.SignerAddress(), Account: client.AccountAddress(), URL: client.URL,
				Endpoint: tc.endpoint, Transport: tc.transport, Nonce: 1,
				Action: json.RawMessage(`{"type":"order"}`),
			}
			raw, err := json.Marshal(pending)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(client.pendingPath(), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := client.RecoverPending(context.Background()); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("removed transport must fail before reconciliation: %v", err)
			}
			retained, err := os.ReadFile(client.pendingPath())
			if err != nil || string(retained) != string(raw) {
				t.Fatalf("unknown legacy request must remain unchanged: %s, %v", retained, err)
			}
		})
	}
}
