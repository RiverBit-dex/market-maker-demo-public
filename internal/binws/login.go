package binws

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// LoginTag is the first line of the login message (bin-ws-api.md §3).
const LoginTag = "riverbit-ws-v2-login"

// Signer signs msg with EIP-191 personal_sign and returns 65 bytes r || s || v
// (v = 27/28). hlexchange.Client.PersonalSign implements it, so the private key
// never leaves that package.
type Signer func(msg []byte) ([]byte, error)

// LoginMessage is the four-line text to personal_sign, without a trailing newline:
//
//	riverbit-ws-v2-login
//	host:<host>
//	timestamp:<ms>
//	nonce:<lowercase hex of the 16 nonce bytes, no 0x>
func LoginMessage(host string, timestampMs uint64, nonce []byte) string {
	return LoginTag + "\nhost:" + host + "\ntimestamp:" + strconv.FormatUint(timestampMs, 10) + "\nnonce:" + hex.EncodeToString(nonce)
}

// PersonalHash is the EIP-191 version 0x45 digest:
// keccak256("\x19Ethereum Signed Message:\n" + len(msg) + msg).
func PersonalHash(msg []byte) []byte {
	prefix := "\x19Ethereum Signed Message:\n" + strconv.Itoa(len(msg))
	return crypto.Keccak256([]byte(prefix), msg)
}

// NewLogin builds a signed WsV2Login for host at time now. rnd supplies the
// 16-byte nonce (nil = crypto/rand).
func NewLogin(host string, now time.Time, rnd io.Reader, sign Signer) (Login, error) {
	if host == "" {
		return Login{}, fmt.Errorf("login host is empty")
	}
	if rnd == nil {
		rnd = rand.Reader
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(rnd, nonce); err != nil {
		return Login{}, fmt.Errorf("login nonce: %w", err)
	}
	ts := uint64(now.UnixMilli())
	sig, err := sign([]byte(LoginMessage(host, ts, nonce)))
	if err != nil {
		return Login{}, fmt.Errorf("sign login message: %w", err)
	}
	if len(sig) != 65 || (sig[64] != 27 && sig[64] != 28) {
		return Login{}, fmt.Errorf("login signature must be 65 bytes with v=27/28")
	}
	return Login{TimestampMs: ts, Nonce: nonce, Signature: sig}, nil
}

// RecoverLoginSigner recovers the lowercase 0x address that signed l for host,
// the same way the server does. v may be 27/28 or 0/1.
func RecoverLoginSigner(host string, l Login) (string, error) {
	if len(l.Signature) != 65 {
		return "", fmt.Errorf("login signature is %d bytes, want 65", len(l.Signature))
	}
	sig := append([]byte(nil), l.Signature...)
	if sig[64] >= 27 {
		sig[64] -= 27
	}
	pub, err := crypto.SigToPub(PersonalHash([]byte(LoginMessage(host, l.TimestampMs, l.Nonce))), sig)
	if err != nil {
		return "", err
	}
	return strings.ToLower(crypto.PubkeyToAddress(*pub).Hex()), nil
}
