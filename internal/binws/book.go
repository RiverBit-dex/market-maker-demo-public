package binws

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// Level is one price level. Px/Sz are exact; PxText/SzText keep the wire rendering.
type Level struct {
	Px, Sz         *big.Rat
	PxText, SzText string
}

// Book replays one `l2` subscription (bin-ws-api.md §6). Typed L2DeltaFrame
// and the JSON fallback (channel "l2") act on the same state.
type Book struct {
	Coin       string
	Bids, Asks []Level // bids descending, asks ascending
	PxDecimals uint32
	SzDecimals uint32
	// Locked is true after a typed snapshot: typed updates must then carry the
	// same decimals. A JSON snapshot has no decimals and unlocks the book.
	Locked  bool
	HasBook bool
	Time    uint64
	// LastSpread is the optional spread carried by the latest update, if any.
	LastSpread *big.Rat
}

func NewBook(coin string) *Book { return &Book{Coin: coin} }

// Best returns the best bid and ask, nil for an empty side.
func (b *Book) Best() (bid, ask *Level) {
	if len(b.Bids) > 0 {
		bid = &b.Bids[0]
	}
	if len(b.Asks) > 0 {
		ask = &b.Asks[0]
	}
	return bid, ask
}

// ApplyDelta applies one typed L2DeltaFrame.
func (b *Book) ApplyDelta(d L2DeltaFrame) error {
	switch {
	case d.Snapshot != nil:
		return b.applySnapshot(*d.Snapshot)
	case d.Update != nil:
		return b.applyUpdate(*d.Update)
	}
	return fmt.Errorf("l2 delta without snapshot or update")
}

func fixedLevels(px, sz []uint64, pxd, szd uint32) []Level {
	out := make([]Level, len(px))
	for i := range px {
		out[i] = newLevel(FormatFixed(px[i], pxd), FormatFixed(sz[i], szd))
	}
	return out
}

func newLevel(px, sz string) Level {
	p, _ := new(big.Rat).SetString(px)
	s, _ := new(big.Rat).SetString(sz)
	return Level{Px: p, Sz: s, PxText: px, SzText: sz}
}

// Rule 1: a snapshot replaces the whole book and its decimals become the reference.
func (b *Book) applySnapshot(s L2BookFrame) error {
	if b.Coin != "" && s.Coin != b.Coin {
		return fmt.Errorf("l2 snapshot for %q, subscribed %q", s.Coin, b.Coin)
	}
	if len(s.BidPx) != len(s.BidSz) || len(s.AskPx) != len(s.AskSz) {
		return fmt.Errorf("l2 snapshot px/sz arrays differ in length")
	}
	bids := fixedLevels(s.BidPx, s.BidSz, s.PxDecimals, s.SzDecimals)
	asks := fixedLevels(s.AskPx, s.AskSz, s.PxDecimals, s.SzDecimals)
	if err := checkOrdered(bids, asks); err != nil {
		return err
	}
	b.Bids, b.Asks = bids, asks
	b.PxDecimals, b.SzDecimals, b.Locked, b.HasBook, b.Time = s.PxDecimals, s.SzDecimals, true, true, s.Time
	b.LastSpread = nil
	return nil
}

func (b *Book) applyUpdate(u L2DeltaUpdate) error {
	if !b.HasBook {
		return fmt.Errorf("l2 update before any snapshot")
	}
	if b.Coin != "" && u.Coin != b.Coin {
		return fmt.Errorf("l2 update for %q, subscribed %q", u.Coin, b.Coin)
	}
	if !b.Locked {
		return fmt.Errorf("typed l2 update on a book built from a JSON snapshot")
	}
	if u.PxDecimals != b.PxDecimals || u.SzDecimals != b.SzDecimals {
		return fmt.Errorf("l2 update decimals (%d,%d) differ from the snapshot's (%d,%d)",
			u.PxDecimals, u.SzDecimals, b.PxDecimals, b.SzDecimals)
	}
	if len(u.BidPx) != len(u.BidSz) || len(u.AskPx) != len(u.AskSz) {
		return fmt.Errorf("l2 update px/sz arrays differ in length")
	}
	changed := [2][]Level{
		fixedLevels(u.BidPx, u.BidSz, u.PxDecimals, u.SzDecimals),
		fixedLevels(u.AskPx, u.AskSz, u.PxDecimals, u.SzDecimals),
	}
	if err := b.apply(changed, [2][]uint32{u.BidRemoved, u.AskRemoved}); err != nil {
		return err
	}
	b.Time = u.Time
	b.LastSpread = nil
	if u.Spread != nil {
		b.LastSpread = newLevel(FormatFixed(*u.Spread, u.PxDecimals), "0").Px
	}
	return nil
}

// apply implements rules 2-4. It computes both sides first and only then
// commits, so an invalid update leaves the book untouched.
func (b *Book) apply(changed [2][]Level, removed [2][]uint32) error {
	previous := [2][]Level{b.Bids, b.Asks}
	var next [2][]Level
	for side := 0; side < 2; side++ {
		prev := previous[side]
		// Rule 2: indices refer to the previous book and are removed as one set.
		drop := make(map[uint32]bool, len(removed[side]))
		for _, i := range removed[side] {
			if int(i) >= len(prev) {
				return fmt.Errorf("removed index %d outside the previous book (%d levels)", i, len(prev))
			}
			drop[i] = true
		}
		levels := make([]Level, 0, len(prev)+len(changed[side]))
		for i, lvl := range prev {
			if !drop[uint32(i)] {
				levels = append(levels, lvl)
			}
		}
		// Rule 3: overwrite by price, or insert.
		for _, lvl := range changed[side] {
			replaced := false
			for i := range levels {
				if levels[i].Px.Cmp(lvl.Px) == 0 {
					levels[i] = lvl
					replaced = true
					break
				}
			}
			if !replaced {
				levels = append(levels, lvl)
			}
		}
		// Rule 4: re-sort, bids descending, asks ascending.
		desc := side == 0
		sort.SliceStable(levels, func(i, j int) bool {
			c := levels[i].Px.Cmp(levels[j].Px)
			if desc {
				return c > 0
			}
			return c < 0
		})
		next[side] = levels
	}
	if err := checkOrdered(next[0], next[1]); err != nil {
		return err
	}
	b.Bids, b.Asks = next[0], next[1]
	return nil
}

func checkOrdered(bids, asks []Level) error {
	for side, levels := range [2][]Level{bids, asks} {
		for i := 1; i < len(levels); i++ {
			c := levels[i-1].Px.Cmp(levels[i].Px)
			if (side == 0 && c <= 0) || (side == 1 && c >= 0) {
				name := "bid"
				if side == 1 {
					name = "ask"
				}
				return fmt.Errorf("%s side is not strictly ordered at level %d (%s, %s)", name, i, levels[i-1].PxText, levels[i].PxText)
			}
		}
	}
	return nil
}

// ApplyJSON applies the JSON fallback of an `l2` subscription: data is the
// "data" member of a {"channel":"l2"} frame, either {"s": <L2Book>} or
// {"u": {"c":..,"t":..,"l":[[{p,s}..],[..]],"r":[[idx..],[..]],"s":spread}}.
func (b *Book) ApplyJSON(data json.RawMessage) error {
	var frame struct {
		Snapshot *struct {
			Coin   string `json:"coin"`
			Time   uint64 `json:"time"`
			Levels [2][]struct {
				Px string `json:"px"`
				Sz string `json:"sz"`
			} `json:"levels"`
		} `json:"s"`
		Update *struct {
			Coin   string `json:"c"`
			Time   uint64 `json:"t"`
			Levels [2][]struct {
				P string `json:"p"`
				S string `json:"s"`
			} `json:"l"`
			Removed [2][]uint32     `json:"r"`
			Spread  json.RawMessage `json:"s"`
		} `json:"u"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return fmt.Errorf("l2 JSON frame: %w", err)
	}
	switch {
	case frame.Snapshot != nil:
		s := frame.Snapshot
		if b.Coin != "" && s.Coin != "" && s.Coin != b.Coin {
			return fmt.Errorf("l2 JSON snapshot for %q, subscribed %q", s.Coin, b.Coin)
		}
		var sides [2][]Level
		for side := 0; side < 2; side++ {
			for _, l := range s.Levels[side] {
				lvl, err := parseLevel(l.Px, l.Sz)
				if err != nil {
					return err
				}
				sides[side] = append(sides[side], lvl)
			}
		}
		if err := checkOrdered(sides[0], sides[1]); err != nil {
			return err
		}
		b.Bids, b.Asks, b.Locked, b.HasBook, b.Time, b.LastSpread = sides[0], sides[1], false, true, s.Time, nil
		return nil
	case frame.Update != nil:
		u := frame.Update
		if !b.HasBook {
			return fmt.Errorf("l2 JSON update before any snapshot")
		}
		if b.Coin != "" && u.Coin != "" && u.Coin != b.Coin {
			return fmt.Errorf("l2 JSON update for %q, subscribed %q", u.Coin, b.Coin)
		}
		var changed [2][]Level
		for side := 0; side < 2; side++ {
			for _, l := range u.Levels[side] {
				lvl, err := parseLevel(l.P, l.S)
				if err != nil {
					return err
				}
				changed[side] = append(changed[side], lvl)
			}
		}
		if err := b.apply(changed, u.Removed); err != nil {
			return err
		}
		b.Time, b.LastSpread = u.Time, nil
		if text := strings.Trim(string(u.Spread), `"`); text != "" && text != "null" {
			if spread, ok := new(big.Rat).SetString(text); ok {
				b.LastSpread = spread
			}
		}
		return nil
	}
	return fmt.Errorf("l2 JSON frame without s or u")
}

func parseLevel(px, sz string) (Level, error) {
	p, ok1 := new(big.Rat).SetString(px)
	s, ok2 := new(big.Rat).SetString(sz)
	if !ok1 || !ok2 {
		return Level{}, fmt.Errorf("invalid l2 level px=%q sz=%q", px, sz)
	}
	return Level{Px: p, Sz: s, PxText: px, SzText: sz}, nil
}
