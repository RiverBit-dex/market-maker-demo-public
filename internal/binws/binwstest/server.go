// Package binwstest is an in-process fake bin-ws server for unit tests. It
// performs the real login check (EIP-191 recovery over the login message) and
// then hands every client frame to a test handler. It never touches the network
// beyond 127.0.0.1.
package binwstest

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/riverbit-dex/market-maker-demo-public/internal/binws"
)

// Conn is the server side of one client connection.
type Conn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

// Send writes one WsV2ServerFrame.
func (c *Conn) Send(f binws.ServerFrame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteMessage(websocket.BinaryMessage, f.Marshal())
}

// Close drops the connection abruptly (no close frame).
func (c *Conn) Close() error { return c.ws.Close() }

// SendJSON writes a json frame with the given outer id.
func (c *Conn) SendJSON(id uint64, body string) error {
	return c.Send(binws.ServerFrame{ID: id, Kind: binws.KindJSON, Data: []byte(body)})
}

// Server accepts bin-ws connections.
type Server struct {
	*httptest.Server
	LoginHost string
	// User is the master account reported on successful login; empty = the signer.
	User string
	// Handle receives every client frame after login.
	Handle func(c *Conn, f binws.ClientFrame)

	mu     sync.Mutex
	logins []binws.Login
}

// URL returns the ws:// URL of the fake endpoint.
func (s *Server) URL() string { return "ws" + strings.TrimPrefix(s.Server.URL, "http") + "/bin-ws" }

// Logins returns the login frames received so far.
func (s *Server) Logins() []binws.Login {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]binws.Login(nil), s.logins...)
}

func New(t testing.TB, loginHost string) *Server {
	s := &Server{LoginHost: loginHost}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bin-ws" {
			http.NotFound(w, r)
			return
		}
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer ws.Close()
		conn := &Conn{ws: ws}
		if !s.login(t, conn) {
			return
		}
		for {
			kind, raw, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage {
				t.Errorf("client sent a non-binary frame")
				return
			}
			frame, err := binws.UnmarshalClientFrame(raw)
			if err != nil {
				t.Errorf("decode client frame: %v", err)
				return
			}
			if frame.Body == binws.ClientPing {
				_ = conn.SendJSON(0, `{"channel":"pong"}`)
				continue
			}
			if s.Handle != nil {
				s.Handle(conn, frame)
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) login(t testing.TB, conn *Conn) bool {
	_, raw, err := conn.ws.ReadMessage()
	if err != nil {
		t.Errorf("read login: %v", err)
		return false
	}
	frame, err := binws.UnmarshalClientFrame(raw)
	if err != nil || frame.Body != binws.ClientLogin {
		t.Errorf("first client frame is not a login (body=%d err=%v)", frame.Body, err)
		return false
	}
	login, err := binws.UnmarshalLogin(frame.Data)
	if err != nil {
		t.Errorf("decode login: %v", err)
		return false
	}
	s.mu.Lock()
	s.logins = append(s.logins, login)
	s.mu.Unlock()
	result := binws.LoginResult{}
	signer, err := binws.RecoverLoginSigner(s.LoginHost, login)
	if err != nil || len(login.Nonce) != 16 {
		result.Error = "bad signature"
	} else {
		result.OK = true
		result.Signer, _ = hex.DecodeString(signer[2:])
		user := s.User
		if user == "" {
			user = signer
		}
		result.User, _ = hex.DecodeString(strings.TrimPrefix(user, "0x"))
	}
	if err := conn.Send(binws.ServerFrame{Kind: binws.KindLogin, Data: result.Marshal()}); err != nil {
		t.Errorf("send login result: %v", err)
		return false
	}
	return result.OK
}
