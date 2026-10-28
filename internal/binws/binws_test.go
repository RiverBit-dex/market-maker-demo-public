package binws

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// testSigner is a throwaway key (never a real wallet).
func testSigner(t *testing.T) (Signer, string) {
	t.Helper()
	key, err := crypto.HexToECDSA(strings.Repeat("44", 32))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(msg []byte) ([]byte, error) {
		sig, err := crypto.Sign(PersonalHash(msg), key)
		if err != nil {
			return nil, err
		}
		sig[64] += 27
		return sig, nil
	}
	return sign, strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
}

func TestLoginMessageFormat(t *testing.T) {
	nonce := bytes.Repeat([]byte{0xab}, 16)
	got := LoginMessage("pre-api.riverbit.io", 1760000000123, nonce)
	want := "riverbit-ws-v2-login\nhost:pre-api.riverbit.io\ntimestamp:1760000000123\nnonce:abababababababababababababababab"
	if got != want {
		t.Fatalf("login message\n got %q\nwant %q", got, want)
	}
	if strings.HasSuffix(got, "\n") || strings.Count(got, "\n") != 3 {
		t.Fatal("login message must be exactly four lines without a trailing newline")
	}
}

func TestPersonalHashMatchesEIP191Vector(t *testing.T) {
	// hashMessage("hello") as computed by ethers / web3 / eth-account.
	const want = "50b2c43fd39106bafbba0da34fc430e1f91e3c96ea2acee2bc34119f92b37750"
	if got := hex.EncodeToString(PersonalHash([]byte("hello"))); got != want {
		t.Fatalf("personal hash = %s, want %s", got, want)
	}
}

func TestLoginSignatureRecoversSigner(t *testing.T) {
	sign, address := testSigner(t)
	now := time.UnixMilli(1760000000123)
	login, err := NewLogin("pre-api.riverbit.io", now, bytes.NewReader(bytes.Repeat([]byte{7}, 16)), sign)
	if err != nil {
		t.Fatal(err)
	}
	if login.TimestampMs != 1760000000123 || !bytes.Equal(login.Nonce, bytes.Repeat([]byte{7}, 16)) {
		t.Fatalf("login = %+v", login)
	}
	if len(login.Signature) != 65 || (login.Signature[64] != 27 && login.Signature[64] != 28) {
		t.Fatalf("signature shape: %x", login.Signature)
	}
	got, err := RecoverLoginSigner("pre-api.riverbit.io", login)
	if err != nil || got != address {
		t.Fatalf("recovered %s (%v), want %s", got, err, address)
	}
	// v = 0/1 is accepted too.
	low := login
	low.Signature = append([]byte(nil), login.Signature...)
	low.Signature[64] -= 27
	if got, err := RecoverLoginSigner("pre-api.riverbit.io", low); err != nil || got != address {
		t.Fatalf("v=0/1: recovered %s (%v)", got, err)
	}
	// The host is bound into the signature.
	if got, _ := RecoverLoginSigner("127.0.0.1:18081", login); got == address {
		t.Fatal("signature for one host must not recover the signer under another host")
	}
	// Encoded login survives the client frame round trip.
	frame, err := UnmarshalClientFrame(ClientFrame{Body: ClientLogin, Data: login.Marshal()}.Marshal())
	if err != nil || frame.Body != ClientLogin || frame.ID != 0 {
		t.Fatalf("frame = %+v, %v", frame, err)
	}
	back, err := UnmarshalLogin(frame.Data)
	if err != nil || !reflect.DeepEqual(back, login) {
		t.Fatalf("login round trip = %+v, %v", back, err)
	}
}

func TestClientFrameWireBytes(t *testing.T) {
	sub := []byte(`{"type":"l2","c":"RBT"}`)
	got := ClientFrame{Body: ClientSubscribe, Data: sub}.Marshal()
	want := append([]byte{0x22, byte(len(sub))}, sub...) // id 0 omitted; field 4, wire 2
	if !bytes.Equal(got, want) {
		t.Fatalf("subscribe frame = %x, want %x", got, want)
	}
	if got := (ClientFrame{Body: ClientPing}).Marshal(); !bytes.Equal(got, []byte{0x32, 0x00}) {
		t.Fatalf("ping frame = %x, want 3200", got)
	}
	action := []byte(`{"action":{}}`)
	got = ClientFrame{ID: 300, Body: ClientAction, Data: action}.Marshal()
	want = append([]byte{0x08, 0xac, 0x02, 0x12, byte(len(action))}, action...)
	if !bytes.Equal(got, want) {
		t.Fatalf("action frame = %x, want %x", got, want)
	}
	back, err := UnmarshalClientFrame(got)
	if err != nil || back.ID != 300 || back.Body != ClientAction || !bytes.Equal(back.Data, action) {
		t.Fatalf("action round trip = %+v, %v", back, err)
	}
	if _, err := UnmarshalClientFrame([]byte{0x08, 0x01}); err == nil {
		t.Fatal("frame without body must fail")
	}
}

func u64(v uint64) *uint64 { return &v }
func u32(v uint32) *uint32 { return &v }

func TestServerFrameRoundTrips(t *testing.T) {
	book := L2BookFrame{Coin: "RBT", Time: 1760000000000, PxDecimals: 4, SzDecimals: 2,
		BidPx: []uint64{1000000, 999000}, BidSz: []uint64{10, 250}, BidN: []uint32{1, 3},
		AskPx: []uint64{1001000}, AskSz: []uint64{70}, AskN: []uint32{2}}
	update := L2DeltaUpdate{Coin: "RBT", Time: 2, PxDecimals: 4, SzDecimals: 2,
		BidPx: []uint64{999500}, BidSz: []uint64{5}, BidRemoved: []uint32{1}, AskRemoved: []uint32{0}, Spread: u64(0)}
	bbo := BboFrame{Coin: "RBT", Time: 3, PxDecimals: 4, SzDecimals: 2, BidPx: u64(1000000), BidSz: u64(10), BidN: u32(1)}
	candle := CandleFrame{Coin: "RBT", Interval: "1m", OpenTime: 60000, CloseTime: 119999, PxDecimals: 1, SzDecimals: 3,
		Open: 1000, Close: 1005, High: 1010, Low: 995, Volume: 12345, TradeCount: 7}
	trades := TradesFrame{Coin: "RBT", PxDecimals: 4, SzDecimals: 2, Trades: []Trade{
		{Buy: true, Px: 1000500, Sz: 3, Time: 5, TID: 9, Hash: bytes.Repeat([]byte{1}, 32), Buyer: bytes.Repeat([]byte{2}, 20), Seller: bytes.Repeat([]byte{3}, 20)},
		{Px: 1000400, Sz: 1, Time: 6, TID: 10},
	}}
	login := LoginResult{OK: true, User: bytes.Repeat([]byte{0xaa}, 20), Signer: bytes.Repeat([]byte{0xbb}, 20)}

	roundTrip := func(name string, kind ServerKind, body []byte, id uint64) []byte {
		t.Helper()
		f, err := UnmarshalServerFrame(ServerFrame{ID: id, Kind: kind, Data: body}.Marshal())
		if err != nil || f.Kind != kind || f.ID != id || !bytes.Equal(f.Data, body) {
			t.Fatalf("%s: server frame round trip = %+v, %v", name, f, err)
		}
		return f.Data
	}

	if got, err := UnmarshalL2Book(roundTrip("l2Book", KindL2Book, book.Marshal(), 0)); err != nil || !reflect.DeepEqual(got, book) {
		t.Fatalf("l2Book = %+v, %v", got, err)
	}
	for _, d := range []L2DeltaFrame{{Snapshot: &book}, {Update: &update}} {
		got, err := UnmarshalL2Delta(roundTrip("l2Delta", KindL2Delta, d.Marshal(), 0))
		if err != nil || !reflect.DeepEqual(got, d) {
			t.Fatalf("l2Delta = %+v, %v; want %+v", got, err, d)
		}
	}
	if got, err := UnmarshalBBO(roundTrip("bbo", KindBBO, bbo.Marshal(), 0)); err != nil || !reflect.DeepEqual(got, bbo) {
		t.Fatalf("bbo = %+v, %v", got, err)
	} else if got.AskPx != nil || got.AskSz != nil || got.AskN != nil {
		t.Fatal("empty ask side must stay absent")
	}
	if got, err := UnmarshalCandle(roundTrip("candle", KindCandle, candle.Marshal(), 0)); err != nil || !reflect.DeepEqual(got, candle) {
		t.Fatalf("candle = %+v, %v", got, err)
	} else if FormatFixed(got.High, got.PxDecimals) != "101.0" || FormatFixed(got.Volume, got.SzDecimals) != "12.345" {
		t.Fatalf("candle decimals: high=%s volume=%s", FormatFixed(got.High, got.PxDecimals), FormatFixed(got.Volume, got.SzDecimals))
	}
	if got, err := UnmarshalTrades(roundTrip("trades", KindTrades, trades.Marshal(), 0)); err != nil || !reflect.DeepEqual(got, trades) {
		t.Fatalf("trades = %+v, %v", got, err)
	}
	if got, err := UnmarshalLoginResult(roundTrip("login", KindLogin, login.Marshal(), 0)); err != nil || !reflect.DeepEqual(got, login) {
		t.Fatalf("login result = %+v, %v", got, err)
	} else if got.UserAddress() != "0x"+strings.Repeat("aa", 20) {
		t.Fatalf("user = %s", got.UserAddress())
	}
	roundTrip("json", KindJSON, []byte(`{"channel":"post","data":{"id":9}}`), 9)
}

func TestRepeatedFieldsAcceptUnpackedEncoding(t *testing.T) {
	// Unpacked repeated varints (one key per element) are valid Protobuf too.
	var raw []byte
	raw = appendString(raw, 1, "RBT")
	raw = appendVarint(raw, 3, 1)
	raw = appendVarintAlways(raw, 5, 1000)
	raw = appendVarintAlways(raw, 5, 990)
	raw = appendVarintAlways(raw, 6, 2)
	raw = appendVarintAlways(raw, 6, 3)
	b, err := UnmarshalL2Book(raw)
	if err != nil || !reflect.DeepEqual(b.BidPx, []uint64{1000, 990}) || !reflect.DeepEqual(b.BidSz, []uint64{2, 3}) {
		t.Fatalf("book = %+v, %v", b, err)
	}
}

func TestDecodeRejectsTruncatedFrames(t *testing.T) {
	full := ServerFrame{Kind: KindJSON, Data: []byte(`{"channel":"pong"}`)}.Marshal()
	if _, err := UnmarshalServerFrame(full[:len(full)-1]); err == nil {
		t.Fatal("truncated frame must fail")
	}
	if _, err := UnmarshalServerFrame([]byte{0x08, 0x01}); err == nil {
		t.Fatal("frame without body must fail")
	}
}

func TestFormatFixed(t *testing.T) {
	for _, c := range []struct {
		v    uint64
		d    uint32
		want string
	}{{1010, 1, "101.0"}, {5, 3, "0.005"}, {12345, 3, "12.345"}, {0, 2, "0.00"}, {42, 0, "42"}, {100, 2, "1.00"}} {
		if got := FormatFixed(c.v, c.d); got != c.want {
			t.Errorf("FormatFixed(%d,%d) = %s, want %s", c.v, c.d, got, c.want)
		}
	}
}

func levels(ls []Level) [][2]string {
	out := make([][2]string, len(ls))
	for i, l := range ls {
		out[i] = [2]string{l.Px.RatString(), l.Sz.RatString()}
	}
	return out
}

// Same vector as riverbit-ws scripts/e2e-bin-ws.py self_test (and book/encoding.rs).
func TestL2ReplayTypedAndJSON(t *testing.T) {
	snap := L2BookFrame{Coin: "BTC", Time: 1, PxDecimals: 1, SzDecimals: 3,
		BidPx: []uint64{1000, 990}, BidSz: []uint64{2000, 3000}, BidN: []uint32{1, 1},
		AskPx: []uint64{1010, 1020}, AskSz: []uint64{4000, 5000}, AskN: []uint32{1, 1}}
	book := NewBook("BTC")
	if err := book.ApplyDelta(mustDelta(t, L2DeltaFrame{Snapshot: &snap})); err != nil {
		t.Fatal(err)
	}
	// Remove bid[1] (99.0) and ask[0] (101.0) of the previous book, overwrite
	// 100.0, insert 101.0 bid and 103.0 ask.
	upd := L2DeltaUpdate{Coin: "BTC", Time: 2, PxDecimals: 1, SzDecimals: 3,
		BidPx: []uint64{1010, 1000}, BidSz: []uint64{1000, 2500},
		AskPx: []uint64{1030}, AskSz: []uint64{2000},
		BidRemoved: []uint32{1}, AskRemoved: []uint32{0}, Spread: u64(10)}
	if err := book.ApplyDelta(mustDelta(t, L2DeltaFrame{Update: &upd})); err != nil {
		t.Fatal(err)
	}
	if got, want := levels(book.Bids), [][2]string{{"101", "1"}, {"100", "5/2"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bids = %v, want %v", got, want)
	}
	if got, want := levels(book.Asks), [][2]string{{"102", "5"}, {"103", "2"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("asks = %v, want %v", got, want)
	}
	bid, ask := book.Best()
	if bid.PxText != "101.0" || bid.SzText != "1.000" || ask.PxText != "102.0" || ask.SzText != "5.000" {
		t.Fatalf("best = %+v / %+v", bid, ask)
	}
	if book.LastSpread == nil || book.LastSpread.RatString() != "1" {
		t.Fatalf("spread = %v", book.LastSpread)
	}

	// JSON fallback update on the typed book.
	if err := book.ApplyJSON(json.RawMessage(`{"u":{"c":"BTC","t":3,"l":[[{"p":"98","s":"7"}],[{"p":"101","s":"3"}]],"r":[[0],[0]]}}`)); err != nil {
		t.Fatal(err)
	}
	if got, want := levels(book.Bids), [][2]string{{"100", "5/2"}, {"98", "7"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bids after JSON = %v, want %v", got, want)
	}
	if got, want := levels(book.Asks), [][2]string{{"101", "3"}, {"103", "2"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("asks after JSON = %v, want %v", got, want)
	}

	// A new snapshot replaces everything.
	if err := book.ApplyDelta(L2DeltaFrame{Snapshot: &L2BookFrame{Coin: "BTC", PxDecimals: 2, SzDecimals: 0, AskPx: []uint64{10100}, AskSz: []uint64{4}}}); err != nil {
		t.Fatal(err)
	}
	if len(book.Bids) != 0 || len(book.Asks) != 1 || book.Asks[0].PxText != "101.00" || book.PxDecimals != 2 {
		t.Fatalf("after resnapshot: %+v", book)
	}
}

func mustDelta(t *testing.T, d L2DeltaFrame) L2DeltaFrame {
	t.Helper()
	got, err := UnmarshalL2Delta(d.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestL2ReplayRejectsInvalidUpdates(t *testing.T) {
	snap := L2BookFrame{Coin: "BTC", PxDecimals: 1, SzDecimals: 3, BidPx: []uint64{1000}, BidSz: []uint64{1}, AskPx: []uint64{1010}, AskSz: []uint64{1}}
	book := NewBook("BTC")
	if err := book.ApplyDelta(L2DeltaFrame{Update: &L2DeltaUpdate{Coin: "BTC", PxDecimals: 1, SzDecimals: 3}}); err == nil {
		t.Fatal("update before snapshot must fail")
	}
	if err := book.ApplyDelta(L2DeltaFrame{Snapshot: &snap}); err != nil {
		t.Fatal(err)
	}
	before := levels(book.Bids)
	for name, u := range map[string]L2DeltaUpdate{
		"decimals change": {Coin: "BTC", PxDecimals: 2, SzDecimals: 3},
		"index outside":   {Coin: "BTC", PxDecimals: 1, SzDecimals: 3, BidPx: []uint64{999}, BidSz: []uint64{1}, AskRemoved: []uint32{5}},
		"other coin":      {Coin: "ETH", PxDecimals: 1, SzDecimals: 3},
	} {
		if err := book.ApplyDelta(L2DeltaFrame{Update: &u}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if got := levels(book.Bids); !reflect.DeepEqual(got, before) {
			t.Errorf("%s: failed update changed the book: %v", name, got)
		}
	}
	// After a JSON snapshot the book has no decimals: typed updates need a typed snapshot first.
	if err := book.ApplyJSON(json.RawMessage(`{"s":{"coin":"BTC","time":1,"levels":[[{"px":"100","sz":"1","n":1}],[{"px":"101","sz":"1","n":1}]]}}`)); err != nil {
		t.Fatal(err)
	}
	if err := book.ApplyDelta(L2DeltaFrame{Update: &L2DeltaUpdate{Coin: "BTC", PxDecimals: 1, SzDecimals: 3}}); err == nil {
		t.Fatal("typed update on a JSON-snapshot book must fail")
	}
}

func TestParsePostReplyMatchesID(t *testing.T) {
	reply := func(id uint64, body string) ServerFrame {
		return ServerFrame{ID: id, Kind: KindJSON, Data: []byte(body)}
	}
	payload, err := ParsePostReply(reply(5, `{"channel":"post","data":{"id":5,"response":{"type":"action","payload":{"status":"ok"}}}}`), 5, "action")
	if err != nil || string(payload) != `{"status":"ok"}` {
		t.Fatalf("payload = %s, %v", payload, err)
	}
	if _, err := ParsePostReply(reply(6, `{"channel":"post","data":{"id":6,"response":{"type":"action","payload":{}}}}`), 5, "action"); err == nil {
		t.Fatal("outer id mismatch must fail")
	}
	if _, err := ParsePostReply(reply(5, `{"channel":"post","data":{"id":4,"response":{"type":"action","payload":{}}}}`), 5, "action"); err == nil {
		t.Fatal("inner id mismatch must fail")
	}
	if _, err := ParsePostReply(reply(5, `{"channel":"post","data":{"id":5,"response":{"type":"info","payload":{}}}}`), 5, "action"); err == nil {
		t.Fatal("wrong response type must fail")
	}
	_, err = ParsePostReply(reply(5, `{"channel":"post","data":{"id":5,"response":{"type":"error","payload":"429 Too Many Requests"}}}`), 5, "action")
	var replyErr *PostReplyError
	if !asPostReplyError(err, &replyErr) || replyErr.Payload != "429 Too Many Requests" {
		t.Fatalf("error reply = %v", err)
	}
}

func asPostReplyError(err error, target **PostReplyError) bool {
	e, ok := err.(*PostReplyError)
	if ok {
		*target = e
	}
	return ok
}
