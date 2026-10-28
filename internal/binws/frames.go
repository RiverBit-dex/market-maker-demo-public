package binws

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ClientBody is the oneof field number of a WsV2ClientFrame body.
type ClientBody int

const (
	ClientAction      ClientBody = 2 // bytes action_json: signed /exchange request JSON
	ClientInfo        ClientBody = 3 // bytes info_json
	ClientSubscribe   ClientBody = 4 // bytes subscribe_json: the subscription object itself
	ClientUnsubscribe ClientBody = 5 // bytes unsubscribe_json
	ClientPing        ClientBody = 6 // WsV2Ping (empty message)
	ClientLogin       ClientBody = 7 // WsV2Login, must be the first frame
)

// ServerKind is the oneof field number of a WsV2ServerFrame body.
type ServerKind int

const (
	KindJSON    ServerKind = 2 // bytes json: {"channel":...,"data":...}
	KindL2Book  ServerKind = 3 // L2BookFrame
	KindL2Delta ServerKind = 4 // L2DeltaFrame
	KindBBO     ServerKind = 5 // BboFrame
	KindCandle  ServerKind = 6 // CandleFrame
	KindTrades  ServerKind = 7 // TradesFrame
	KindLogin   ServerKind = 8 // WsV2LoginResult
)

func (k ServerKind) String() string {
	switch k {
	case KindJSON:
		return "json"
	case KindL2Book:
		return "l2_book"
	case KindL2Delta:
		return "l2_delta"
	case KindBBO:
		return "bbo"
	case KindCandle:
		return "candle"
	case KindTrades:
		return "trades"
	case KindLogin:
		return "login"
	}
	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// ClientFrame is a decoded WsV2ClientFrame.
type ClientFrame struct {
	ID   uint64
	Body ClientBody
	Data []byte // the body field's bytes (JSON, or an encoded WsV2Login / WsV2Ping)
}

// Marshal encodes a WsV2ClientFrame. The body is always written, even when
// empty: for oneof members the presence is the information (ping).
func (f ClientFrame) Marshal() []byte {
	out := appendVarint(nil, 1, f.ID)
	return appendBytesAlways(out, int(f.Body), f.Data)
}

func UnmarshalClientFrame(data []byte) (ClientFrame, error) {
	m, err := parse(data)
	if err != nil {
		return ClientFrame{}, err
	}
	var f ClientFrame
	if f.ID, err = m.uint64(1); err != nil {
		return ClientFrame{}, err
	}
	found := false
	for body := ClientAction; body <= ClientLogin; body++ {
		if !m.has(int(body)) {
			continue
		}
		if found {
			return ClientFrame{}, errors.New("client frame has more than one body")
		}
		found = true
		f.Body = body
		if f.Data, err = m.bytes(int(body)); err != nil {
			return ClientFrame{}, err
		}
	}
	if !found {
		return ClientFrame{}, errors.New("client frame has no body")
	}
	return f, nil
}

// ServerFrame is a decoded WsV2ServerFrame. Data holds the body field's bytes;
// decode it with the Unmarshal* function matching Kind.
type ServerFrame struct {
	ID   uint64
	Kind ServerKind
	Data []byte
}

func (f ServerFrame) Marshal() []byte {
	out := appendVarint(nil, 1, f.ID)
	return appendBytesAlways(out, int(f.Kind), f.Data)
}

func UnmarshalServerFrame(data []byte) (ServerFrame, error) {
	m, err := parse(data)
	if err != nil {
		return ServerFrame{}, err
	}
	var f ServerFrame
	if f.ID, err = m.uint64(1); err != nil {
		return ServerFrame{}, err
	}
	found := false
	for kind := KindJSON; kind <= KindLogin; kind++ {
		if !m.has(int(kind)) {
			continue
		}
		if found {
			return ServerFrame{}, errors.New("server frame has more than one body")
		}
		found = true
		f.Kind = kind
		if f.Data, err = m.bytes(int(kind)); err != nil {
			return ServerFrame{}, err
		}
	}
	if !found {
		return ServerFrame{}, errors.New("server frame has no known body")
	}
	return f, nil
}

// ---------------------------------------------------------------- login

type Login struct {
	TimestampMs uint64
	Nonce       []byte // 16 raw bytes
	Signature   []byte // 65 bytes r || s || v
}

func (l Login) Marshal() []byte {
	out := appendVarint(nil, 1, l.TimestampMs)
	out = appendBytes(out, 2, l.Nonce)
	return appendBytes(out, 3, l.Signature)
}

func UnmarshalLogin(data []byte) (Login, error) {
	r, err := newReader(data)
	if err != nil {
		return Login{}, err
	}
	l := Login{TimestampMs: r.u64(1), Nonce: r.raw(2), Signature: r.raw(3)}
	return l, r.err
}

type LoginResult struct {
	OK     bool
	User   []byte // resolved master account, 20 bytes
	Signer []byte // recovered signer, 20 bytes
	Error  string
}

func (l LoginResult) Marshal() []byte {
	out := appendBool(nil, 1, l.OK)
	out = appendBytes(out, 2, l.User)
	out = appendBytes(out, 3, l.Signer)
	return appendString(out, 4, l.Error)
}

func UnmarshalLoginResult(data []byte) (LoginResult, error) {
	r, err := newReader(data)
	if err != nil {
		return LoginResult{}, err
	}
	l := LoginResult{OK: r.boolean(1), User: r.raw(2), Signer: r.raw(3), Error: r.str(4)}
	return l, r.err
}

func (l LoginResult) UserAddress() string   { return hexAddress(l.User) }
func (l LoginResult) SignerAddress() string { return hexAddress(l.Signer) }

func hexAddress(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return fmt.Sprintf("0x%x", b)
}

// ---------------------------------------------------------------- market data

// L2BookFrame: the three arrays of each side share indices (level i).
type L2BookFrame struct {
	Coin       string
	Time       uint64
	PxDecimals uint32
	SzDecimals uint32
	BidPx      []uint64
	BidSz      []uint64
	BidN       []uint32
	AskPx      []uint64
	AskSz      []uint64
	AskN       []uint32
}

func (b L2BookFrame) Marshal() []byte {
	out := appendString(nil, 1, b.Coin)
	out = appendVarint(out, 2, b.Time)
	out = appendVarint(out, 3, uint64(b.PxDecimals))
	out = appendVarint(out, 4, uint64(b.SzDecimals))
	out = appendPacked64(out, 5, b.BidPx)
	out = appendPacked64(out, 6, b.BidSz)
	out = appendPacked32(out, 7, b.BidN)
	out = appendPacked64(out, 8, b.AskPx)
	out = appendPacked64(out, 9, b.AskSz)
	return appendPacked32(out, 10, b.AskN)
}

func UnmarshalL2Book(data []byte) (L2BookFrame, error) {
	r, err := newReader(data)
	if err != nil {
		return L2BookFrame{}, err
	}
	b := L2BookFrame{
		Coin: r.str(1), Time: r.u64(2), PxDecimals: r.u32(3), SzDecimals: r.u32(4),
		BidPx: r.rep64(5), BidSz: r.rep64(6), BidN: r.rep32(7),
		AskPx: r.rep64(8), AskSz: r.rep64(9), AskN: r.rep32(10),
	}
	if r.err != nil {
		return L2BookFrame{}, r.err
	}
	if len(b.BidPx) != len(b.BidSz) || len(b.AskPx) != len(b.AskSz) {
		return L2BookFrame{}, fmt.Errorf("l2 book %s: px/sz arrays differ in length", b.Coin)
	}
	if (b.BidN != nil && len(b.BidN) != len(b.BidPx)) || (b.AskN != nil && len(b.AskN) != len(b.AskPx)) {
		return L2BookFrame{}, fmt.Errorf("l2 book %s: n array differs in length", b.Coin)
	}
	return b, nil
}

// L2DeltaUpdate changes the previous book; see Book.ApplyDelta for the rules.
type L2DeltaUpdate struct {
	Coin       string
	Time       uint64
	PxDecimals uint32
	SzDecimals uint32
	BidPx      []uint64 // new or size-changed levels
	BidSz      []uint64
	AskPx      []uint64
	AskSz      []uint64
	BidRemoved []uint32 // indices into the previous book, removed as one set
	AskRemoved []uint32
	Spread     *uint64
}

func (u L2DeltaUpdate) Marshal() []byte {
	out := appendString(nil, 1, u.Coin)
	out = appendVarint(out, 2, u.Time)
	out = appendVarint(out, 3, uint64(u.PxDecimals))
	out = appendVarint(out, 4, uint64(u.SzDecimals))
	out = appendPacked64(out, 5, u.BidPx)
	out = appendPacked64(out, 6, u.BidSz)
	out = appendPacked64(out, 7, u.AskPx)
	out = appendPacked64(out, 8, u.AskSz)
	out = appendPacked32(out, 9, u.BidRemoved)
	out = appendPacked32(out, 10, u.AskRemoved)
	if u.Spread != nil {
		out = appendVarintAlways(out, 11, *u.Spread)
	}
	return out
}

func UnmarshalL2DeltaUpdate(data []byte) (L2DeltaUpdate, error) {
	r, err := newReader(data)
	if err != nil {
		return L2DeltaUpdate{}, err
	}
	u := L2DeltaUpdate{
		Coin: r.str(1), Time: r.u64(2), PxDecimals: r.u32(3), SzDecimals: r.u32(4),
		BidPx: r.rep64(5), BidSz: r.rep64(6), AskPx: r.rep64(7), AskSz: r.rep64(8),
		BidRemoved: r.rep32(9), AskRemoved: r.rep32(10), Spread: r.opt64(11),
	}
	if r.err != nil {
		return L2DeltaUpdate{}, r.err
	}
	if len(u.BidPx) != len(u.BidSz) || len(u.AskPx) != len(u.AskSz) {
		return L2DeltaUpdate{}, fmt.Errorf("l2 update %s: px/sz arrays differ in length", u.Coin)
	}
	return u, nil
}

// L2DeltaFrame is a oneof: exactly one of Snapshot and Update is set.
type L2DeltaFrame struct {
	Snapshot *L2BookFrame
	Update   *L2DeltaUpdate
}

func (d L2DeltaFrame) Marshal() []byte {
	if d.Snapshot != nil {
		return appendBytesAlways(nil, 1, d.Snapshot.Marshal())
	}
	if d.Update != nil {
		return appendBytesAlways(nil, 2, d.Update.Marshal())
	}
	return nil
}

func UnmarshalL2Delta(data []byte) (L2DeltaFrame, error) {
	m, err := parse(data)
	if err != nil {
		return L2DeltaFrame{}, err
	}
	switch {
	case m.has(1) && m.has(2):
		return L2DeltaFrame{}, errors.New("l2 delta has both snapshot and update")
	case m.has(1):
		raw, err := m.bytes(1)
		if err != nil {
			return L2DeltaFrame{}, err
		}
		b, err := UnmarshalL2Book(raw)
		return L2DeltaFrame{Snapshot: &b}, err
	case m.has(2):
		raw, err := m.bytes(2)
		if err != nil {
			return L2DeltaFrame{}, err
		}
		u, err := UnmarshalL2DeltaUpdate(raw)
		return L2DeltaFrame{Update: &u}, err
	}
	return L2DeltaFrame{}, errors.New("l2 delta without snapshot or update")
}

// BboFrame: a side with no level leaves all three of its fields absent (nil).
type BboFrame struct {
	Coin       string
	Time       uint64
	PxDecimals uint32
	SzDecimals uint32
	BidPx      *uint64
	BidSz      *uint64
	BidN       *uint32
	AskPx      *uint64
	AskSz      *uint64
	AskN       *uint32
}

func (b BboFrame) Marshal() []byte {
	out := appendString(nil, 1, b.Coin)
	out = appendVarint(out, 2, b.Time)
	out = appendVarint(out, 3, uint64(b.PxDecimals))
	out = appendVarint(out, 4, uint64(b.SzDecimals))
	for _, f := range []struct {
		n int
		v *uint64
	}{{5, b.BidPx}, {6, b.BidSz}, {8, b.AskPx}, {9, b.AskSz}} {
		if f.v != nil {
			out = appendVarintAlways(out, f.n, *f.v)
		}
	}
	if b.BidN != nil {
		out = appendVarintAlways(out, 7, uint64(*b.BidN))
	}
	if b.AskN != nil {
		out = appendVarintAlways(out, 10, uint64(*b.AskN))
	}
	return out
}

func UnmarshalBBO(data []byte) (BboFrame, error) {
	r, err := newReader(data)
	if err != nil {
		return BboFrame{}, err
	}
	b := BboFrame{
		Coin: r.str(1), Time: r.u64(2), PxDecimals: r.u32(3), SzDecimals: r.u32(4),
		BidPx: r.opt64(5), BidSz: r.opt64(6), BidN: r.opt32(7),
		AskPx: r.opt64(8), AskSz: r.opt64(9), AskN: r.opt32(10),
	}
	return b, r.err
}

type CandleFrame struct {
	Coin       string
	Interval   string
	OpenTime   uint64
	CloseTime  uint64
	PxDecimals uint32
	SzDecimals uint32
	Open       uint64
	Close      uint64
	High       uint64
	Low        uint64
	Volume     uint64 // scaled by SzDecimals
	TradeCount uint32
}

func (c CandleFrame) Marshal() []byte {
	out := appendString(nil, 1, c.Coin)
	out = appendString(out, 2, c.Interval)
	out = appendVarint(out, 3, c.OpenTime)
	out = appendVarint(out, 4, c.CloseTime)
	out = appendVarint(out, 5, uint64(c.PxDecimals))
	out = appendVarint(out, 6, uint64(c.SzDecimals))
	out = appendVarint(out, 7, c.Open)
	out = appendVarint(out, 8, c.Close)
	out = appendVarint(out, 9, c.High)
	out = appendVarint(out, 10, c.Low)
	out = appendVarint(out, 11, c.Volume)
	return appendVarint(out, 12, uint64(c.TradeCount))
}

func UnmarshalCandle(data []byte) (CandleFrame, error) {
	r, err := newReader(data)
	if err != nil {
		return CandleFrame{}, err
	}
	c := CandleFrame{
		Coin: r.str(1), Interval: r.str(2), OpenTime: r.u64(3), CloseTime: r.u64(4),
		PxDecimals: r.u32(5), SzDecimals: r.u32(6),
		Open: r.u64(7), Close: r.u64(8), High: r.u64(9), Low: r.u64(10),
		Volume: r.u64(11), TradeCount: r.u32(12),
	}
	return c, r.err
}

type Trade struct {
	Buy    bool // aggressor bought
	Px     uint64
	Sz     uint64
	Time   uint64
	TID    uint64
	Hash   []byte
	Buyer  []byte // 20 bytes
	Seller []byte // 20 bytes
}

func (t Trade) Marshal() []byte {
	out := appendBool(nil, 1, t.Buy)
	out = appendVarint(out, 2, t.Px)
	out = appendVarint(out, 3, t.Sz)
	out = appendVarint(out, 4, t.Time)
	out = appendVarint(out, 5, t.TID)
	out = appendBytes(out, 6, t.Hash)
	out = appendBytes(out, 7, t.Buyer)
	return appendBytes(out, 8, t.Seller)
}

func unmarshalTrade(data []byte) (Trade, error) {
	r, err := newReader(data)
	if err != nil {
		return Trade{}, err
	}
	t := Trade{Buy: r.boolean(1), Px: r.u64(2), Sz: r.u64(3), Time: r.u64(4), TID: r.u64(5),
		Hash: r.raw(6), Buyer: r.raw(7), Seller: r.raw(8)}
	return t, r.err
}

// TradesFrame carries several trades of one coin.
type TradesFrame struct {
	Coin       string
	PxDecimals uint32
	SzDecimals uint32
	Trades     []Trade
}

func (t TradesFrame) Marshal() []byte {
	out := appendString(nil, 1, t.Coin)
	out = appendVarint(out, 2, uint64(t.PxDecimals))
	out = appendVarint(out, 3, uint64(t.SzDecimals))
	for _, trade := range t.Trades {
		out = appendBytesAlways(out, 4, trade.Marshal())
	}
	return out
}

func UnmarshalTrades(data []byte) (TradesFrame, error) {
	r, err := newReader(data)
	if err != nil {
		return TradesFrame{}, err
	}
	t := TradesFrame{Coin: r.str(1), PxDecimals: r.u32(2), SzDecimals: r.u32(3)}
	for _, raw := range r.msgs(4) {
		trade, err := unmarshalTrade(raw)
		if err != nil {
			return TradesFrame{}, fmt.Errorf("trade: %w", err)
		}
		t.Trades = append(t.Trades, trade)
	}
	return t, r.err
}

// ---------------------------------------------------------------- fixed point

// FormatFixed renders a fixed-point integer as a decimal string:
// value = v / 10^decimals, e.g. FormatFixed(1010, 1) == "101.0". It is exact
// (string arithmetic, no floating point).
func FormatFixed(v uint64, decimals uint32) string {
	s := strconv.FormatUint(v, 10)
	if decimals == 0 {
		return s
	}
	d := int(decimals)
	if len(s) <= d {
		s = strings.Repeat("0", d-len(s)+1) + s
	}
	return s[:len(s)-d] + "." + s[len(s)-d:]
}
