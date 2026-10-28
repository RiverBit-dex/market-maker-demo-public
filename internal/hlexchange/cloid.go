package hlexchange

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vmihailenco/msgpack/v5"
)

// Cloid is a 128-bit client order ID. JSON uses the public 0x-prefixed hex
// representation; MessagePack uses the 16-byte binary representation expected
// by the chain action decoder and signer.
type Cloid [16]byte

func (c Cloid) String() string { return "0x" + hex.EncodeToString(c[:]) }

func (c Cloid) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

func (c *Cloid) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("cloid must be a 0x-prefixed 128-bit hex string")
	}
	if !strings.HasPrefix(value, "0x") || len(value) != 34 {
		return fmt.Errorf("cloid must be a 0x-prefixed 128-bit hex string")
	}
	decoded, err := hex.DecodeString(value[2:])
	if err != nil || len(decoded) != len(c) {
		return fmt.Errorf("cloid must be a 0x-prefixed 128-bit hex string")
	}
	copy(c[:], decoded)
	return nil
}

// MarshalMsgpack encodes the same 0x string as JSON. The node re-encodes
// request JSON as MessagePack to verify the signature, so cloid must be a
// string in both representations or the recovered signer is wrong (code 91).
func (c Cloid) MarshalMsgpack() ([]byte, error) {
	return msgpack.Marshal(c.String())
}
