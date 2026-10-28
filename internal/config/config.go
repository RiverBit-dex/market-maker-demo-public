package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	// BinWSURL is the bin-ws endpoint used for market data, order events and
	// trading (one long-lived connection per process).
	BinWSURL string
	// BinWSLoginHost is signed into the login message ("host:" line). It must
	// equal the server's configured public host.
	BinWSLoginHost string
	// APIGatewayURL is the HTTP base; queries POST to <base>/info.
	APIGatewayURL string
	Coin          string
	// BookStream selects the order book subscription: "l2" (incremental, the
	// default) or "l2Book" (full snapshots).
	BookStream string
	// OrderTransport selects how the trade command submits signed actions and
	// reconciles them: "ws" (default) = bin-ws action_json + info_json on one
	// connection; "rest" = POST <APIGatewayURL>/exchange + POST /info, no bin-ws.
	OrderTransport string
	GuildVault     string
	KeyFile        string
	StateDir       string
	SignChainID    string
	// Ignored lists deprecated variables that are set but no longer read.
	Ignored []string
}

const (
	DefaultBinWSURL       = "wss://pre-api.riverbit.io/bin-ws"
	DefaultBinWSLoginHost = "pre-api.riverbit.io"
	DefaultAPIGatewayURL  = "https://pre-api.riverbit.io"

	OrderTransportWS   = "ws"
	OrderTransportREST = "rest"
)

func Load() Config {
	cfg := Config{
		BinWSURL:       value("BIN_WS_URL", DefaultBinWSURL),
		BinWSLoginHost: value("BIN_WS_LOGIN_HOST", DefaultBinWSLoginHost),
		APIGatewayURL:  value("API_GATEWAY_URL", DefaultAPIGatewayURL),
		Coin:           value("COIN", "RBT"),
		BookStream:     value("BOOK_STREAM", "l2"),
		OrderTransport: value("ORDER_TRANSPORT", OrderTransportWS),
		GuildVault:     strings.ToLower(os.Getenv("GUILD_VAULT_ADDRESS")),
		KeyFile:        value("KEY_FILE", "key"),
		StateDir:       value("STATE_DIR", ".guild-mm-state"),
		SignChainID:    os.Getenv("SIGN_CHAIN_ID"),
	}
	// WS_URL / TRADE_WS_URL pointed at the old /trade-ws/v2 protocol, which
	// bin-ws does not speak (no login frame). They are not used as fallbacks so
	// a stale value cannot silently connect to the wrong protocol.
	for _, old := range []string{"WS_URL", "TRADE_WS_URL"} {
		if os.Getenv(old) != "" {
			cfg.Ignored = append(cfg.Ignored, old)
		}
	}
	return cfg
}

// Validate checks values that would otherwise fail late on the wire.
func (c Config) Validate() error {
	if c.BookStream != "l2" && c.BookStream != "l2Book" {
		return fmt.Errorf("BOOK_STREAM must be l2 or l2Book, got %q", c.BookStream)
	}
	if c.OrderTransport != OrderTransportWS && c.OrderTransport != OrderTransportREST {
		return fmt.Errorf("ORDER_TRANSPORT must be ws or rest, got %q", c.OrderTransport)
	}
	if c.BinWSLoginHost == "" {
		return fmt.Errorf("BIN_WS_LOGIN_HOST is empty")
	}
	return nil
}

func value(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
