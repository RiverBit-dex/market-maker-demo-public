package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/riverbit-dex/market-maker-demo-public/internal/binws"
	"github.com/riverbit-dex/market-maker-demo-public/internal/config"
	"github.com/riverbit-dex/market-maker-demo-public/internal/gateway"
	"github.com/riverbit-dex/market-maker-demo-public/internal/hlexchange"
	"github.com/riverbit-dex/market-maker-demo-public/internal/marketdata"
)

func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	for _, name := range cfg.Ignored {
		logger.Warn("environment variable is no longer used; set BIN_WS_URL instead", "variable", name)
	}
	if err := cfg.Validate(); err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 {
		command := os.Args[1]
		if len(os.Args) != 2 || (command != "trade" && command != "recover") {
			logger.Error("usage: market-maker-demo [trade|recover]")
			os.Exit(1)
		}
		if err := runTrade(ctx, cfg, logger, command == "recover"); err != nil {
			logger.Error(command+" failed", "error", err)
			os.Exit(1)
		}
		return
	}

	logger.Info("starting market maker demo", "coin", cfg.Coin, "bin_ws", cfg.BinWSURL, "login_host", cfg.BinWSLoginHost, "vault_configured", cfg.GuildVault != "")
	checkCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	err := gateway.New(cfg.APIGatewayURL).Check(checkCtx)
	cancel()
	if err != nil {
		logger.Warn("HTTP /info check failed; continuing with bin-ws only", "error", err)
	} else {
		logger.Info("HTTP /info is reachable", "base", cfg.APIGatewayURL)
	}
	// bin-ws requires a login on every connection, market data included.
	client, err := newClient(cfg, "")
	if err != nil {
		logger.Error("load signing key", "error", err)
		os.Exit(1)
	}
	err = marketdata.Run(ctx, marketdata.RunConfig{
		URL: cfg.BinWSURL, LoginHost: cfg.BinWSLoginHost, Sign: client.PersonalSign,
		Coin: cfg.Coin, BookStream: cfg.BookStream, User: cfg.GuildVault, Log: logger,
	})
	if err != nil {
		logger.Error("market data stopped", "error", err)
		os.Exit(1)
	}
}

// newClient reads KEY_FILE and builds the signing client: for the vault when
// vault is set (connectionId tail 0x01 || vault, top-level vaultAddress),
// otherwise for the master account itself. The key is never printed.
func newClient(cfg config.Config, vault string) (*hlexchange.Client, error) {
	keyBytes, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	defer func() {
		for i := range keyBytes {
			keyBytes[i] = 0
		}
	}()
	signerKey := strings.TrimSpace(string(keyBytes))
	if signerKey == "" {
		return nil, fmt.Errorf("key file is empty")
	}
	var client *hlexchange.Client
	if vault != "" {
		client, err = hlexchange.NewForVault(cfg.APIGatewayURL, signerKey, false, vault)
	} else {
		client, err = hlexchange.New(cfg.APIGatewayURL, signerKey, false)
	}
	if err != nil {
		return nil, fmt.Errorf("configure signer: invalid key or address")
	}
	return client, nil
}

// readActions loads one typed action per file; ACTION_FILE may list several
// files separated by commas, submitted in order on the same connection.
func readActions(list string) ([]any, error) {
	var actions []any
	for _, path := range strings.Split(list, ",") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		action, err := parseAction(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		actions = append(actions, action)
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("ACTION_FILE must point to one or more JSON action files")
	}
	return actions, nil
}

func parseAction(raw []byte) (any, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, err
	}
	var action any
	switch head.Type {
	case "order":
		action = &hlexchange.Place{}
	case "cancel":
		action = &hlexchange.Cancel{}
	case "cancelByCloid":
		action = &hlexchange.CancelByCloid{}
	case "modify":
		action = &hlexchange.Modify{}
	case "batchModify":
		action = &hlexchange.BatchModify{}
	default:
		return nil, fmt.Errorf("unsupported action type %q", head.Type)
	}
	if err := json.Unmarshal(raw, action); err != nil {
		return nil, err
	}
	return action, nil
}

// trade submits every action from ACTION_FILE and reconciles each one before
// the next. ORDER_TRANSPORT picks the path:
//
//   - ws (default): one logged-in bin-ws connection carries the order-event
//     subscriptions, the signed actions (action_json) and the orderStatus /
//     openOrders reconciliation (info_json), replies matched by frame id.
//   - rest: signed actions are POSTed to <API_GATEWAY_URL>/exchange and
//     reconciled over POST /info; bin-ws is never dialed.
//
// Startup queries (chainIdentity, vault checks, userRole, metaAndAssetCtxs)
// use HTTP /info in both modes. Uncertain outcomes are left for
// reconciliation, never resent.
func trade(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	return runTrade(ctx, cfg, logger, false)
}

// recover only observes the journal and current orders. Keeping it separate
// from trade prevents a restart from accidentally resubmitting ACTION_FILE.
func runTrade(ctx context.Context, cfg config.Config, logger *slog.Logger, recoverOnly bool) error {
	useREST := cfg.OrderTransport == config.OrderTransportREST
	var actions []any
	var err error
	if !recoverOnly {
		actions, err = readActions(os.Getenv("ACTION_FILE"))
		if err != nil {
			return err
		}
	}
	client, err := newClient(cfg, cfg.GuildVault)
	if err != nil {
		return err
	}
	client.StateDir = cfg.StateDir
	client.RequireExecutionHeight = true
	if !recoverOnly {
		// The node verifies L1 agent signatures using the next-block EIP-712 chain ID.
		// Use nextAgentChainId, falling back to agentChainId on older gateways.
		// signatureChainId is for user-signed actions and must not be used here.
		var identity struct {
			NextAgentChainID uint64 `json:"nextAgentChainId"`
			AgentChainID     uint64 `json:"agentChainId"`
		}
		if err = client.Info(ctx, map[string]any{"type": "chainIdentity"}, &identity); err != nil {
			return fmt.Errorf("read chain signing identity: %w", err)
		}
		id := identity.NextAgentChainID
		if id == 0 {
			id = identity.AgentChainID
		}
		if id == 0 {
			return fmt.Errorf("gateway chainIdentity has no agentChainId")
		}
		if cfg.SignChainID != "" {
			configured, e := strconv.ParseUint(cfg.SignChainID, 10, 64)
			if e != nil || configured != id {
				return fmt.Errorf("SIGN_CHAIN_ID does not match gateway chainIdentity")
			}
		}
		client.SignChainID = id
		if err = client.VerifySubAccount(ctx); err != nil {
			return fmt.Errorf("verify vault signer authorization: %w", err)
		}
		assetIndex, sizeDecimals, maxLeverage, err := readAssetInfo(ctx, client, cfg.Coin)
		if err != nil {
			return err
		}
		for _, action := range actions {
			if err = validateActionAsset(action, assetIndex); err != nil {
				return err
			}
		}
		fmt.Printf("Verified %s asset: index=%d size_decimals=%d max_leverage=%d (from POST /info metaAndAssetCtxs)\n", cfg.Coin, assetIndex, sizeDecimals, maxLeverage)
	}

	via := "bin-ws (action_json; orderStatus/openOrders via info_json)"
	if useREST {
		via = "REST (POST /exchange; orderStatus/openOrders via POST /info)"
		if err = resolveAccountREST(ctx, client, cfg, logger); err != nil {
			return err
		}
	} else {
		session, err := dialTradeSession(ctx, cfg, client, logger)
		if err != nil {
			return err
		}
		defer session.Close()
		client.WS, client.Queries = session, session
	}
	account := client.AccountAddress()
	var recoveryErr error
	if recoverOnly {
		observeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		recoveryErr = client.RecoverPending(observeCtx)
		cancel()
		if recoveryErr != nil && !errors.Is(recoveryErr, hlexchange.ErrExecutionRejected) {
			return fmt.Errorf("recovery incomplete; keep STATE_DIR and run recover again: %w", recoveryErr)
		}
	}

	for i, action := range actions {
		fmt.Printf("Submitting action %d/%d (%T) for %s via %s\n", i+1, len(actions), action, account, via)
		submitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = client.Exchange(submitCtx, action)
		cancel()
		if err != nil {
			return fmt.Errorf("action stopped; partial effects are possible; inspect STATE_DIR and use recover before any new submission: %w", err)
		}
		if len(client.LastOrderIDs) > 0 {
			fmt.Printf("Resolved order IDs: %v (execution height=%d; statuses=%v)\n", client.LastOrderIDs, client.LastExecutionHeight, client.LastStatuses)
		}
	}
	orders, err := client.OpenOrders(ctx)
	if err != nil {
		return fmt.Errorf("open-orders reconciliation failed: %w", err)
	}
	for _, action := range actions {
		if err := verifyCanceledOrders(action, orders); err != nil {
			return err
		}
	}
	if recoverOnly {
		fmt.Printf("Recovery finished without submitting actions; current open orders for %s: %d\n", account, len(orders))
	} else {
		fmt.Printf("Actions accepted via %s; current open orders for %s: %d\n", via, account, len(orders))
	}
	for _, order := range orders {
		fmt.Printf("coin=%s oid=%d side=%s price=%s remaining=%s\n", order.Coin, order.OID, order.Side, order.Price, order.Size)
	}
	return recoveryErr
}

// dialTradeSession (ws mode only) logs in to bin-ws once, takes the trading
// account from the login and subscribes to its order events. The session is
// closed when ctx ends or the caller closes it.
func dialTradeSession(ctx context.Context, cfg config.Config, client *hlexchange.Client, logger *slog.Logger) (*binws.Session, error) {
	events := &marketdata.Handler{Log: logger}
	session, err := binws.Dial(ctx, binws.Config{
		URL: cfg.BinWSURL, LoginHost: cfg.BinWSLoginHost, Sign: client.PersonalSign, OnFrame: events.Handle,
	})
	if err != nil {
		return nil, fmt.Errorf("bin-ws login: %w", err)
	}
	marketdata.LogLogin(logger, session)
	if cfg.GuildVault == "" {
		// An API wallet trades for the master account the server resolved.
		if err = client.SetPrincipal(session.User()); err != nil {
			session.Close()
			return nil, fmt.Errorf("login user: %w", err)
		}
	} else {
		logger.Info("trading for vault", "vault", cfg.GuildVault, "leader", session.User())
	}
	for _, sub := range marketdata.OrderSubscriptions(client.AccountAddress()) {
		if err = session.Subscribe(sub); err != nil {
			session.Close()
			return nil, fmt.Errorf("subscribe %s: %w", sub.Type, err)
		}
	}
	return session, nil
}

// resolveAccountREST (rest mode only) finds the account to trade for without
// bin-ws. A vault target is already fixed and verified. Otherwise HTTP /info
// userRole says whether the key is an API wallet (role "agent", trading for
// data.user) or the master account itself.
func resolveAccountREST(ctx context.Context, client *hlexchange.Client, cfg config.Config, logger *slog.Logger) error {
	if cfg.GuildVault != "" {
		logger.Info("trading for vault", "vault", cfg.GuildVault, "signer", client.SignerAddress())
		return nil
	}
	var role struct {
		Role string `json:"role"`
		Data struct {
			User string `json:"user"`
		} `json:"data"`
	}
	if err := client.Info(ctx, map[string]any{"type": "userRole", "user": client.SignerAddress()}, &role); err != nil {
		return fmt.Errorf("read signer role: %w", err)
	}
	if role.Role == "agent" {
		if err := client.SetPrincipal(role.Data.User); err != nil {
			return fmt.Errorf("API wallet master account: %w", err)
		}
	}
	logger.Info("trading account resolved via POST /info userRole", "account", client.AccountAddress(), "signer", client.SignerAddress(), "signer_role", role.Role)
	return nil
}

func verifyCanceledOrders(action any, openOrders []hlexchange.OpenOrder) error {
	openOID := make(map[uint64]struct{}, len(openOrders))
	openCloid := make(map[string]struct{}, len(openOrders))
	for _, order := range openOrders {
		openOID[order.OID] = struct{}{}
		if order.Cloid != "" {
			openCloid[strings.ToLower(order.Cloid)] = struct{}{}
		}
	}
	checkOID := func(oid uint64) error {
		if _, ok := openOID[oid]; ok {
			return fmt.Errorf("cancel was accepted but OID %d is still open", oid)
		}
		return nil
	}
	checkCloid := func(cloid hlexchange.Cloid) error {
		if _, ok := openCloid[strings.ToLower(cloid.String())]; ok {
			return fmt.Errorf("cancelByCloid was accepted but cloid %s is still open", cloid.String())
		}
		return nil
	}
	switch a := action.(type) {
	case *hlexchange.Cancel:
		for _, item := range a.Cancels {
			if err := checkOID(item.OID); err != nil {
				return err
			}
		}
	case *hlexchange.CancelByCloid:
		for _, item := range a.Cancels {
			if err := checkCloid(item.Cloid); err != nil {
				return err
			}
		}
	}
	return nil
}

func readAssetInfo(ctx context.Context, client *hlexchange.Client, coin string) (int, int, int, error) {
	var snapshot []json.RawMessage
	if err := client.Info(ctx, map[string]any{"type": "metaAndAssetCtxs"}, &snapshot); err != nil {
		return 0, 0, 0, fmt.Errorf("read market asset info: %w", err)
	}
	if len(snapshot) == 0 {
		return 0, 0, 0, fmt.Errorf("metaAndAssetCtxs returned no market metadata")
	}
	var meta struct {
		Universe []struct {
			Name         string `json:"name"`
			SizeDecimals int    `json:"szDecimals"`
			MaxLeverage  int    `json:"maxLeverage"`
		} `json:"universe"`
	}
	if err := json.Unmarshal(snapshot[0], &meta); err != nil {
		return 0, 0, 0, fmt.Errorf("decode market metadata: %w", err)
	}
	for index, asset := range meta.Universe {
		if asset.Name == coin {
			return index, asset.SizeDecimals, asset.MaxLeverage, nil
		}
	}
	return 0, 0, 0, fmt.Errorf("market %q is absent from gateway metaAndAssetCtxs", coin)
}

func validateActionAsset(action any, expected int) error {
	check := func(actual int) error {
		if actual != expected {
			return fmt.Errorf("action asset index %d does not match selected market asset index %d", actual, expected)
		}
		return nil
	}
	switch a := action.(type) {
	case *hlexchange.Place:
		for _, order := range a.Orders {
			if err := check(order.Asset); err != nil {
				return err
			}
		}
	case *hlexchange.Modify:
		return check(a.Order.Asset)
	case *hlexchange.BatchModify:
		for _, item := range a.Modifies {
			if err := check(item.Order.Asset); err != nil {
				return err
			}
		}
	case *hlexchange.Cancel:
		for _, item := range a.Cancels {
			if err := check(item.Asset); err != nil {
				return err
			}
		}
	case *hlexchange.CancelByCloid:
		for _, item := range a.Cancels {
			if err := check(item.Asset); err != nil {
				return err
			}
			if item.Cloid == (hlexchange.Cloid{}) {
				return fmt.Errorf("cancelByCloid requires a non-zero cloid")
			}
		}
	default:
		return fmt.Errorf("unsupported action type %T", action)
	}
	return nil
}
