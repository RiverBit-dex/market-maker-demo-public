// Package marketdata subscribes to bin-ws market data and order events and
// prints them. Typed frames are converted with value = integer / 10^decimals.
package marketdata

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverbit-dex/market-maker-demo-public/internal/binws"
)

// Subscription is a bin-ws subscription object, sent as subscribe_json. Field
// order is fixed so the wire JSON is deterministic.
type Subscription struct {
	Type     string `json:"type"`
	C        string `json:"c,omitempty"` // `l2` uses "c" for the coin
	Coin     string `json:"coin,omitempty"`
	Interval string `json:"interval,omitempty"`
	User     string `json:"user,omitempty"`
}

// Subscriptions lists what the demo subscribes to: the order book (`l2`
// incremental by default, or `l2Book`), trades, 1m candles and, when user is
// set, orderUpdates and userFills for that account (master or vault).
func Subscriptions(coin, bookStream, user string) []Subscription {
	book := Subscription{Type: "l2", C: coin}
	if bookStream == "l2Book" {
		book = Subscription{Type: "l2Book", Coin: coin}
	}
	subs := []Subscription{book, {Type: "trades", Coin: coin}, {Type: "candle", Coin: coin, Interval: "1m"}}
	if user != "" {
		subs = append(subs, Subscription{Type: "orderUpdates", User: user}, Subscription{Type: "userFills", User: user})
	}
	return subs
}

// OrderSubscriptions are the account event streams only (used by `trade`).
func OrderSubscriptions(user string) []Subscription {
	return []Subscription{{Type: "orderUpdates", User: user}, {Type: "userFills", User: user}}
}

type orderEvent struct {
	Order struct {
		OID   uint64 `json:"oid"`
		Coin  string `json:"coin"`
		Side  string `json:"side"`
		Price string `json:"limitPx"`
		Size  string `json:"sz"`
		Cloid string `json:"cloid"`
	} `json:"order"`
	Status string `json:"status"`
}

type jsonTrade struct {
	Coin  string `json:"coin"`
	Side  string `json:"side"`
	Price string `json:"px"`
	Size  string `json:"sz"`
	Time  int64  `json:"time"`
}

type jsonLevel struct {
	Price string `json:"px"`
	Size  string `json:"sz"`
	N     int    `json:"n"`
}

// Handler prints server frames. It is safe to use as binws.Config.OnFrame:
// it runs on the session's read goroutine and never waits on the connection.
type Handler struct {
	Log *slog.Logger
	// Book replays the `l2` subscription; nil when not subscribed to `l2`.
	Book *binws.Book
	// Resync is called when an `l2` frame cannot be applied (gap, decimals
	// change): the book is cleared and should be re-subscribed.
	Resync func()
}

// Handle dispatches one frame: json by channel, typed frames by kind.
func (h *Handler) Handle(f binws.ServerFrame) {
	switch f.Kind {
	case binws.KindJSON:
		h.handleJSON(f)
	case binws.KindL2Delta:
		d, err := binws.UnmarshalL2Delta(f.Data)
		if err != nil {
			h.applyBook(func(*binws.Book) error { return err }, false)
			return
		}
		h.applyBook(func(b *binws.Book) error { return b.ApplyDelta(d) }, d.Snapshot != nil)
	case binws.KindL2Book:
		b, err := binws.UnmarshalL2Book(f.Data)
		if err != nil {
			h.Log.Warn("bad l2Book frame", "error", err)
			return
		}
		attrs := []any{"coin", b.Coin, "time_ms", b.Time, "bid_levels", len(b.BidPx), "ask_levels", len(b.AskPx)}
		if len(b.BidPx) > 0 {
			attrs = append(attrs, "best_bid_px", binws.FormatFixed(b.BidPx[0], b.PxDecimals), "best_bid_sz", binws.FormatFixed(b.BidSz[0], b.SzDecimals))
		}
		if len(b.AskPx) > 0 {
			attrs = append(attrs, "best_ask_px", binws.FormatFixed(b.AskPx[0], b.PxDecimals), "best_ask_sz", binws.FormatFixed(b.AskSz[0], b.SzDecimals))
		}
		h.Log.Info("l2Book", attrs...)
	case binws.KindBBO:
		b, err := binws.UnmarshalBBO(f.Data)
		if err != nil {
			h.Log.Warn("bad bbo frame", "error", err)
			return
		}
		attrs := []any{"coin", b.Coin, "time_ms", b.Time}
		if b.BidPx != nil && b.BidSz != nil {
			attrs = append(attrs, "bid_px", binws.FormatFixed(*b.BidPx, b.PxDecimals), "bid_sz", binws.FormatFixed(*b.BidSz, b.SzDecimals))
		}
		if b.AskPx != nil && b.AskSz != nil {
			attrs = append(attrs, "ask_px", binws.FormatFixed(*b.AskPx, b.PxDecimals), "ask_sz", binws.FormatFixed(*b.AskSz, b.SzDecimals))
		}
		h.Log.Info("bbo", attrs...)
	case binws.KindCandle:
		c, err := binws.UnmarshalCandle(f.Data)
		if err != nil {
			h.Log.Warn("bad candle frame", "error", err)
			return
		}
		px := func(v uint64) string { return binws.FormatFixed(v, c.PxDecimals) }
		h.Log.Info("candle", "coin", c.Coin, "interval", c.Interval, "open_time_ms", c.OpenTime, "close_time_ms", c.CloseTime,
			"o", px(c.Open), "h", px(c.High), "l", px(c.Low), "c", px(c.Close),
			"v", binws.FormatFixed(c.Volume, c.SzDecimals), "n", c.TradeCount)
	case binws.KindTrades:
		t, err := binws.UnmarshalTrades(f.Data)
		if err != nil {
			h.Log.Warn("bad trades frame", "error", err)
			return
		}
		for _, tr := range t.Trades {
			side := "sell"
			if tr.Buy {
				side = "buy"
			}
			h.Log.Info("trade", "coin", t.Coin, "side", side, "px", binws.FormatFixed(tr.Px, t.PxDecimals),
				"sz", binws.FormatFixed(tr.Sz, t.SzDecimals), "time_ms", tr.Time, "tid", tr.TID,
				"buyer", hexOrEmpty(tr.Buyer), "seller", hexOrEmpty(tr.Seller))
		}
	default:
		h.Log.Warn("unexpected frame", "kind", f.Kind.String(), "id", f.ID)
	}
}

func hexOrEmpty(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return "0x" + hex.EncodeToString(b)
}

func (h *Handler) applyBook(apply func(*binws.Book) error, snapshot bool) {
	if h.Book == nil {
		h.Log.Warn("l2 frame without an l2 subscription")
		return
	}
	if err := apply(h.Book); err != nil {
		h.Log.Warn("l2 book out of sync; resubscribing", "error", err)
		*h.Book = *binws.NewBook(h.Book.Coin)
		if h.Resync != nil {
			h.Resync()
		}
		return
	}
	bid, ask := h.Book.Best()
	attrs := []any{"coin", h.Book.Coin, "time_ms", h.Book.Time, "snapshot", snapshot, "bid_levels", len(h.Book.Bids), "ask_levels", len(h.Book.Asks)}
	if bid != nil {
		attrs = append(attrs, "bid1_px", bid.PxText, "bid1_sz", bid.SzText)
	}
	if ask != nil {
		attrs = append(attrs, "ask1_px", ask.PxText, "ask1_sz", ask.SzText)
	}
	h.Log.Info("l2", attrs...)
}

func (h *Handler) handleJSON(f binws.ServerFrame) {
	var frame struct {
		Channel string          `json:"channel"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(f.Data, &frame); err != nil {
		h.Log.Warn("invalid json frame", "error", err)
		return
	}
	switch frame.Channel {
	case "l2": // JSON fallback of the typed l2 stream: same book
		h.applyBook(func(b *binws.Book) error { return b.ApplyJSON(frame.Data) }, false)
	case "l2Book":
		var book struct {
			Coin   string         `json:"coin"`
			Time   int64          `json:"time"`
			Levels [2][]jsonLevel `json:"levels"`
		}
		if err := json.Unmarshal(frame.Data, &book); err != nil {
			h.Log.Warn("invalid l2Book json", "error", err)
			return
		}
		attrs := []any{"coin", book.Coin, "time_ms", book.Time}
		if len(book.Levels[0]) > 0 {
			attrs = append(attrs, "best_bid_px", book.Levels[0][0].Price, "best_bid_sz", book.Levels[0][0].Size)
		}
		if len(book.Levels[1]) > 0 {
			attrs = append(attrs, "best_ask_px", book.Levels[1][0].Price, "best_ask_sz", book.Levels[1][0].Size)
		}
		h.Log.Info("l2Book (json)", attrs...)
	case "trades":
		var trades []jsonTrade
		if err := json.Unmarshal(frame.Data, &trades); err != nil {
			h.Log.Warn("invalid trades json", "error", err)
			return
		}
		for _, t := range trades {
			h.Log.Info("trade (json)", "coin", t.Coin, "side", t.Side, "px", t.Price, "sz", t.Size, "time_ms", t.Time)
		}
	case "candle", "bbo":
		h.Log.Info(frame.Channel+" (json)", "data", string(frame.Data))
	case "orderUpdates":
		var events []orderEvent
		if err := json.Unmarshal(frame.Data, &events); err != nil {
			h.Log.Warn("invalid orderUpdates json", "error", err)
			return
		}
		for _, e := range events {
			h.Log.Info("order update", "oid", e.Order.OID, "cloid", e.Order.Cloid, "coin", e.Order.Coin, "side", e.Order.Side,
				"px", e.Order.Price, "sz", e.Order.Size, "status", e.Status)
		}
	case "userFills", "openOrders":
		h.Log.Info(frame.Channel, "data", truncate(frame.Data, 2000))
	case "subscriptionResponse":
		h.Log.Info("subscribed", "data", truncate(frame.Data, 300))
	case "pong":
	case "error":
		h.Log.Warn("server error", "data", truncate(frame.Data, 500))
	case "post":
		h.Log.Warn("reply without a waiting request (late or timed out)", "id", f.ID, "data", truncate(frame.Data, 500))
	default:
		h.Log.Info("json frame", "channel", frame.Channel, "data", truncate(frame.Data, 500))
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// RunConfig configures the market data loop.
type RunConfig struct {
	URL        string
	LoginHost  string
	Sign       binws.Signer
	Coin       string
	BookStream string // "l2" or "l2Book"
	// User is the account whose orderUpdates/userFills are printed: the vault
	// when trading for one. Empty = the master account resolved by login.
	User string
	Log  *slog.Logger
}

// Run keeps one bin-ws session alive (reconnect -> login -> subscribe) until
// ctx is canceled.
func Run(ctx context.Context, rc RunConfig) error {
	backoff := time.Second
	for ctx.Err() == nil {
		err := runOnce(ctx, rc)
		if ctx.Err() != nil {
			return nil
		}
		rc.Log.Warn("bin-ws connection ended", "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff = min(2*backoff, 15*time.Second)
		}
	}
	return nil
}

func runOnce(ctx context.Context, rc RunConfig) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	h := &Handler{Log: rc.Log}
	if rc.BookStream != "l2Book" {
		h.Book = binws.NewBook(rc.Coin)
	}
	var session *binws.Session
	ready := make(chan struct{})
	h.Resync = func() {
		<-ready
		sub := Subscription{Type: "l2", C: rc.Coin}
		_ = session.Unsubscribe(sub)
		if err := session.Subscribe(sub); err != nil {
			rc.Log.Warn("l2 resubscribe failed", "error", err)
		}
	}
	session, err := binws.Dial(sessionCtx, binws.Config{
		URL: rc.URL, LoginHost: rc.LoginHost, Sign: rc.Sign, OnFrame: h.Handle,
		OnDecodeError: func(err error) { rc.Log.Warn("undecodable server frame", "error", err) },
	})
	close(ready)
	if err != nil {
		return err
	}
	defer session.Close()
	LogLogin(rc.Log, session)
	user := rc.User
	if user == "" {
		user = session.User()
	}
	for _, sub := range Subscriptions(rc.Coin, rc.BookStream, user) {
		if err := session.Subscribe(sub); err != nil {
			return fmt.Errorf("subscribe %s: %w", sub.Type, err)
		}
	}
	rc.Log.Info("subscribed to bin-ws streams", "coin", rc.Coin, "book", rc.BookStream, "orders_user", user, "url", rc.URL)
	<-session.Done()
	return session.Err()
}

// LogLogin prints who the server recognized.
func LogLogin(log *slog.Logger, s *binws.Session) {
	role := "API wallet"
	if s.User() == s.Signer() {
		role = "master key"
	}
	log.Info("bin-ws login ok", "user", s.User(), "signer", s.Signer(), "signer_role", role)
}
