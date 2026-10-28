package hlexchange

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/vmihailenco/msgpack/v5"
)

func TestActionSignatureRecoversSignerFromDigest(t *testing.T) {
	client, err := New("https://example.invalid/api", strings.Repeat("0", 63)+"1", false)
	if err != nil {
		t.Fatal(err)
	}
	client.SignChainID = 12589
	action := Place{
		Type: "order",
		Orders: []Order{{
			Asset: 0, Buy: true, Price: "100.0000", Size: "0.10",
			Type: OrderType{Limit: Limit{TIF: "Alo"}},
		}},
		Grouping: "na",
	}
	digest, err := client.signingDigest(action, 1792140859836)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := client.signDigest(digest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signatureBytes(signature)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := crypto.SigToPub(digest, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ToLower(crypto.PubkeyToAddress(*publicKey).Hex()); got != client.SignerAddress() {
		t.Fatalf("recovered signer %s, want %s", got, client.SignerAddress())
	}
}

func TestCancelByCloidJSONAndSigningEncoding(t *testing.T) {
	const cloidText = "0x1234567890abcdef1234567890abcdef"
	var action CancelByCloid
	if err := json.Unmarshal([]byte(`{"type":"cancelByCloid","cancels":[{"asset":7,"cloid":"`+cloidText+`"}]}`), &action); err != nil {
		t.Fatal(err)
	}
	if action.Type != "cancelByCloid" || len(action.Cancels) != 1 || action.Cancels[0].Asset != 7 || action.Cancels[0].Cloid.String() != cloidText {
		t.Fatalf("decoded action = %#v", action)
	}

	encoded, err := msgpack.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := msgpack.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	cancels, ok := wire["cancels"].([]any)
	if !ok || len(cancels) != 1 {
		t.Fatalf("MessagePack cancels = %#v", wire["cancels"])
	}
	item, ok := cancels[0].(map[string]any)
	if !ok {
		t.Fatalf("MessagePack cancel item = %#v", cancels[0])
	}
	// The node re-encodes request JSON as MessagePack for verification; cloid must remain a 0x string.
	if got, ok := item["cloid"].(string); !ok || got != "0x1234567890abcdef1234567890abcdef" {
		t.Fatalf("MessagePack cloid = %#v; expected the JSON string", item["cloid"])
	}

	client, err := New("https://example.invalid/api", strings.Repeat("0", 63)+"1", false)
	if err != nil {
		t.Fatal(err)
	}
	client.SignChainID = 12589
	if _, err := client.Sign(action, 1792140859836); err != nil {
		t.Fatalf("sign cancelByCloid action: %v", err)
	}
}

func TestCloidRejectsMalformedHex(t *testing.T) {
	for _, value := range []string{"0x1234", "1234567890abcdef1234567890abcdef", "0xgggg567890abcdef1234567890abcdef"} {
		var cloid Cloid
		if err := json.Unmarshal([]byte(`"`+value+`"`), &cloid); err == nil {
			t.Errorf("accepted malformed cloid %q", value)
		}
	}
}

// The node re-encodes request JSON as MessagePack for verification, so the cloid
// must be a string in both. Encoding it as 16 binary bytes recovers a different
// signer (observed code 91: User or API Wallet does not exist).
func TestCancelByCloidSignsCloidAsTheJSONString(t *testing.T) {
	var cloid Cloid
	if err := json.Unmarshal([]byte(`"0x1ccfc83cae93766d6669c4b169b88031"`), &cloid); err != nil {
		t.Fatal(err)
	}
	got, err := msgpack.Marshal(CancelByCloid{Type: "cancelByCloid", Cancels: []CancelByCloidItem{{Asset: 0, Cloid: cloid}}})
	if err != nil {
		t.Fatal(err)
	}
	type item struct {
		Asset int    `msgpack:"asset"`
		Cloid string `msgpack:"cloid"`
	}
	type action struct {
		Type    string `msgpack:"type"`
		Cancels []item `msgpack:"cancels"`
	}
	want, err := msgpack.Marshal(action{Type: "cancelByCloid", Cancels: []item{{Asset: 0, Cloid: "0x1ccfc83cae93766d6669c4b169b88031"}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("cancelByCloid msgpack must carry the cloid as its JSON string\n got %x\nwant %x", got, want)
	}
}

// Signing vector from the guide: private key 0x0101…01, chainId 12589, source "b".
// Matches riverbit-ws scripts/e2e-bin-ws.py byte for byte; PRE accepted its signature.
func TestSigningVectorsFromTheAPIGuide(t *testing.T) {
	key := strings.Repeat("01", 32)
	action := Place{
		Type: "order",
		Orders: []Order{{
			Asset: 0, Buy: true, Price: "100.5", Size: "0.2",
			Type: OrderType{Limit: Limit{TIF: "Alo"}},
		}},
		Grouping: "na",
	}
	const nonce = 1792168694973
	cases := []struct {
		name, vault, connID, digest, r, s string
		v                                 byte
	}{
		{"no vault", "", "0260ebaee4220bfd192a49fad29e62a1f9890f5b70e89a095e8099686b4805a6",
			"724d79bc83f9bb39300af824a91d9e5a84028b1f15b90c77d1e3212f5290d272",
			"0xb075d6ff3fe5bfa399a22ecfca3375af8223181b21a363bde243ce9186edd7ec",
			"0x1f82100252650e8ddde4815134d4e0420afaca71b09ec03c278f0a4985cf552", 27},
		{"vault", "0x1111111111111111111111111111111111111111", "749d2e6560886f2092bcee6423609c41dc20e47d17ad9a50d5483e4165bc3d39",
			"e61bf62709ae5db02f460316839fbd16baf772a6abc096b7d44e5773f125d272",
			"0xc69888725b054488375d419e27dea48f41f2f8da5ec863adf9af1e1d31f1c071",
			"0x1adb693fdc1e45c68f4c39f4f868f731bcaaa11eb159fb2a5d46ebe8843aff24", 28},
	}
	for _, tc := range cases {
		var (
			client *Client
			err    error
		)
		if tc.vault == "" {
			client, err = New("https://example.invalid", key, false)
		} else {
			client, err = NewForVault("https://example.invalid", key, false, tc.vault)
		}
		if err != nil {
			t.Fatal(err)
		}
		client.SignChainID = 12589
		connID, err := client.connectionID(action, nonce)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := client.signingDigest(action, nonce)
		if err != nil {
			t.Fatal(err)
		}
		sig, err := client.Sign(action, nonce)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", connID); got != tc.connID {
			t.Errorf("%s connectionId %s, want %s", tc.name, got, tc.connID)
		}
		if got := fmt.Sprintf("%x", digest); got != tc.digest {
			t.Errorf("%s digest %s, want %s", tc.name, got, tc.digest)
		}
		if sig.R != tc.r || sig.S != tc.s || sig.V != tc.v {
			t.Errorf("%s signature %+v, want r=%s s=%s v=%d", tc.name, sig, tc.r, tc.s, tc.v)
		}
	}
}
