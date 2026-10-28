# RiverBit Market Maker Integration Demo

English | [简体中文](README.zh-CN.md)

This Go demo shows how a market maker can use an API wallet to connect to RiverBit PRE: subscribe to market data and order events, place, cancel and modify orders, and reconcile order status. The same code supports trading for the master account or a vault.

**This demo does not contain an automated quoting strategy.** It submits actions from JSON files supplied by the user; it does not choose prices or sizes. Continuous market making additionally requires market-data freshness gates, inventory and funding limits, pause and cancellation handling, order and fill recovery, and real-load acceptance. The internal offchain maker uses native `/ws/v2`, not this demo's authenticated `/bin-ws`; changing the URL alone is insufficient.

The [API guide](docs/guild-market-maker-api.en.md) describes protocol details, signing, fields and errors. This README describes how to run the demo.

## Features

- Market data: incremental `l2` book (default, locally replayed to print best bid and ask), full `l2Book`, `trades`, and one-minute `candle`.
- Order and fill events: `orderUpdates`, `userFills`.
- Single and batch orders (`order`), cancel by oid (`cancel`) or cloid (`cancelByCloid`), and single or batch modification (`modify`, `batchModify`).
- Reconciliation using per-leg receipts and `orderStatus`, followed by `openOrders` and a check that cancellation targets are no longer open.
- `ORDER_TRANSPORT=ws` (default) or `rest`; master account or vault via `GUILD_VAULT_ADDRESS`.
- Nonce and pending requests persisted in `STATE_DIR`. Unknown outcomes are reconciled, not resent. `recover` reads pending requests and open orders without reading `ACTION_FILE` or submitting transactions. Complete batch-modify receipts survive restart.
- Market-data bin-ws connections reconnect, log in, and resubscribe automatically.

## Endpoints

| Purpose | Endpoint |
|---|---|
| REST queries | `https://pre-api.riverbit.io/info` |
| REST trading | `https://pre-api.riverbit.io/exchange` |
| WebSocket | `wss://pre-api.riverbit.io/bin-ws` |

## Prerequisites

1. Use the Go version in `go.mod`, at least 1.26.9, with the latest patch of a maintained release.
2. Put the API wallet private key on one line in `key`, or specify `KEY_FILE`. The default file is gitignored. Never commit a private key or put it in command arguments, logs, or action files; the program does not print it.
3. The master account must first authorize the API wallet. **Every bin-ws connection must log in, including market-data-only connections.**
4. For vault trading, the signer must be the vault leader or its authorized API wallet. Query `/info` `leadingVaults` using the master address for the vault address.

## Quick start

Clone the repository before running the commands below:

```sh
git clone https://github.com/RiverBit-dex/market-maker-demo-public.git
cd market-maker-demo-public
```


Run commands from the repository root with `./key` prepared. Logs use `slog` text format.

### Market data and order events

```sh
go run ./cmd/market-maker-demo
```

Expect startup and HTTP reachability messages, `bin-ws login ok`, a subscription confirmation, and streaming `l2`, trades and candles; order updates appear when orders change. An unreachable HTTP `/info` produces a warning and the program continues. Set `BOOK_STREAM=l2Book` for full snapshots. Set `GUILD_VAULT_ADDRESS` to subscribe to a vault's order events; otherwise they follow the logged-in master account. Stop with Ctrl+C.

### Place orders via REST or WebSocket

Copy `examples/rbt-batch-order.sample.json` to `my-order.json` and adjust the market index, prices and sizes for the live market. Then run:

```sh
ORDER_TRANSPORT=rest ACTION_FILE=my-order.json go run ./cmd/market-maker-demo trade
```

The program verifies the asset against `COIN`, resolves the trading account, submits the action, reports resolved order IDs and execution statuses, and lists current open orders. REST mode does not connect to bin-ws or receive order events. To use WebSocket instead, set `ORDER_TRANSPORT=ws` or omit it (the default); this logs in and subscribes before submitting and reconciling over the same connection.

For vault trading, set `GUILD_VAULT_ADDRESS=0x<your-vault-address>` with either transport. Startup verifies authorization. **Guild vault limit orders must all use `Alo`**; a `Gtc` leg rejects the entire action with code 1160. Master-account orders do not have this Guild restriction.

### Full order, modify and cancel flow

Create `flow-order.json` with two unique lowercase cloids (`0x` plus 32 lowercase hex digits). Adjust the asset, price and size:

```json
{"type":"order","orders":[
  {"a":0,"b":true,"p":"100.0000","s":"0.15","r":false,"t":{"limit":{"tif":"Alo"}},"c":"0x11111111111111111111111111111111"},
  {"a":0,"b":true,"p":"99.0000","s":"0.15","r":false,"t":{"limit":{"tif":"Alo"}},"c":"0x22222222222222222222222222222222"}
],"grouping":"na"}
```

Run `ACTION_FILE=flow-order.json go run ./cmd/market-maker-demo trade` and record the two returned oids in order. Create `flow-modify.json`, replacing `<oid1>` with the first actual oid:

```json
{"type":"batchModify","modifies":[{"oid":<oid1>,"order":{"a":0,"b":true,"p":"99.5000","s":"0.15","r":false,"t":{"limit":{"tif":"Alo"}},"c":"0x11111111111111111111111111111111"}}]}
```

Run it with `ACTION_FILE=flow-modify.json go run ./cmd/market-maker-demo trade`. The replacement has a new oid. Including the original `c` preserves the cloid; without it the demo generates a new one. Single `modify` is also supported; its response does not include the new oid, so the demo resolves it via cloid.

Cancel the second original order with `flow-cancel.json`, replacing `<oid2>` with its actual oid:

```json
{"type":"cancel","cancels":[{"a":0,"o":<oid2>}]}
```

Cancel the modified order with `flow-cancel-cloid.json`:

```json
{"type":"cancelByCloid","cancels":[{"asset":0,"cloid":"0x11111111111111111111111111111111"}]}
```

Run each with `ACTION_FILE=<file> go run ./cmd/market-maker-demo trade`. The demo checks that each cancellation target is absent from open orders; `current open orders ...: 0` means there are no current open orders. Multiple action files may be comma-separated in `ACTION_FILE`; each is reconciled before the next is submitted.

### Recover after timeout, disconnect or restart

Each action has a combined 30-second submission and result-wait limit. A timeout is not proof of failure and does not clear pending. Preserve the original `KEY_FILE`, `STATE_DIR`, account/vault, endpoints and transport, then run:

```sh
go run ./cmd/market-maker-demo recover
```

Set `ORDER_TRANSPORT=rest` if REST was used and keep the original vault setting when applicable. `recover` does not read `ACTION_FILE` or place, modify or cancel orders. Each recovery round waits up to 30 seconds; unresolved requests remain pending and return a nonzero exit code. With no pending request it only queries open orders. `trade` refuses new submissions when pending exists; it never automatically replays the action file.

Large batches make progress in five-second query rounds. Positive evidence that passes the height check is saved in the original journal and reused after slow queries, history eviction or restart. Saved statuses are historical observations, not proof that orders remain open. Unknown legs cannot be inferred from known legs.

Batch modification can partially succeed. Complete per-leg receipts are retained: rejected legs do not wait for nonexistent new cloids or require cancellation of the old order; successful legs are queried by their returned new oid. Partial failure exits nonzero; successful legs are not rolled back. Do not resend the entire batch. Check open orders, order status and fills before constructing a new action.

Cancellation also saves per-leg receipts and independently checks the target's current `order.status`. A `legResult.filled`, a filled target, an unknown status, or `action already pending` does not prove this cancellation has finished. Missing receipts leave the request pending. In particular, do not reuse a cloid after an unknown `cancelByCloid`: a delayed cancellation could affect a new order. Rejected legs cause an error and are not described as canceled.

`unknownOid`, elapsed time and an empty `openOrders` list cannot prove that the original request was not executed: it may already have filled. If public queries cannot supply sufficient evidence, keep pending and obtain the original execution evidence for manual reconciliation. **Do not delete or change `STATE_DIR`, or use a new nonce to bypass pending.**

This demo recovers order evidence; its final `openOrders` response has no snapshot height and cannot prove that orders, positions and balances are from the same instant. The internal maker additionally verifies complete signed-transaction finality and fresh complete order/account snapshots beyond a read watermark, and supplies legacy journal recovery hints and controlled migration. The demo does not implement those capabilities or infer inventory or funds from order events.

## Action files and configuration

Each `ACTION_FILE` is a JSON action of type `order`, `cancel`, `cancelByCloid`, `modify` or `batchModify`. The `examples/` directory contains batch order, batch modify and cancel-by-cloid samples. All sample prices, sizes, asset indices, oids and cloids are placeholders. Check the live market and actual orders before submitting; the asset must match `COIN`. Each order must have at least $10 notional. Supplied `c` values are preserved; otherwise the demo generates a random cloid. `batchModify` is recommended because it returns the new oid. To preserve a cloid in a replacement, include it in `order.c`.

| Variable | Default | Purpose |
|---|---|---|
| `BIN_WS_URL` | `wss://pre-api.riverbit.io/bin-ws` | Market data, events and WS trading |
| `BIN_WS_LOGIN_HOST` | `pre-api.riverbit.io` | Signed login host; must match the server's public host |
| `API_GATEWAY_URL` | `https://pre-api.riverbit.io` | HTTP base for `/info` and REST `/exchange` |
| `ORDER_TRANSPORT` | `ws` | `trade` / `recover` transport: `ws` or `rest` |
| `COIN` | `RBT` | Market used to validate the asset index |
| `BOOK_STREAM` | `l2` | `l2` incremental or `l2Book` full book |
| `GUILD_VAULT_ADDRESS` | empty | Vault target; otherwise use the master account |
| `KEY_FILE` | `key` | API wallet private-key file |
| `STATE_DIR` | `.guild-mm-state` | Persistent nonce and pending journal; processes using the same signer must share it |
| `ACTION_FILE` | empty | Comma-separated action files, required for `trade`; ignored by `recover` |
| `SIGN_CHAIN_ID` | empty | Optional check against `/info` `chainIdentity.nextAgentChainId` (currently 12589) |

Deprecated `WS_URL` and `TRADE_WS_URL` are ignored and produce a warning. See `internal/config/config.go` for the authoritative configuration.

For `trade`, both transports use HTTP `/info` for startup identity, market and vault checks. WS uses login to resolve the account, subscribes to order events, sends `action_json`, and reconciles via `info_json`; REST uses HTTP `userRole`, `/exchange` and `/info`, without subscribing. Both send the same signed request body. Market-data mode always uses bin-ws. `recover` resolves the account and queries orders without the pre-submission signing-domain, market-parameter and vault-execution checks.

## Troubleshooting

- **Code 91:** check API-wallet authorization and compare signing against the guide's vectors.
- **Code 1160:** every Guild vault limit order must use `Alo`; the entire action is rejected otherwise.
- **Minimum notional / `PrecisionExceeded`:** adjust price and size; use `szDecimals` from `metaAndAssetCtxs` and the guide's price rules. One invalid leg can reject the batch.
- **Timeout or disconnect:** preserve `STATE_DIR` and use `recover`. No automatic resend or time-based pending release occurs. Unknown is not a rejection.
- **Changing transport:** pending records are bound to the original transport; reconcile them before switching.
- **Market-data disconnect:** the market-data loop reconnects with a 1–15 second backoff, logs in and resubscribes. Requery after missed events; `openOrders` only shows current open orders and does not establish fills or unknown request outcomes.
- **Invalid order-book data:** an invalid delta clears the book and resubscribes; an undecodable outer frame closes the connection for the market-data loop to reconnect.

## Local verification

```sh
go test -race ./...
go vet ./...
```

This repository demonstrates PRE API integration. Trading examples require your own authorized test account and parameters adjusted to the live market.

## License

Licensed under the [Apache License 2.0](LICENSE). Third-party dependencies retain their respective licenses.
