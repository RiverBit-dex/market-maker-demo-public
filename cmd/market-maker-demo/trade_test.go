package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/riverbit-dex/market-maker-demo-public/internal/binws"
	"github.com/riverbit-dex/market-maker-demo-public/internal/binws/binwstest"
	"github.com/riverbit-dex/market-maker-demo-public/internal/config"
	"github.com/riverbit-dex/market-maker-demo-public/internal/hlexchange"
)

func TestExampleActionFilesParse(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("examples: %v %v", files, err)
	}
	actions, err := readActions(strings.Join(files, ","))
	if err != nil || len(actions) != len(files) {
		t.Fatalf("readActions = %d, %v", len(actions), err)
	}
	for _, a := range actions {
		if err := validateActionAsset(a, 1); err != nil {
			t.Errorf("%T: %v", a, err)
		}
	}
}

const (
	testVault  = "0x00000000000000000000000000000000000000aa"
	testLeader = "0x00000000000000000000000000000000000000bb"
	testKeyHex = "0000000000000000000000000000000000000000000000000000000000000007"
)

func testSigner() string {
	key, _ := crypto.HexToECDSA(testKeyHex)
	return strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
}

// fakeGateway is the HTTP side: POST /info always (startup queries), plus
// POST /exchange and the orderStatus/openOrders reconciliation when rest is set.
type fakeGateway struct {
	*httptest.Server
	mu                  sync.Mutex
	infos               []string // info types in arrival order
	exchanges           []string // raw /exchange bodies
	unknown             bool
	orderStatusObserved chan struct{}
}

func newFakeGateway(t *testing.T, account string, rest bool) *fakeGateway {
	g := &fakeGateway{orderStatusObserved: make(chan struct{}, 1)}
	signer := testSigner()
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/exchange" {
			if !rest {
				t.Errorf("ws mode must not POST /exchange: %s", body)
				http.Error(w, "unexpected", http.StatusBadRequest)
				return
			}
			g.mu.Lock()
			g.exchanges = append(g.exchanges, string(body))
			g.mu.Unlock()
			fmt.Fprint(w, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":501}},{"resting":{"oid":502}}]}}}`)
			return
		}
		if r.URL.Path != "/info" {
			t.Errorf("HTTP path %s, want /info or /exchange (no /api prefix)", r.URL.Path)
		}
		var q map[string]any
		_ = json.Unmarshal(body, &q)
		typ, _ := q["type"].(string)
		g.mu.Lock()
		g.infos = append(g.infos, typ)
		g.mu.Unlock()
		switch typ {
		case "chainIdentity":
			fmt.Fprint(w, `{"agentChainId":12581,"chainId":12581,"nextAgentChainId":12581,"nextChainId":12581,"signatureChainId":"0x3125"}`)
		case "vaultRegistry":
			fmt.Fprintf(w, `{"vaultAddress":%q,"status":"normal","class":"guild"}`, testVault)
		case "vaultDetails":
			fmt.Fprintf(w, `{"leader":%q}`, testLeader)
		case "userRole":
			if q["user"] != signer {
				t.Errorf("userRole user = %v, want the API wallet %s", q["user"], signer)
			}
			fmt.Fprintf(w, `{"role":"agent","data":{"user":%q}}`, testLeader)
		case "metaAndAssetCtxs":
			fmt.Fprint(w, `[{"universe":[{"name":"RBT","szDecimals":2,"maxLeverage":20},{"name":"BTC","szDecimals":5,"maxLeverage":40}]},[]]`)
		case "orderStatus", "openOrders":
			if !rest {
				t.Errorf("ws mode must reconcile over info_json, got HTTP %s", body)
			}
			if q["user"] != account {
				t.Errorf("%s user = %v, want %s", typ, q["user"], account)
			}
			if typ == "openOrders" {
				fmt.Fprint(w, `[]`)
				return
			}
			g.mu.Lock()
			unknown := g.unknown
			g.mu.Unlock()
			if unknown {
				fmt.Fprint(w, `{"height":3,"status":"unknownOid"}`)
				select {
				case g.orderStatusObserved <- struct{}{}:
				default:
				}
				return
			}
			fmt.Fprint(w, `{"height":3,"status":"order","order":{"status":"open","order":{"coin":"BTC","oid":501}}}`)
		default:
			t.Errorf("unexpected info %s", body)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

// recover reconnects using the same identity but never reads ACTION_FILE or
// submits another exchange. It works for a vault and an API wallet's master.
func TestRecoverCommandNeverSubmitsActions(t *testing.T) {
	for _, transport := range []string{config.OrderTransportREST, config.OrderTransportWS} {
		for _, vault := range []string{"", testVault} {
			t.Run(transport+"/vault="+vault, func(t *testing.T) {
				account := testLeader
				if vault != "" {
					account = vault
				}
				gateway := newFakeGateway(t, account, transport == config.OrderTransportREST)
				gateway.unknown = true
				var visible atomic.Bool
				var sends atomic.Int32
				ws := binwstest.New(t, "pre-api.riverbit.io")
				ws.User = testLeader
				ws.Handle = func(conn *binwstest.Conn, f binws.ClientFrame) {
					if f.Body == binws.ClientSubscribe {
						return
					}
					typ, payload := "action", `{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":501}}]}}}`
					if f.Body == binws.ClientAction {
						sends.Add(1)
					} else {
						var q struct {
							Type string `json:"type"`
						}
						_ = json.Unmarshal(f.Data, &q)
						data := `[]`
						if q.Type == "orderStatus" {
							data = `{"height":3,"status":"unknownOid"}`
							select {
							case gateway.orderStatusObserved <- struct{}{}:
							default:
							}
							if visible.Load() {
								data = `{"height":3,"status":"order","order":{"status":"open","order":{"oid":501}}}`
							}
						}
						typ, payload = "info", fmt.Sprintf(`{"type":%q,"data":%s}`, q.Type, data)
					}
					_ = conn.SendJSON(f.ID, fmt.Sprintf(`{"channel":"post","data":{"id":%d,"response":{"type":%q,"payload":%s}}}`, f.ID, typ, payload))
				}
				cfg := tradeConfig(t, ws.URL(), gateway.URL, vault, transport)
				c, err := newClient(cfg, vault)
				if err != nil {
					t.Fatal(err)
				}
				c.StateDir, c.SignChainID = cfg.StateDir, 12581
				if vault == "" {
					if err := c.SetPrincipal(testLeader); err != nil {
						t.Fatal(err)
					}
				}
				if transport == config.OrderTransportWS {
					session, err := binws.Dial(context.Background(), binws.Config{URL: ws.URL(), LoginHost: cfg.BinWSLoginHost, Sign: c.PersonalSign})
					if err != nil {
						t.Fatal(err)
					}
					defer session.Close()
					c.WS, c.Queries = session, session
				}
				// Cancel only after submission and the first reconciliation query.
				// A short wall-clock deadline can expire before HTTP sends anything
				// on a loaded race-test runner, leaving no submitted request to recover.
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				submitted := make(chan error, 1)
				go func() {
					submitted <- c.Exchange(ctx, &hlexchange.Place{Type: "order", Orders: []hlexchange.Order{{Asset: 0, Price: "100", Size: "1", Type: hlexchange.OrderType{Limit: hlexchange.Limit{TIF: "Alo"}}}}, Grouping: "na"})
				}()
				select {
				case <-gateway.orderStatusObserved:
					cancel()
				case <-ctx.Done():
					t.Fatalf("initial request never reached reconciliation: %v", ctx.Err())
				}
				if err = <-submitted; !errors.Is(err, context.Canceled) {
					t.Fatalf("prepare pending: %v", err)
				}
				gateway.mu.Lock()
				gateway.unknown = false
				gateway.mu.Unlock()
				visible.Store(true)
				t.Setenv("ACTION_FILE", "/does-not-exist.json")
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				err = runTrade(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), true)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				gateway.mu.Lock()
				count := len(gateway.exchanges)
				gateway.mu.Unlock()
				if count+int(sends.Load()) != 1 {
					t.Fatalf("recovery submitted actions: HTTP=%d WS=%d", count, sends.Load())
				}
				files, err := filepath.Glob(filepath.Join(cfg.StateDir, "*.pending.json"))
				if err != nil || len(files) != 0 {
					t.Fatalf("pending after recovery: %v, %v", files, err)
				}
			})
		}
	}
}

func tradeConfig(t *testing.T, binWSURL, gatewayURL, vault, transport string) config.Config {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte(testKeyHex+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACTION_FILE", "../../examples/btc-batch-order.sample.json")
	return config.Config{
		BinWSURL: binWSURL, BinWSLoginHost: "pre-api.riverbit.io", APIGatewayURL: gatewayURL,
		Coin: "BTC", BookStream: "l2", OrderTransport: transport, GuildVault: vault, KeyFile: keyFile, StateDir: filepath.Join(dir, "state"),
	}
}

type signedPlace struct {
	Action       hlexchange.Place     `json:"action"`
	Nonce        uint64               `json:"nonce"`
	Signature    hlexchange.Signature `json:"signature"`
	VaultAddress string               `json:"vaultAddress"`
}

// The whole `trade` command for a vault in ws mode: HTTP startup checks, one
// bin-ws login, order-event subscriptions for the vault, the signed order as
// action_json, then orderStatus / openOrders as info_json on the same
// connection.
func TestTradeForVaultOverBinWS(t *testing.T) {
	var mu sync.Mutex
	var frames []string // client frames in arrival order: "subscribe:<json>" / "action:<json>" / "info:<json>"
	ws := binwstest.New(t, "pre-api.riverbit.io")
	ws.User = testLeader
	ws.Handle = func(c *binwstest.Conn, f binws.ClientFrame) {
		mu.Lock()
		defer mu.Unlock()
		switch f.Body {
		case binws.ClientSubscribe:
			frames = append(frames, "subscribe:"+string(f.Data))
		case binws.ClientAction:
			frames = append(frames, "action:"+string(f.Data))
			_ = c.SendJSON(f.ID, fmt.Sprintf(`{"channel":"post","data":{"id":%d,"response":{"type":"action","payload":{"status":"ok","response":{"type":"order","data":{"statuses":[{"resting":{"oid":501}},{"resting":{"oid":502}}]}}}}}}`, f.ID))
		case binws.ClientInfo:
			frames = append(frames, "info:"+string(f.Data))
			var q struct {
				Type string `json:"type"`
				User string `json:"user"`
			}
			_ = json.Unmarshal(f.Data, &q)
			if q.User != testVault {
				t.Errorf("info_json %s user = %s, want the vault", q.Type, q.User)
			}
			data := `[]`
			if q.Type == "orderStatus" {
				data = `{"height":3,"status":"order","order":{"status":"open","order":{"coin":"BTC","oid":501}}}`
			}
			_ = c.SendJSON(f.ID, fmt.Sprintf(`{"channel":"post","data":{"id":%d,"response":{"type":"info","payload":{"type":%q,"data":%s}}}}`, f.ID, q.Type, data))
		}
	}
	gateway := newFakeGateway(t, testVault, false)
	cfg := tradeConfig(t, ws.URL(), gateway.URL, testVault, config.OrderTransportWS)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := trade(context.Background(), cfg, logger); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(ws.Logins()) != 1 {
		t.Fatalf("logins = %d", len(ws.Logins()))
	}
	want := []string{
		`subscribe:{"type":"orderUpdates","user":"` + testVault + `"}`,
		`subscribe:{"type":"userFills","user":"` + testVault + `"}`,
	}
	// subscribe, subscribe, action, orderStatus x2 (one per cloid), openOrders
	if len(frames) != 6 || frames[0] != want[0] || frames[1] != want[1] || !strings.HasPrefix(frames[2], "action:") {
		t.Fatalf("frames = %v", frames)
	}
	for _, f := range frames[3:5] {
		if !strings.HasPrefix(f, `info:{"oid":"0x`) || !strings.Contains(f, `"type":"orderStatus"`) {
			t.Fatalf("reconciliation frame = %s, want orderStatus by cloid over info_json", f)
		}
	}
	if frames[5] != `info:{"type":"openOrders","user":"`+testVault+`"}` {
		t.Fatalf("last frame = %s, want openOrders over info_json", frames[5])
	}
	var req signedPlace
	if err := json.Unmarshal([]byte(strings.TrimPrefix(frames[2], "action:")), &req); err != nil {
		t.Fatal(err)
	}
	if req.VaultAddress != testVault || len(req.Action.Orders) != 2 || req.Signature.R == "" {
		t.Fatalf("request = %+v", req)
	}
}

// rest mode: no bin-ws connection at all. The signed order is POSTed to
// /exchange and reconciled with orderStatus and openOrders over POST /info.
func TestTradeForVaultOverREST(t *testing.T) {
	ws := binwstest.New(t, "pre-api.riverbit.io")
	ws.Handle = func(*binwstest.Conn, binws.ClientFrame) { t.Error("rest mode must not send bin-ws frames") }
	gateway := newFakeGateway(t, testVault, true)
	cfg := tradeConfig(t, ws.URL(), gateway.URL, testVault, config.OrderTransportREST)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := trade(context.Background(), cfg, logger); err != nil {
		t.Fatal(err)
	}
	if n := len(ws.Logins()); n != 0 {
		t.Fatalf("rest mode dialed bin-ws (%d logins)", n)
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	if len(gateway.exchanges) != 1 {
		t.Fatalf("/exchange requests = %d, want 1", len(gateway.exchanges))
	}
	var req signedPlace
	if err := json.Unmarshal([]byte(gateway.exchanges[0]), &req); err != nil {
		t.Fatal(err)
	}
	if req.VaultAddress != testVault || len(req.Action.Orders) != 2 || req.Signature.R == "" {
		t.Fatalf("request = %+v", req)
	}
	if !strings.HasPrefix(gateway.exchanges[0], `{"action":{"type":"order","orders":[`) {
		t.Fatalf("/exchange body not in signing key order: %s", gateway.exchanges[0])
	}
	got := strings.Join(gateway.infos, ",")
	if want := "chainIdentity,vaultRegistry,vaultDetails,userRole,metaAndAssetCtxs,orderStatus,orderStatus,openOrders"; got != want {
		t.Fatalf("/info sequence = %s, want %s", got, want)
	}
}

// rest mode without a vault: the API wallet's master account comes from
// POST /info userRole instead of the bin-ws login.
func TestTradeForAPIWalletOverRESTResolvesMasterViaUserRole(t *testing.T) {
	gateway := newFakeGateway(t, testLeader, true)
	// BinWSURL points nowhere: any dial attempt would fail the command.
	cfg := tradeConfig(t, "ws://127.0.0.1:1/bin-ws", gateway.URL, "", config.OrderTransportREST)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := trade(context.Background(), cfg, logger); err != nil {
		t.Fatal(err)
	}
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	var req struct {
		VaultAddress *string `json:"vaultAddress"`
	}
	if len(gateway.exchanges) != 1 || json.Unmarshal([]byte(gateway.exchanges[0]), &req) != nil || req.VaultAddress != nil {
		t.Fatalf("/exchange = %v; an API wallet trading for its master carries no vaultAddress", gateway.exchanges)
	}
	if got := strings.Join(gateway.infos, ","); got != "chainIdentity,metaAndAssetCtxs,userRole,orderStatus,orderStatus,openOrders" {
		t.Fatalf("/info sequence = %s", got)
	}
}
