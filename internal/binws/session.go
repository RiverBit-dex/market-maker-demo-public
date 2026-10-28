package binws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Config describes one bin-ws connection.
type Config struct {
	URL       string // e.g. wss://pre-api.riverbit.io/bin-ws
	LoginHost string // "host:" line of the login message, e.g. pre-api.riverbit.io
	Sign      Signer // EIP-191 personal_sign with the API wallet (or master) key
	// OnFrame receives every frame not claimed by a pending request: pushes,
	// subscription acks, pongs (all id 0) and late replies. It runs on the read
	// goroutine; keep it fast and do not call Session methods that wait on replies.
	OnFrame func(ServerFrame)
	// OnDecodeError, if set, is told about server frames that are not valid
	// WsV2ServerFrames. The session closes because a skipped frame could lose
	// an incremental book update or a trading reply.
	OnDecodeError func(error)

	Dialer       *websocket.Dialer // nil = websocket.DefaultDialer
	LoginTimeout time.Duration     // zero = 10s (the server's limit)
	PingInterval time.Duration     // zero = 20s; the server drops a connection silent for 60s
	Now          func() time.Time  // nil = time.Now
	Rand         io.Reader         // nil = crypto/rand; login nonce source
}

// Session is one logged-in bin-ws connection. Market data, order events and
// signed actions all share it; replies are matched by the outer frame id.
type Session struct {
	cfg    Config
	conn   *websocket.Conn
	login  LoginResult
	signer string // locally recovered signer of our own login signature

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan ServerFrame
	err     error
	done    chan struct{}
	closing sync.Once
}

// ErrClosed is returned for requests on a session whose connection ended.
var ErrClosed = errors.New("bin-ws session closed")

// Dial connects, sends WsV2Login as the first frame and waits for
// WsV2LoginResult. It fails unless the result is ok and the server recovered
// the same signer as the local key. ctx bounds the whole session: when it is
// canceled the connection is closed.
func Dial(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.Sign == nil {
		return nil, errors.New("bin-ws: no signer configured")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.LoginTimeout == 0 {
		cfg.LoginTimeout = 10 * time.Second
	}
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 20 * time.Second
	}
	dialer := cfg.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, _, err := dialer.DialContext(ctx, cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", cfg.URL, err)
	}
	conn.SetReadLimit(2 << 20)
	s := &Session{cfg: cfg, conn: conn, pending: map[uint64]chan ServerFrame{}, done: make(chan struct{})}
	if err := s.doLogin(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	go s.readLoop()
	go s.pingLoop()
	go func() {
		select {
		case <-ctx.Done():
			s.fail(ctx.Err())
		case <-s.done:
		}
	}()
	return s, nil
}

func (s *Session) doLogin(ctx context.Context) error {
	login, err := NewLogin(s.cfg.LoginHost, s.cfg.Now(), s.cfg.Rand, s.cfg.Sign)
	if err != nil {
		return err
	}
	if s.signer, err = RecoverLoginSigner(s.cfg.LoginHost, login); err != nil {
		return fmt.Errorf("verify own login signature: %w", err)
	}
	deadline := time.Now().Add(s.cfg.LoginTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = s.conn.SetWriteDeadline(deadline)
	frame := ClientFrame{Body: ClientLogin, Data: login.Marshal()}
	if err := s.conn.WriteMessage(websocket.BinaryMessage, frame.Marshal()); err != nil {
		return fmt.Errorf("send login: %w", err)
	}
	_ = s.conn.SetReadDeadline(deadline)
	kind, raw, err := s.conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read login result: %w", err)
	}
	if kind != websocket.BinaryMessage {
		return fmt.Errorf("login: server sent a non-binary frame")
	}
	reply, err := UnmarshalServerFrame(raw)
	if err != nil {
		return fmt.Errorf("decode login reply: %w", err)
	}
	if reply.Kind != KindLogin {
		return fmt.Errorf("first server frame is %s, want login result: %.200s", reply.Kind, reply.Data)
	}
	result, err := UnmarshalLoginResult(reply.Data)
	if err != nil {
		return fmt.Errorf("decode login result: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("login rejected: %s", result.Error)
	}
	if got := result.SignerAddress(); got != s.signer {
		return fmt.Errorf("server recovered signer %s, local key is %s (check BIN_WS_LOGIN_HOST)", got, s.signer)
	}
	if len(result.User) != 20 {
		return fmt.Errorf("login result user is %d bytes, want 20", len(result.User))
	}
	s.login = result
	_ = s.conn.SetWriteDeadline(time.Time{})
	return nil
}

// LoginResult is the server's answer to our login (ok, user, signer).
func (s *Session) LoginResult() LoginResult { return s.login }

// User is the master account the server resolved from the login signature.
func (s *Session) User() string { return s.login.UserAddress() }

// Signer is the address that signed the login (API wallet or master itself).
func (s *Session) Signer() string { return s.login.SignerAddress() }

// URL identifies the transport (used for durable request journals).
func (s *Session) URL() string { return s.cfg.URL }

// Done is closed when the connection ends; Err then reports why.
func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the connection; pending requests fail with ErrClosed.
func (s *Session) Close() error {
	s.fail(ErrClosed)
	return nil
}

func (s *Session) fail(err error) {
	s.closing.Do(func() {
		s.mu.Lock()
		s.err = err
		s.pending = map[uint64]chan ServerFrame{}
		s.mu.Unlock()
		s.writeMu.Lock()
		_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		s.writeMu.Unlock()
		_ = s.conn.Close()
		close(s.done)
	})
}

func (s *Session) write(f ClientFrame) error {
	select {
	case <-s.done:
		return fmt.Errorf("%w: %v", ErrClosed, s.Err())
	default:
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := s.conn.WriteMessage(websocket.BinaryMessage, f.Marshal()); err != nil {
		go s.fail(fmt.Errorf("write: %w", err))
		return err
	}
	return nil
}

// Subscribe sends subscribe_json with the subscription object itself, e.g.
// {"type":"l2","c":"RBT"} or {"type":"orderUpdates","user":"0x.."}. The ack
// arrives later through OnFrame as {"channel":"subscriptionResponse",...}.
func (s *Session) Subscribe(sub any) error   { return s.sendJSON(ClientSubscribe, sub) }
func (s *Session) Unsubscribe(sub any) error { return s.sendJSON(ClientUnsubscribe, sub) }

func (s *Session) sendJSON(body ClientBody, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.write(ClientFrame{Body: body, Data: data})
}

// Ping sends an empty WsV2Ping.
func (s *Session) Ping() error { return s.write(ClientFrame{Body: ClientPing}) }

// PostAction sends a signed /exchange request body as action_json and returns
// the node's /exchange answer (the "payload" of the post reply). The bytes are
// sent unchanged: the key order of "action" is part of the signature.
//
// An error does not prove the action was not executed. Preserve the request
// and reconcile its outcome; do not resend an unknown action.
func (s *Session) PostAction(ctx context.Context, signedRequest []byte) (json.RawMessage, error) {
	return s.post(ctx, ClientAction, signedRequest, "action")
}

// PostInfo sends info_json. bin-ws only serves a whitelist of info types
// (bin-ws-api.md §7); HTTP /info has no such limit.
func (s *Session) PostInfo(ctx context.Context, request []byte) (json.RawMessage, error) {
	return s.post(ctx, ClientInfo, request, "info")
}

func (s *Session) post(ctx context.Context, body ClientBody, data []byte, wantType string) (json.RawMessage, error) {
	ch := make(chan ServerFrame, 1)
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrClosed, err)
	}
	s.nextID++
	id := s.nextID
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	if err := s.write(ClientFrame{ID: id, Body: body, Data: data}); err != nil {
		return nil, fmt.Errorf("send %s request id=%d: %w", wantType, id, err)
	}
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("%s request id=%d: no reply: %w", wantType, id, ctx.Err())
	case <-s.done:
		return nil, fmt.Errorf("%s request id=%d: connection ended before the reply: %v", wantType, id, s.Err())
	case reply := <-ch:
		return ParsePostReply(reply, id, wantType)
	}
}

// PostReplyError is a {"type":"error"} post reply from the gateway or node,
// for example "429 Too Many Requests".
type PostReplyError struct {
	ID      uint64
	Payload string
}

func (e *PostReplyError) Error() string {
	return fmt.Sprintf("bin-ws request id=%d: error reply: %s", e.ID, e.Payload)
}

// ParsePostReply checks that frame is the reply to request id and returns its payload:
//
//	{"channel":"post","data":{"id":<id>,"response":{"type":"action"|"info","payload":..}}}
func ParsePostReply(frame ServerFrame, id uint64, wantType string) (json.RawMessage, error) {
	if frame.ID != id {
		return nil, fmt.Errorf("reply frame id=%d, request id=%d", frame.ID, id)
	}
	if frame.Kind != KindJSON {
		return nil, fmt.Errorf("reply to request id=%d is a %s frame, want json", id, frame.Kind)
	}
	var push struct {
		Channel string `json:"channel"`
		Data    struct {
			ID       *uint64 `json:"id"`
			Response struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			} `json:"response"`
		} `json:"data"`
	}
	if err := json.Unmarshal(frame.Data, &push); err != nil {
		return nil, fmt.Errorf("reply to request id=%d: invalid JSON: %w", id, err)
	}
	if push.Channel != "post" {
		return nil, fmt.Errorf("reply to request id=%d: channel %q, want post: %.300s", id, push.Channel, frame.Data)
	}
	if push.Data.ID != nil && *push.Data.ID != id {
		return nil, fmt.Errorf("reply to request id=%d carries data.id=%d", id, *push.Data.ID)
	}
	switch push.Data.Response.Type {
	case wantType:
		if len(push.Data.Response.Payload) == 0 {
			return nil, fmt.Errorf("reply to request id=%d has no payload", id)
		}
		return push.Data.Response.Payload, nil
	case "error":
		var text string
		if json.Unmarshal(push.Data.Response.Payload, &text) != nil {
			text = string(push.Data.Response.Payload)
		}
		return nil, &PostReplyError{ID: id, Payload: text}
	}
	return nil, fmt.Errorf("reply to request id=%d: response type %q, want %q", id, push.Data.Response.Type, wantType)
}

func (s *Session) readLoop() {
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(3*s.cfg.PingInterval + 15*time.Second))
		kind, raw, err := s.conn.ReadMessage()
		if err != nil {
			s.fail(fmt.Errorf("read: %w", err))
			return
		}
		if kind != websocket.BinaryMessage {
			continue // bin-ws only sends binary frames
		}
		frame, err := UnmarshalServerFrame(raw)
		if err != nil {
			if s.cfg.OnDecodeError != nil {
				s.cfg.OnDecodeError(err)
			}
			s.fail(fmt.Errorf("decode server frame: %w", err))
			return
		}
		if frame.ID != 0 {
			s.mu.Lock()
			ch, ok := s.pending[frame.ID]
			if ok {
				delete(s.pending, frame.ID)
			}
			s.mu.Unlock()
			if ok {
				ch <- frame
				continue
			}
		}
		if s.cfg.OnFrame != nil {
			s.cfg.OnFrame(frame)
		}
	}
}

func (s *Session) pingLoop() {
	ticker := time.NewTicker(s.cfg.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if err := s.Ping(); err != nil {
				return
			}
		}
	}
}
