package marketdata

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/riverbit-dex/market-maker-demo-public/internal/binws"
	"github.com/riverbit-dex/market-maker-demo-public/internal/binws/binwstest"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

func TestSubscriptionObjects(t *testing.T) {
	var got []string
	for _, s := range Subscriptions("RBT", "l2", "0xabc") {
		b, _ := json.Marshal(s)
		got = append(got, string(b))
	}
	want := []string{
		`{"type":"l2","c":"RBT"}`,
		`{"type":"trades","coin":"RBT"}`,
		`{"type":"candle","coin":"RBT","interval":"1m"}`,
		`{"type":"orderUpdates","user":"0xabc"}`,
		`{"type":"userFills","user":"0xabc"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("subscriptions:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if b, _ := json.Marshal(Subscriptions("RBT", "l2Book", "")[0]); string(b) != `{"type":"l2Book","coin":"RBT"}` || len(Subscriptions("RBT", "l2", "")) != 3 {
		t.Fatalf("l2Book subscription = %s", b)
	}
}

func p64(v uint64) *uint64 { return &v }

func TestHandlerDecodesTypedFramesWithDecimals(t *testing.T) {
	log, out := newLogger()
	resyncs := 0
	h := &Handler{Log: log, Book: binws.NewBook("RBT"), Resync: func() { resyncs++ }}
	snap := binws.L2BookFrame{Coin: "RBT", Time: 1, PxDecimals: 4, SzDecimals: 2,
		BidPx: []uint64{1000000, 999000}, BidSz: []uint64{150, 20}, BidN: []uint32{1, 1},
		AskPx: []uint64{1001000}, AskSz: []uint64{7}, AskN: []uint32{1}}
	frames := []binws.ServerFrame{
		{Kind: binws.KindL2Delta, Data: binws.L2DeltaFrame{Snapshot: &snap}.Marshal()},
		{Kind: binws.KindL2Delta, Data: binws.L2DeltaFrame{Update: &binws.L2DeltaUpdate{Coin: "RBT", Time: 2, PxDecimals: 4, SzDecimals: 2,
			BidPx: []uint64{1000500}, BidSz: []uint64{3}, AskRemoved: []uint32{0}, AskPx: []uint64{1002000}, AskSz: []uint64{9}}}.Marshal()},
		{Kind: binws.KindCandle, Data: binws.CandleFrame{Coin: "RBT", Interval: "1m", PxDecimals: 1, SzDecimals: 3, Open: 1000, High: 1010, Low: 995, Close: 1005, Volume: 12345, TradeCount: 7}.Marshal()},
		{Kind: binws.KindTrades, Data: binws.TradesFrame{Coin: "RBT", PxDecimals: 4, SzDecimals: 2, Trades: []binws.Trade{{Buy: true, Px: 1000500, Sz: 3, TID: 1}}}.Marshal()},
		{Kind: binws.KindBBO, Data: binws.BboFrame{Coin: "RBT", PxDecimals: 4, SzDecimals: 2, BidPx: p64(1000500), BidSz: p64(3)}.Marshal()},
		{Kind: binws.KindL2Book, Data: snap.Marshal()},
	}
	for _, f := range frames {
		h.Handle(f)
	}
	text := out.String()
	for _, want := range []string{
		"bid1_px=100.0500 bid1_sz=0.03 ask1_px=100.2000 ask1_sz=0.09",
		"o=100.0 h=101.0 l=99.5 c=100.5 v=12.345 n=7",
		"side=buy px=100.0500 sz=0.03",
		"bid_px=100.0500 bid_sz=0.03",
		"best_bid_px=100.0000 best_bid_sz=1.50 best_ask_px=100.1000 best_ask_sz=0.07",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("log lacks %q:\n%s", want, text)
		}
	}
	if resyncs != 0 {
		t.Fatalf("unexpected resync")
	}
	// An update that does not fit the book clears it and asks for a resubscribe.
	h.Handle(binws.ServerFrame{Kind: binws.KindL2Delta, Data: binws.L2DeltaFrame{Update: &binws.L2DeltaUpdate{Coin: "RBT", PxDecimals: 4, SzDecimals: 2, BidRemoved: []uint32{9}}}.Marshal()})
	if resyncs != 1 || h.Book.HasBook {
		t.Fatalf("resyncs=%d hasBook=%v", resyncs, h.Book.HasBook)
	}
	// Decode failures must also invalidate the book; later deltas use indices
	// into the missing update and cannot safely continue from the old snapshot.
	h.Handle(frames[0])
	h.Handle(binws.ServerFrame{Kind: binws.KindL2Delta, Data: []byte{0xff}})
	if resyncs != 2 || h.Book.HasBook {
		t.Fatalf("malformed delta retained book: resyncs=%d hasBook=%v", resyncs, h.Book.HasBook)
	}
}

func TestHandlerJSONChannels(t *testing.T) {
	log, out := newLogger()
	h := &Handler{Log: log, Book: binws.NewBook("RBT")}
	for _, body := range []string{
		`{"channel":"l2","data":{"s":{"coin":"RBT","time":1,"levels":[[{"px":"100","sz":"1","n":1}],[{"px":"101","sz":"2","n":1}]]}}}`,
		`{"channel":"l2","data":{"u":{"c":"RBT","t":2,"l":[[{"p":"100.5","s":"4"}],[]],"r":[[],[]]}}}`,
		`{"channel":"orderUpdates","data":[{"order":{"oid":42,"coin":"RBT","side":"B","limitPx":"100.5","sz":"0.1","cloid":"0x01"},"status":"open"}]}`,
		`{"channel":"userFills","data":{"user":"0xabc","fills":[]}}`,
		`{"channel":"subscriptionResponse","data":{"method":"subscribe"}}`,
	} {
		h.Handle(binws.ServerFrame{Kind: binws.KindJSON, Data: []byte(body)})
	}
	text := out.String()
	for _, want := range []string{"bid1_px=100.5 bid1_sz=4 ask1_px=101 ask1_sz=2", "oid=42 cloid=0x01", "status=open", "msg=userFills", "msg=subscribed"} {
		if !strings.Contains(text, want) {
			t.Errorf("log lacks %q:\n%s", want, text)
		}
	}
}

func TestRunLogsInAndSubscribes(t *testing.T) {
	key, err := crypto.HexToECDSA(strings.Repeat("66", 32))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(msg []byte) ([]byte, error) {
		sig, err := crypto.Sign(binws.PersonalHash(msg), key)
		if err == nil {
			sig[64] += 27
		}
		return sig, err
	}
	server := binwstest.New(t, "pre-api.riverbit.io")
	server.User = "0x2222222222222222222222222222222222222222"
	subs := make(chan string, 16)
	server.Handle = func(c *binwstest.Conn, f binws.ClientFrame) {
		if f.Body != binws.ClientSubscribe {
			return
		}
		subs <- string(f.Data)
		if strings.Contains(string(f.Data), `"l2"`) {
			snap := binws.L2BookFrame{Coin: "RBT", PxDecimals: 1, SzDecimals: 0, BidPx: []uint64{995}, BidSz: []uint64{3}, AskPx: []uint64{1005}, AskSz: []uint64{4}}
			_ = c.Send(binws.ServerFrame{Kind: binws.KindL2Delta, Data: binws.L2DeltaFrame{Snapshot: &snap}.Marshal()})
		}
	}
	log, out := newLogger()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunConfig{URL: server.URL(), LoginHost: "pre-api.riverbit.io", Sign: sign, Coin: "RBT", BookStream: "l2", Log: log})
	}()
	var got []string
	for len(got) < 5 {
		select {
		case s := <-subs:
			got = append(got, s)
		case <-time.After(5 * time.Second):
			t.Fatalf("subscriptions so far: %v\n%s", got, out.String())
		}
	}
	// Without a vault the order streams follow the logged-in master account.
	if got[3] != `{"type":"orderUpdates","user":"0x2222222222222222222222222222222222222222"}` {
		t.Fatalf("orderUpdates subscription = %s", got[3])
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "bid1_px=99.5") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"bin-ws login ok", "user=0x2222222222222222222222222222222222222222", "bid1_px=99.5 bid1_sz=3 ask1_px=100.5 ask1_sz=4"} {
		if !strings.Contains(text, want) {
			t.Errorf("log lacks %q:\n%s", want, text)
		}
	}
}
