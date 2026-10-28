package hlexchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/riverbit-dex/market-maker-demo-public/internal/binws"
)

// ActionTransport submits signed /exchange request bodies over a long-lived
// connection. *binws.Session implements it: the body goes out as action_json
// with an incrementing frame id and the reply is matched by that id.
type ActionTransport interface {
	// URL identifies the transport in durable request journals.
	URL() string
	// PostAction returns the node's /exchange answer for one signed request.
	PostAction(ctx context.Context, signedRequest []byte) (json.RawMessage, error)
}

// InfoTransport sends info queries over a long-lived connection.
// *binws.Session implements it with info_json; the reply payload is
// {"type":<query type>,"data":<the HTTP /info answer>}.
type InfoTransport interface {
	PostInfo(ctx context.Context, request []byte) (json.RawMessage, error)
}

var (
	_ ActionTransport = (*binws.Session)(nil)
	_ InfoTransport   = (*binws.Session)(nil)
)

// exchangeTimeout bounds one submission on either transport. Running out of
// time leaves the outcome unknown: the caller reconciles, it never resends.
const exchangeTimeout = 20 * time.Second

// postBinary sends the signed request body over bin-ws
// (WsV2ClientFrame.action_json), byte for byte as produced by submit.
func (c *Client) postBinary(ctx context.Context, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	payload, err := c.WS.PostAction(ctx, body)
	if err != nil {
		return fmt.Errorf("bin-ws action: %w", err)
	}
	if err = json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("invalid exchange response: %w", err)
	}
	return nil
}

// postREST POSTs the same signed request body to <URL>/exchange. A non-200
// status or a timeout is returned as an error, which the caller treats as an
// unknown outcome (reconcile via orderStatus, no resend).
func (c *Client) postREST(ctx context.Context, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	if err := c.postRaw(ctx, "/exchange", body, out); err != nil {
		return fmt.Errorf("REST exchange: %w", err)
	}
	return nil
}

// wsInfo sends one info query as info_json on Queries and decodes the
// unwrapped data into out. A "429" error reply is retried with backoff.
func (c *Client) wsInfo(ctx context.Context, query, out any) error {
	request, err := json.Marshal(query)
	if err != nil {
		return err
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(request, &head); err != nil || head.Type == "" {
		return fmt.Errorf("bin-ws info query without type")
	}
	var payload json.RawMessage
	err = retryRateLimited(ctx, func() error {
		var e error
		payload, e = c.Queries.PostInfo(ctx, request)
		return e
	}, func(err error) bool {
		var reply *binws.PostReplyError
		return errors.As(err, &reply) && strings.Contains(reply.Payload, "429")
	})
	if err != nil {
		return fmt.Errorf("bin-ws info %s: %w", head.Type, err)
	}
	var wrapped struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &wrapped); err != nil || wrapped.Type != head.Type || len(wrapped.Data) == 0 {
		return fmt.Errorf("bin-ws info %s: unexpected payload %.300s", head.Type, payload)
	}
	if err := json.Unmarshal(wrapped.Data, out); err != nil {
		return fmt.Errorf("bin-ws info %s invalid JSON: %w", head.Type, err)
	}
	return nil
}

// PersonalSign signs msg with EIP-191 personal_sign using the client key and
// returns 65 bytes r || s || v (v = 27/28). It is the binws.Signer for the
// bin-ws login, so the key stays inside this package.
func (c *Client) PersonalSign(msg []byte) ([]byte, error) {
	sig, err := crypto.Sign(binws.PersonalHash(msg), c.key)
	if err != nil {
		return nil, err
	}
	sig[64] += 27
	return sig, nil
}
