// Package binws implements the client side of RiverBit bin-ws: the binary
// WebSocket at wss://<host>/bin-ws where every message is one Protobuf frame
// (WsV2ClientFrame from the client, WsV2ServerFrame from the server).
//
// The protocol reference is riverbit-ws docs/bin-ws-api.md and
// server/proto/api.proto. The Protobuf wire format is hand-written here (only
// varint and length-delimited fields are needed) so the demo does not depend on
// protoc-generated code.
package binws

import (
	"errors"
	"fmt"
	"io"
)

// ---------------------------------------------------------------- encoding

func appendUvarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func appendKey(dst []byte, field int, wire uint8) []byte {
	return appendUvarint(dst, uint64(field)<<3|uint64(wire))
}

// appendVarint writes a varint field. Proto3 omits zero scalars, and so does this.
func appendVarint(dst []byte, field int, v uint64) []byte {
	if v == 0 {
		return dst
	}
	return appendVarintAlways(dst, field, v)
}

// appendVarintAlways writes a varint field even when it is zero (proto3 `optional`).
func appendVarintAlways(dst []byte, field int, v uint64) []byte {
	return appendUvarint(appendKey(dst, field, 0), v)
}

func appendBool(dst []byte, field int, v bool) []byte {
	if !v {
		return dst
	}
	return appendVarintAlways(dst, field, 1)
}

// appendBytesAlways writes a length-delimited field even when empty (oneof members,
// whose presence is the information, e.g. an empty WsV2Ping).
func appendBytesAlways(dst []byte, field int, b []byte) []byte {
	dst = appendKey(dst, field, 2)
	dst = appendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

func appendBytes(dst []byte, field int, b []byte) []byte {
	if len(b) == 0 {
		return dst
	}
	return appendBytesAlways(dst, field, b)
}

func appendString(dst []byte, field int, s string) []byte {
	return appendBytes(dst, field, []byte(s))
}

func appendPacked64(dst []byte, field int, values []uint64) []byte {
	if len(values) == 0 {
		return dst
	}
	var body []byte
	for _, v := range values {
		body = appendUvarint(body, v)
	}
	return appendBytesAlways(dst, field, body)
}

func appendPacked32(dst []byte, field int, values []uint32) []byte {
	wide := make([]uint64, len(values))
	for i, v := range values {
		wide[i] = uint64(v)
	}
	return appendPacked64(dst, field, wide)
}

// ---------------------------------------------------------------- decoding

func readUvarint(data []byte, offset *int) (uint64, error) {
	var v uint64
	for shift := uint(0); shift < 64; shift += 7 {
		if *offset >= len(data) {
			return 0, io.ErrUnexpectedEOF
		}
		b := data[*offset]
		*offset++
		v |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return v, nil
		}
	}
	return 0, errors.New("invalid protobuf varint")
}

type field struct {
	wire  uint8
	value uint64 // wire type 0
	bytes []byte // wire type 2
}

// message is one decoded Protobuf message: field number -> occurrences in wire order.
type message map[int][]field

func parse(data []byte) (message, error) {
	m := message{}
	for offset := 0; offset < len(data); {
		key, err := readUvarint(data, &offset)
		if err != nil {
			return nil, err
		}
		number, wire := int(key>>3), uint8(key&7)
		if number == 0 {
			return nil, errors.New("protobuf field number 0")
		}
		f := field{wire: wire}
		switch wire {
		case 0:
			if f.value, err = readUvarint(data, &offset); err != nil {
				return nil, err
			}
		case 1:
			if len(data)-offset < 8 {
				return nil, io.ErrUnexpectedEOF
			}
			offset += 8 // fixed64: not used by bin-ws, skipped
			continue
		case 2:
			n, err := readUvarint(data, &offset)
			if err != nil {
				return nil, err
			}
			if n > uint64(len(data)-offset) {
				return nil, io.ErrUnexpectedEOF
			}
			f.bytes = data[offset : offset+int(n)]
			offset += int(n)
		case 5:
			if len(data)-offset < 4 {
				return nil, io.ErrUnexpectedEOF
			}
			offset += 4 // fixed32: not used by bin-ws, skipped
			continue
		default:
			return nil, fmt.Errorf("unsupported protobuf wire type %d", wire)
		}
		m[number] = append(m[number], f)
	}
	return m, nil
}

func (m message) has(n int) bool { return len(m[n]) > 0 }

// last returns the last occurrence (proto3: last one wins for singular fields).
func (m message) last(n int) (field, bool) {
	fs := m[n]
	if len(fs) == 0 {
		return field{}, false
	}
	return fs[len(fs)-1], true
}

func (m message) uint64(n int) (uint64, error) {
	f, ok := m.last(n)
	if !ok {
		return 0, nil
	}
	if f.wire != 0 {
		return 0, fmt.Errorf("field %d: wire type %d, want varint", n, f.wire)
	}
	return f.value, nil
}

func (m message) uint32(n int) (uint32, error) {
	v, err := m.uint64(n)
	if err != nil {
		return 0, err
	}
	if v > 0xFFFFFFFF {
		return 0, fmt.Errorf("field %d: %d overflows uint32", n, v)
	}
	return uint32(v), nil
}

func (m message) optUint64(n int) (*uint64, error) {
	if !m.has(n) {
		return nil, nil
	}
	v, err := m.uint64(n)
	return &v, err
}

func (m message) optUint32(n int) (*uint32, error) {
	if !m.has(n) {
		return nil, nil
	}
	v, err := m.uint32(n)
	return &v, err
}

func (m message) bool(n int) (bool, error) {
	v, err := m.uint64(n)
	return v != 0, err
}

func (m message) bytes(n int) ([]byte, error) {
	f, ok := m.last(n)
	if !ok {
		return nil, nil
	}
	if f.wire != 2 {
		return nil, fmt.Errorf("field %d: wire type %d, want bytes", n, f.wire)
	}
	return append([]byte(nil), f.bytes...), nil
}

func (m message) string(n int) (string, error) {
	b, err := m.bytes(n)
	return string(b), err
}

// repeated64 accepts both packed (wire 2) and unpacked (wire 0) encodings, as
// Protobuf parsers must.
func (m message) repeated64(n int) ([]uint64, error) {
	var out []uint64
	for _, f := range m[n] {
		switch f.wire {
		case 0:
			out = append(out, f.value)
		case 2:
			for offset := 0; offset < len(f.bytes); {
				v, err := readUvarint(f.bytes, &offset)
				if err != nil {
					return nil, fmt.Errorf("field %d: %w", n, err)
				}
				out = append(out, v)
			}
		default:
			return nil, fmt.Errorf("field %d: wire type %d in repeated varint", n, f.wire)
		}
	}
	return out, nil
}

func (m message) repeated32(n int) ([]uint32, error) {
	wide, err := m.repeated64(n)
	if err != nil {
		return nil, err
	}
	out := make([]uint32, len(wide))
	for i, v := range wide {
		if v > 0xFFFFFFFF {
			return nil, fmt.Errorf("field %d: %d overflows uint32", n, v)
		}
		out[i] = uint32(v)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// messages returns every occurrence of a repeated embedded-message field.
func (m message) messages(n int) ([][]byte, error) {
	var out [][]byte
	for _, f := range m[n] {
		if f.wire != 2 {
			return nil, fmt.Errorf("field %d: wire type %d, want message", n, f.wire)
		}
		out = append(out, f.bytes)
	}
	return out, nil
}

// reader collects the first decoding error so typed decoders stay linear.
type reader struct {
	m   message
	err error
}

func (r *reader) keep(err error) {
	if r.err == nil && err != nil {
		r.err = err
	}
}
func (r *reader) u64(n int) uint64     { v, err := r.m.uint64(n); r.keep(err); return v }
func (r *reader) u32(n int) uint32     { v, err := r.m.uint32(n); r.keep(err); return v }
func (r *reader) opt64(n int) *uint64  { v, err := r.m.optUint64(n); r.keep(err); return v }
func (r *reader) opt32(n int) *uint32  { v, err := r.m.optUint32(n); r.keep(err); return v }
func (r *reader) boolean(n int) bool   { v, err := r.m.bool(n); r.keep(err); return v }
func (r *reader) raw(n int) []byte     { v, err := r.m.bytes(n); r.keep(err); return v }
func (r *reader) str(n int) string     { v, err := r.m.string(n); r.keep(err); return v }
func (r *reader) rep64(n int) []uint64 { v, err := r.m.repeated64(n); r.keep(err); return v }
func (r *reader) rep32(n int) []uint32 { v, err := r.m.repeated32(n); r.keep(err); return v }
func (r *reader) msgs(n int) [][]byte  { v, err := r.m.messages(n); r.keep(err); return v }
func newReader(data []byte) (*reader, error) {
	m, err := parse(data)
	if err != nil {
		return nil, err
	}
	return &reader{m: m}, nil
}
