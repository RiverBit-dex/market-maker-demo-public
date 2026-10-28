package binws_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/riverbit-dex/market-maker-demo-public/internal/binws"
	"github.com/riverbit-dex/market-maker-demo-public/internal/binws/binwstest"
)

const master = "0x1111111111111111111111111111111111111111"

func signer(t *testing.T) (binws.Signer, string) {
	t.Helper()
	key, err := crypto.HexToECDSA(strings.Repeat("55", 32))
	if err != nil {
		t.Fatal(err)
	}
	return func(msg []byte) ([]byte, error) {
		sig, err := crypto.Sign(binws.PersonalHash(msg), key)
		if err == nil {
			sig[64] += 27
		}
		return sig, err
	}, strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
}

func TestSessionLoginSubscribeAndMatchRepliesByID(t *testing.T) {
	sign, address := signer(t)
	server := binwstest.New(t, "pre-api.riverbit.io")
	server.User = master

	var mu sync.Mutex
	var subs []string
	actions := make(chan binws.ClientFrame, 2)
	server.Handle = func(c *binwstest.Conn, f binws.ClientFrame) {
		switch f.Body {
		case binws.ClientSubscribe:
			mu.Lock()
			subs = append(subs, string(f.Data))
			mu.Unlock()
			_ = c.SendJSON(0, `{"channel":"subscriptionResponse","data":{"method":"subscribe","subscription":`+string(f.Data)+`}}`)
		case binws.ClientAction:
			actions <- f
			if len(actions) < 2 {
				return
			}
			// Answer both in reverse order, with a push in between: replies must
			// still reach the request with the same outer id.
			first, second := <-actions, <-actions // arrival order
			for _, req := range []binws.ClientFrame{first, second} {
				if req.ID == 0 {
					t.Errorf("action frame without id")
				}
			}
			_ = c.SendJSON(second.ID, fmt.Sprintf(`{"channel":"post","data":{"id":%d,"response":{"type":"action","payload":{"echo":%s}}}}`, second.ID, second.Data))
			_ = c.SendJSON(0, `{"channel":"orderUpdates","data":[]}`)
			_ = c.SendJSON(first.ID, fmt.Sprintf(`{"channel":"post","data":{"id":%d,"response":{"type":"action","payload":{"echo":%s}}}}`, first.ID, first.Data))
		}
	}

	pushes := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := binws.Dial(ctx, binws.Config{
		URL: server.URL(), LoginHost: "pre-api.riverbit.io", Sign: sign,
		OnFrame: func(f binws.ServerFrame) {
			var head struct {
				Channel string `json:"channel"`
			}
			_ = json.Unmarshal(f.Data, &head)
			pushes <- head.Channel
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if session.User() != master || session.Signer() != address {
		t.Fatalf("login user=%s signer=%s", session.User(), session.Signer())
	}
	if logins := server.Logins(); len(logins) != 1 || len(logins[0].Nonce) != 16 {
		t.Fatalf("server logins = %+v", logins)
	}

	if err := session.Subscribe(map[string]string{"type": "l2", "c": "RBT"}); err != nil {
		t.Fatal(err)
	}
	if got := waitPush(t, pushes); got != "subscriptionResponse" {
		t.Fatalf("first push = %s", got)
	}
	mu.Lock()
	if len(subs) != 1 || subs[0] != `{"c":"RBT","type":"l2"}` {
		t.Fatalf("subscriptions = %v", subs)
	}
	mu.Unlock()

	type result struct {
		body    string
		payload string
		err     error
	}
	results := make(chan result, 2)
	for _, body := range []string{`{"n":1}`, `{"n":2}`} {
		go func(body string) {
			reqCtx, done := context.WithTimeout(ctx, 5*time.Second)
			defer done()
			payload, err := session.PostAction(reqCtx, []byte(body))
			results <- result{body, string(payload), err}
		}(body)
	}
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if want := `{"echo":` + r.body + `}`; r.payload != want {
			t.Fatalf("request %s got payload %s", r.body, r.payload)
		}
	}
	if got := waitPush(t, pushes); got != "orderUpdates" {
		t.Fatalf("push between replies = %s", got)
	}
}

func waitPush(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no push frame")
		return ""
	}
}

func TestSessionRejectsSignerMismatch(t *testing.T) {
	sign, _ := signer(t)
	// The server checks a different host, so it recovers some other address.
	server := binwstest.New(t, "other-host")
	_, err := binws.Dial(context.Background(), binws.Config{URL: server.URL(), LoginHost: "pre-api.riverbit.io", Sign: sign})
	if err == nil || !strings.Contains(err.Error(), "server recovered signer") {
		t.Fatalf("Dial error = %v", err)
	}
}

func TestSessionPendingRequestFailsWhenConnectionEnds(t *testing.T) {
	sign, _ := signer(t)
	server := binwstest.New(t, "h")
	server.Handle = func(c *binwstest.Conn, f binws.ClientFrame) {
		if f.Body == binws.ClientAction {
			_ = c.Close()
		}
	}
	session, err := binws.Dial(context.Background(), binws.Config{URL: server.URL(), LoginHost: "h", Sign: sign})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := session.PostAction(ctx, []byte(`{}`)); err == nil || ctx.Err() != nil {
		t.Fatalf("PostAction error = %v (ctx %v); want a connection error before the timeout", err, ctx.Err())
	}
}

func TestSessionClosesOnUndecodableFrame(t *testing.T) {
	sign, _ := signer(t)
	server := binwstest.New(t, "h")
	server.Handle = func(c *binwstest.Conn, f binws.ClientFrame) {
		// No recognized body: silently skipping this could lose an l2 delta.
		_ = c.Send(binws.ServerFrame{Kind: 99})
	}
	session, err := binws.Dial(context.Background(), binws.Config{URL: server.URL(), LoginHost: "h", Sign: sign})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Subscribe(map[string]string{"type": "l2", "c": "RBT"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
		if !strings.Contains(session.Err().Error(), "decode server frame") {
			t.Fatal(session.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("undecodable frame was silently skipped")
	}
}
