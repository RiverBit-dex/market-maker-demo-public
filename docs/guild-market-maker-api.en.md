# Market Maker API Integration Guide

[中文](guild-market-maker-api.md) | English

This guide covers market and account queries, signing and submitting orders, cancellations and modifications, and subscribing to market data and order events over binary WebSocket (`bin-ws`). Markets, prices, addresses and oids in examples are placeholders; use your own account and target market. The [demo README](../README.md) explains how to run the example.

## 1. Endpoints and capabilities

| Purpose | PRE endpoint | Protocol |
|---|---|---|
| Queries | `https://pre-api.riverbit.io/info` | HTTP POST JSON |
| Trading | `https://pre-api.riverbit.io/exchange` | HTTP POST signed JSON |
| Market data, events, trading and info | `wss://pre-api.riverbit.io/bin-ws` | WebSocket binary Protobuf frames |

Replace the host when switching environments. REST `/info` supports metadata, book and account queries; `/exchange` accepts signed actions. bin-ws supports `l2`, `l2Book`, `bbo`, `trades`, `candle`, account subscriptions and `info_json` queries. Its `action_json` uses the same signed request bytes as REST. `metaAndAssetCtxs`, `userRole` and `leadingVaults` must use REST.

All `/info` queries are POST JSON with a `type` field:

| Type | Other fields | Purpose |
|---|---|---|
| `chainIdentity` | none | `nextAgentChainId` for signing |
| `userRole` | `user` | Resolve an API wallet to its master account |
| `leadingVaults` | master `user` | Vaults led by the master |
| `metaAndAssetCtxs` | none | Asset index and `szDecimals` |
| `l2Book` | `coin` | Book snapshot; no `nSigFigs` or `mantissa` |
| `candleSnapshot` | `req`: `coin`, `interval`, `startTime`, `endTime` | Historical candles |
| `openOrders`, `frontendOpenOrders`, `userFills`, `clearinghouseState` | `user` | Orders, fills and positions |
| `orderStatus` | `user`, `oid` | Single order status |

An API wallet signs; the master address identifies the account for queries and subscriptions. To trade for a vault, include top-level `vaultAddress`; use the vault address as `user` for vault queries and subscriptions. The vault must be led by the master, and the signer must be the leader or its authorized API wallet. Do not confuse signer and master addresses. `userRole` returns `agent` with `data.user`, `user`, `subAccount` with `data.master`, `vault`, or `missing`. Vault classes include `protocol`, `guild`, `priority` and `user`.

## 2. Integration flow

1. Query `chainIdentity` and record `nextAgentChainId`.
2. Query `userRole` with the API wallet; record the master address. For vault trading query `leadingVaults` for the master.
3. Query `metaAndAssetCtxs` on each startup. The target's index in `universe` is order field `a`; `szDecimals` determines precision. Do not hard-code the index.
4. Connect to bin-ws, log in as the first frame, then subscribe to market and order/fill streams.
5. Construct and sign an action; submit over REST or bin-ws. Inspect every receipt leg and reconcile using order updates or `orderStatus`.

## 3. Signing

Trading uses Hyperliquid-style L1 phantom-agent EIP-712 signing with the chain ID and source below. The vectors in Appendix B permit offline verification.

### 3.1 Request and MessagePack

```json
{"action":{"type":"order","orders":[{"a":1,"b":true,"p":"80000","s":"0.001","r":false,"t":{"limit":{"tif":"Alo"}}}],"grouping":"na"},"nonce":1792168694973,"signature":{"r":"0x<64 hex digits>","s":"0x<64 hex digits>","v":27}}
```

Add top-level `vaultAddress` for vault trading. The node converts the received action JSON to MessagePack in **received key order**. Sign and send the same order; do not reserialize after signing. Strings are not normalized: `"52.000"` and `"52"` have different signing images even though both are accepted. Nonnegative integers use the most compact uint encoding; strings use str, booleans use bool and numbers with decimal points use float64. **Price and size must be strings.** Duplicate keys and fields outside the action's fixed schema are rejected. `order` allows `type`, `orders`, `grouping`, optional `builder`; each order allows `a`, `b`, `p`, `s`, `r`, `t`, optional `c`.

### 3.2 connectionId and EIP-712

```text
connectionId = keccak256(msgpack(action) || nonce as 8-byte big-endian || tail)
```

Without a vault the tail is one byte `0x00` and the request must omit `vaultAddress`. With a vault the tail is `0x01` followed by the 20-byte vault address and the request includes `vaultAddress`.

| EIP-712 field | Value |
|---|---|
| Domain | `name="Exchange"`, `version="1"`, `chainId=chainIdentity.nextAgentChainId`, zero `verifyingContract` |
| Primary type | `Agent` |
| Type string | `Agent(string source,bytes32 connectionId)` |
| Message | `source="b"`, calculated `connectionId` |

Use `nextAgentChainId` (PRE currently 12589), including when adapting an SDK with a hard-coded chain ID. `source` is currently `"b"` in all environments. `signatureChainId` is for user-signed actions, not trading actions. `r` and `s` are hexadecimal strings with optional `0x`, case insensitive; the node left-pads values shorter than 64 hex digits but truncates longer values, breaking verification. `v` is JSON number 27 or 28. A signing mismatch may recover an unknown address and return code 91.

### 3.3 Nonce and unknown results

Use millisecond timestamps in `[block time - 48 hours, block time + 24 hours]`. Deduplication is per signer. Each signer retains its most recent 100 nonces; after the set fills, a new nonce must exceed its minimum. Concurrent requests from one signer need distinct nonces.

**Never re-sign an unknown action with a new nonce.** Timeout, disconnect, `unknownOid`, an empty open-order list, `action already pending`, and duplicate nonce code 205 do not establish the original action's per-leg outcome. The demo retains pending and uses `recover` to query. Obtain sufficient execution evidence before deciding what to do; do not erase pending or infer nonexecution from time or absence of open orders.

## 4. bin-ws connection and login

Every binary WebSocket message is a Protobuf `WsV2ClientFrame` or `WsV2ServerFrame`; complete definitions are in Appendix A. `action_json` and `info_json` replies carry the request's nonzero outer `id`; pushes, subscription acknowledgments and pong have id 0. Use unique nonzero IDs for action/info and match replies by ID. Use a long-lived connection for login, subscriptions and orders; each new connection needs its own login and consumes connection quota.

The **first frame must be `WsV2Login` within 10 seconds**. Sign these four lines with EIP-191 `personal_sign`, separated by newline with no final newline:

```text
riverbit-ws-v2-login
host:pre-api.riverbit.io
timestamp:<milliseconds>
nonce:<16 random bytes in lowercase hex without 0x>
```

`timestamp_ms` must be within ±30 seconds of server time; nonce must not be reused. The login frame carries the 16 raw nonce bytes and 65-byte `r || s || v` signature; login accepts `v` 27/28 or 0/1. `host` must match the public server hostname. The server resolves `agent` to its master and `user` to itself. Other roles, invalid signatures or timeout fail login and disconnect. Authorization is rechecked every five minutes; revocation disconnects the wallet. See Appendix B.1 for a vector.

Account queries and subscriptions may target only the master or vaults it leads; market subscriptions are unrestricted. Every trading action still requires its own signature. `info_json` uses the same body as REST `/info`, and returns `payload.type` and `payload.data`. It is answered by a read-only non-validator and typically trails REST by up to two blocks; a fresh order may initially show old state or `unknownOid`. Only the following query types and their defined fields are accepted:

- Public: `meta`, `spotMeta`, `exchangeStatus`, `chainIdentity`, `allMids`, `l2Book`, `candleSnapshot`, `fundingHistory`.
- Account: `clearinghouseState`, `spotClearinghouseState`, `openOrders`, `frontendOpenOrders`, `orderStatus`, `userFills`, `userFillsByTime`, `historicalOrders`, `userFees`, `userFunding`, `userNonFundingLedgerUpdates`.

## 5. Subscriptions and typed frames

`subscribe_json` and `unsubscribe_json` contain the UTF-8 JSON subscription object itself, without a `method` wrapper. Success yields a JSON `subscriptionResponse` with outer id 0.

| Stream | Subscription example | Frame / behavior |
|---|---|---|
| Incremental book | `{"type":"l2","c":"BTC"}` | `l2_delta`: snapshot then updates |
| Full book | `{"type":"l2Book","coin":"BTC"}` | `l2_book`; optional `nSigFigs` 2–5 and `mantissa` |
| Best bid/offer | `{"type":"bbo","coin":"BTC"}` | `bbo` |
| Trades | `{"type":"trades","coin":"BTC"}` | `trades` |
| Candles | `{"type":"candle","coin":"BTC","interval":"1m"}` | `candle`; intervals `1m 3m 5m 15m 30m 1h 2h 4h 8h 12h 1d 3d 1w` |
| Orders, fills, positions | `{"type":"openOrders","user":"<master-or-vault>"}` | JSON; also `orderUpdates`, `userFills`, `clearinghouseState` |

`l2` is a RiverBit extension and uses `c`, not `coin`; optional numeric `s` has the meaning of `nSigFigs`. The 25 supported subscriptions are `allMids`, `notification`, `webData3`, `twapStates`, `clearinghouseState`, `openOrders`, `candle`, `l2Book`, `trades`, `orderUpdates`, `userEvents` (channel `user`), `userFills`, `userFundings`, `userNonFundingLedgerUpdates`, `activeAssetCtx`, `activeAssetData`, `userTwapSliceFills`, `userTwapHistory`, `bbo`, `spotState`, `allDexsClearinghouseState`, `allDexsAssetCtxs`, `outcomeMetaUpdates`, `fastAssetCtxs`, `l2`.

`l2`, `l2Book`, `bbo`, `trades` and `candle` use typed frames; other streams use JSON. If precision or counts cannot be represented in typed form, those streams can fall back to JSON, so clients must support both. Typed prices and sizes are integers divided by `10^decimals`, using each frame's `px_decimals` and `sz_decimals`. Book arrays correspond by index. Empty BBO sides omit fields. Candle timestamps are milliseconds; trade `buy` indicates the aggressor and buyer/seller are 20-byte addresses.

For `l2`, replace the entire book on snapshot. For an update, remove `bid_removed` and `ask_removed` as sets of indices into the **previous** book; then overwrite or insert levels by price and sort bids descending, asks ascending. Update decimals must match the latest snapshot; otherwise unsubscribe and resubscribe. Optional spread is ask minus bid in `px_decimals` and appears when `s` was supplied.

## 6. Orders

```json
{"type":"order","orders":[{"a":1,"b":true,"p":"80000","s":"0.001","r":false,"t":{"limit":{"tif":"Alo"}},"c":"0x00000000000000000000000000000001"},{"a":1,"b":false,"p":"82000","s":"0.001","r":false,"t":{"limit":{"tif":"Alo"}},"c":"0x00000000000000000000000000000002"}],"grouping":"na"}
```

`a` is asset index, `b` buy when true, `p` price and `s` size as decimal strings, `r` reduce-only, `t.limit.tif` is `Alo` (maker-only; immediately marketable orders are rejected) or `Gtc`, and `grouping` is `na`. Optional `c` is a lowercase `0x` plus 32 hex digits; it can be used for queries and cancellation. Preserve exact field order for signing.

Perpetual order constraints:

- Size decimals must not exceed `szDecimals` (examples BTC 5, ETH 4, SOL 2). Price decimals must not exceed `6 - szDecimals` and price has at most five significant digits; integer prices are exempt from the significant-digit limit, and trailing zeroes do not count. `PrecisionExceeded` yields only one status and allocates no oid to any leg of the action.
- Price must be a multiple of tick size. Decimal strings cannot be `.5`, negative, scientific notation, prefixed with `+`, or contain spaces; invalid values return `BadDecimal`. Size cannot be zero; price must not be too far from oracle.
- Notional is valuation price times size: for buys use min(limit, best ask); for sells max(limit, best bid). At or below $9.999999 is rejected; exactly $10 passes. Modifications are subject to the same check.
- Every limit order submitted on behalf of a Guild vault must use `Alo`, including replacement orders. A `Gtc` leg rejects the **entire action** with code 1160 and no statuses; master-account orders have no such Guild restriction.

Send the signed body via POST `/exchange` or as UTF-8 `action_json` with a nonzero frame id. Inspect **every** `statuses[]` leg. `resting` means placed with no fills; `filled` includes decimal `totalSz` and `avgPx`, but may still have remaining open size, so use `orderStatus`; `waitingForTrigger` / `waitingForFill` need tracking; an `error` is a rejected leg. Whole-action rejections have `status=err` and no `statuses`. An incomplete receipt rendering may include `localCommitted` and does not prove failure: query before deciding the next step, and never simply resend under a new nonce.

## 7. Cancel

Sign and submit cancellations like orders, including `vaultAddress` for a vault. By oid: `{"type":"cancel","cancels":[{"a":1,"o":123456789}]}`. By cloid: `{"type":"cancelByCloid","cancels":[{"asset":1,"cloid":"0x00000000000000000000000000000001"}]}`. The field names differ; cloid is signed as its exact string. `statuses[]` matches `cancels[]` in order: `"success"` or an error, for example when the order was never placed, already canceled or filled. Retain the receipt and confirm the actual target state; target termination alone does not prove the cancellation request reached a final outcome.

## 8. Modify

A replacement must be a complete order satisfying the same precision and Guild rules. The old oid becomes canceled and the replacement has a new oid. Supply the original cloid in the replacement to preserve it. Prefer `batchModify`, even for one order, because its response includes the new oid:

```json
{"type":"batchModify","modifies":[{"oid":123456789,"order":{"a":1,"b":true,"p":"80100","s":"0.001","r":false,"t":{"limit":{"tif":"Alo"}},"c":"0x00000000000000000000000000000001"}}]}
```

`statuses[]` corresponds to `modifies[]`; successful `resting.oid` is the new oid. A rejected replacement has an error prefixed `Error placing new order during modify:`. Single `modify` uses `{"type":"modify","oid":...,"order":{...}}`; acceptance returns `{"status":"ok","response":{"type":"default"}}` without the new oid. Query `orderStatus` by cloid or `frontendOpenOrders` to find it. Acceptance is not finality.

## 9. Queries and reconciliation

`openOrders` returns `coin`, `side` (`B` buy, `A` sell), `limitPx`, remaining `sz`, `oid`, and `timestamp`. Use `frontendOpenOrders` for `cloid`, `origSz` and `tif`. An empty array means no current open orders, not that a request never executed. Compare prices numerically: string formatting can differ between APIs.

`orderStatus` accepts a numeric oid or lowercase `0x` plus 32 hex digits cloid; uppercase cloid or numeric oid encoded as a string returns 400. A cloid query resolves the current order after modification. Results can be `status=order` with height and order, `unknownOid` with height, or `legResult` with height but no order object. `unknownOid` is not proof of nonexecution.

Use `order.status` for current state. Known values include `open`; terminal `filled`, `canceled`, `rejected`; rejected `tickRejected`, `minTradeNtlRejected`, `reduceOnlyRejected`, `badAloPxRejected`, `iocCancelRejected`, `perpMarginRejected`, `oracleRejected`, `insufficientSpotBalanceRejected`; and system cancellations `selfTradeCanceled`, `reduceOnlyCanceled`, `siblingFilledCanceled`, `scheduledCancel`, `vaultWithdrawalCanceled`. `triggered` requires further reconciliation of the resulting order; it does not prove successful cancellation or zero exposure. Do not treat an unknown status as terminal.

`leg` is the latest operation result for an oid, not the current order state: a canceled order can still have a `resting` leg. `leg.filled` quantities are raw integers, unlike decimal strings in the order receipt. A `legResult` with no current order cannot prove no remaining open quantity. Saved recovery evidence is historical, not a complete consistent order/account snapshot.

`orderUpdates` only contains changes after subscription, not an initial snapshot. Obtain `openOrders` separately, associate updates by oid and use `userFills` for execution price and quantity. For each write, inspect all receipt legs, check the appropriate current order state and open orders; for modification confirm old and new oids. Unknown results remain pending.

## 10. Errors, limits and reconnect

`/info` returns 400 for missing fields, unknown type, invalid address or malformed JSON; `l2Book` with `nSigFigs` or `mantissa` returns 501. Unknown coin behavior varies: `l2Book` 404, `fundingHistory` 200 with `[]`, `candleSnapshot` 400. Use bin-ws subscriptions for price-bucketed books.

Whole-action execution errors include code 91 (unknown signer/user or bad signing image), 1160 (Guild requires Alo), 203 (nonce too small), 204 (nonce too large), 205 (duplicate nonce), and 97 (too many pending nonces). A duplicate or `action already pending` still requires reconciliation of the original request.

REST limits are per client IP; the per-minute budget is supplied operationally. 429 returns `Retry-After`; excess in-flight requests return 503; bodies over 2 MiB return 413. `/info` weight is 2 for `l2Book`, `allMids`, `clearinghouseState`, `orderStatus`, `spotClearinghouseState`, `exchangeStatus`; 60 for `userRole`; 20 for other types. `/exchange` weight is `1 + floor(count / 40)` for `order`, `cancel`, `cancelByCloid`, `batchModify`, and 1 for `modify`. Query `userRole` only at startup due to its weight. Do not treat a generic HTTP failure as proof of rejection.

bin-ws quotas per IP: 10 simultaneous connections, 30 new connections/minute, 1000 subscriptions, 10 distinct subscribed users, 2000 client messages/minute, 100 outstanding action/info requests, and 1 MiB per message. Rate excess can send an error then disconnect; excess outstanding requests receive 429; oversized messages disconnect. The server disconnects after 60 seconds with no server-to-client message. On quiet connections send a ping about every 30 seconds; pong is a JSON frame with id 0.

After a disconnect, reconnect and log in, resubscribe and wait for acknowledgments, query open orders and rebuild local state, then process updates. **Keep unknown requests pending and reconcile; do not resend them.** Open-order snapshots do not by themselves establish request finality or a consistent account snapshot.

## 11. Execution and reconciliation for continuous market making

These recommendations draw on the maker executor's lifecycle, durable recovery, snapshot checks and risk budgets. They describe responsibilities for your own continuous maker. The demo does not implement automatic quoting, complete financial reconciliation or every internal execution feature. Internal legacy endpoints, numeric error codes and deployment settings do not apply to this public protocol.

### 11.1 Track requests separately from orders

| Local request phase (not an API enum) | Required handling |
|---|---|
| Prepared | Allocate unique nonce/cloids and persist action, signer, master/vault, network, endpoint and targets before sending. A journal entry does not prove submission. |
| Sent, outcome unknown | Retain pending after timeout, disconnect, malformed replies or lagging queries. Block conflicting writes and reserve possible exposure. |
| Per-leg execution observed | Save each success/rejection, oid/cloid and observed query height. Partial success is not rolled back; never replay the entire batch. |
| Current state reconciled | Read current orders, fills, position and available funds before recomputing targets. Historical success does not mean an order is still open. |
| Explicit rejection | Correct the cause and reevaluate before creating a new request with a new nonce; retain the audit trail. |

One action may create many orders; an order may subsequently have modify and cancel requests. Request completion and order termination are different states. A `filled` receipt leg can leave a resting remainder. Use current `order.status` and remaining `sz` for live state, and fill records for executed price and size. Do not wait for `open` when an order has already filled.

Coordinate nonce allocation across every process using a signer. Prefer one serial execution owner per trading account/market so event handlers, periodic refresh and recovery cannot duplicate mutations. The demo's shared `STATE_DIR` and local file locks are not a multi-host coordinator.

### 11.2 Place, cancel, modify and refill

| Situation | Execution and reconciliation |
|---|---|
| Routine quoting | Retain old orders whose price, side and remaining size still satisfy the target; change only the difference. Use exact decimal or fixed-point arithmetic. |
| Immediate fill | Confirm fills and any resting remainder rather than waiting for `open`; refresh inventory and funds. |
| Partial fill | Measure depth by confirmed remaining size, not order count or original size. Recompute the entire side's risk budget before refilling. |
| Cancel/fill race | Reserve exposure until the old order is confirmed inactive. A filled target alone does not prove this cancellation request completed; retain and reconcile its receipt. |
| Modify | Track old → new oid and cloid mappings. Match replacements to same-side orders, not positions in an oid-sorted list. Re-query the old order for rejected legs. |
| Partial batch failure | Record successes and rejections separately; construct subsequent actions only for newly evaluated gaps. Never replay successful legs. |
| Unknown result | Retain the journal and already observed per-leg evidence. `unknownOid`, an empty book, elapsed time and a consumed nonce do not prove nonexecution. |

Give risk cancellation priority over placement and reserve independent API capacity for cancellation and recovery. A per-side fixed cooldown can start when a remaining-size decrease or disappearance is first confirmed; subsequent notifications should not keep extending its deadline. At expiry, reread inventory, funds and prices. Disappearance indicates a depth gap, not necessarily a fill. Your strategy supplies cooldown, maximum order age and repricing thresholds; the demo has no strategy defaults.

### 11.3 Snapshots, events and exposure

Treat pushes as prompts to reconcile rather than a standalone financial ledger. After reconnecting, log in and resubscribe, buffer updates arriving during recovery, query orders, fills and account state, then deduplicate and reconcile using identity and available event identifiers. Do not assume snapshots and events form a gap-free stream or treat query `height` as the original transaction's execution height.

Persist the minimum query height of accepted execution evidence and reject older projections. Ordinary `openOrders` has no snapshot height, and independent reads do not guarantee one committed view. Pause additional exposure if evidence is insufficient; never invent a height. Internal consistent snapshots and execution proofs are not implied public API capabilities.

Budget the entire side against positions, all live orders, cancel-pending orders, unknown placements/replacements and proposed new orders. An ordinary sell is not necessarily risk-reducing. Do not spend the same inventory or collateral headroom independently at every level. Stale prices, crossed books, disabled trading, mismatched account/market identity or unavailable funds must block new quotes. Cancel known risk orders where authorization and execution state permit, retaining unresolved records.

### 11.4 Choose the next action by error class

| Signal | Classification and response |
|---|---|
| `91` | Check API wallet authorization, master identity, signing field order and dynamic chain ID. Do not universally interpret it as missing funds or automatically call a faucet. |
| `1160` | Guild orders and replacements must use `Alo`; correct offending legs and reevaluate before submitting. |
| `203` / `204` | Check millisecond clock, nonce window and allocator. Resolve only the explicitly rejected request; retain other unknown requests. |
| `205` / `action already pending` | Reconcile the original action; do not copy it with a new nonce. |
| `97` | Reduce in-flight concurrency and prioritize recovery; retain existing request state. |
| `PrecisionExceeded` / `BadDecimal` / tick / minimum-notional rejection | Correct precision, tick, step or notional. The single-status `PrecisionExceeded` response is not an ordinary per-leg array. |
| `badAloPxRejected` / `oracleRejected` | Reread prices, book and trading state before repricing; do not automatically switch to a taker order. |
| `perpMarginRejected` / `reduceOnlyRejected` / insufficient balance | Reread positions and funds, reduce size or adjust side; stop resubmitting the original size. |
| System cancellation | Reconcile the self-trade, position, related-order or vault condition before refilling; do not book every disappearance as a fill. |
| HTTP 429/503/5xx, timeout, disconnect, malformed reply | Reads may back off and retry. For writes, retain pending and recover by querying; HTTP status alone is not proof of nonexecution. |
| `status:"err"` with mixed `committedResult`, `localCommitted` or a query-before-retry hint | Effects may already exist. Save the complete receipt and reconcile every leg instead of rejecting the whole batch locally. |
| Unknown code or order state | Retain diagnostics, pause conflicting writes and seek support; do not infer success, termination or safe replay. |

An explicit rejection without potentially effective results can end that request's pending state. Inspect `committedResult` and every leg before classifying it. A duplicate-nonce rejection applies to this submission, not proof of the original action's outcome. Follow sections 6–10 for field details and error scope. Log request identity, nonce, account/market, operation, oid/cloid, per-leg outcomes, observed height and phase durations; never log private keys or complete replayable signed requests.

### 11.5 Restart, shutdown and monitoring

On restart, verify original signer, account, endpoint, network and durable directory, recover unknown requests, then rebuild orders and risk state. Changing WS/REST, account or instance must not bypass pending. The demo's `recover` only queries and never replays `ACTION_FILE`.

A continuous maker should stop new quotes, retain unknown requests, cancel known orders in its own management scope and verify outcomes before reporting cleanup complete. Exiting a process or closing WS does not cancel orders. This demo does not implement automatic cancellation on strategy shutdown; orders left by `trade` need explicit cancellation. Report incomplete cleanup if cancellation or queries fail; successful process exit is not zero account exposure.

Monitor pending count/age, market/account data age, per-leg rejection reasons, rate limits, disconnects, remaining depth, inventory and unconfirmed risk-cancellation age. Measure submission → observed execution and observed execution → current visibility separately. Report unresolved requests and quoting downtime alongside successful-request latency so stalled work is visible.

## Appendix A: bin-ws Protobuf definitions

Use `syntax = "proto3"; package riverbit.api.v1;` with these messages to generate client code.

```proto
message WsV2ClientFrame {
  // Request id for post bodies (action/info); ignored otherwise.
  uint64 id = 1;
  oneof body {
    // Exact signed action JSON bytes (the same body as REST POST /exchange).
    bytes action_json = 2;
    // Info payload JSON, for example {"type":"openOrders","user":"0x..."}.
    bytes info_json = 3;
    // Subscription object JSON, for example {"type":"candle","coin":"BTC","interval":"1m"}.
    bytes subscribe_json = 4;
    bytes unsubscribe_json = 5;
    WsV2Ping ping = 6;
    WsV2Login login = 7;
  }
}
message WsV2Ping {}

// First frame of every /bin-ws connection. EIP-191 personal_sign over
// "riverbit-ws-v2-login\nhost:<host>\ntimestamp:<timestamp_ms>\nnonce:<hex>".
message WsV2Login {
  uint64 timestamp_ms = 1;
  bytes nonce = 2;
  // 65 bytes r || s || v.
  bytes signature = 3;
}

// One binary WebSocket message on /bin-ws, server to client.
message WsV2ServerFrame {
  // Request id for frames caused by a post request; 0 for pushes and acks.
  uint64 id = 1;
  oneof body {
    // A complete JSON frame, byte-identical to the JSON endpoint.
    bytes json = 2;
    L2BookFrame l2_book = 3;
    L2DeltaFrame l2_delta = 4;
    BboFrame bbo = 5;
    CandleFrame candle = 6;
    TradesFrame trades = 7;
    WsV2LoginResult login = 8;
  }
}
message WsV2LoginResult {
  bool ok = 1;
  // Resolved master account, 20 bytes.
  bytes user = 2;
  // Recovered signer: the API wallet or the master account itself, 20 bytes.
  bytes signer = 3;
  string error = 4;
}

// Fixed point: value = integer / 10^decimals, decimals shared by the frame.
// Parallel packed arrays per side have equal length.
message L2BookFrame {
  string coin = 1;
  uint64 time = 2;
  uint32 px_decimals = 3;
  uint32 sz_decimals = 4;
  repeated uint64 bid_px = 5;
  repeated uint64 bid_sz = 6;
  repeated uint32 bid_n = 7;
  repeated uint64 ask_px = 8;
  repeated uint64 ask_sz = 9;
  repeated uint32 ask_n = 10;
}
// Channel "l2": a snapshot, then updates against the previous book. Decimals
// are locked per subscription by the latest snapshot; a snapshot may arrive at
// any time and replaces the whole book.
message L2DeltaFrame {
  oneof kind {
    L2BookFrame snapshot = 1;
    L2DeltaUpdate update = 2;
  }
}
message L2DeltaUpdate {
  string coin = 1;
  uint64 time = 2;
  uint32 px_decimals = 3;
  uint32 sz_decimals = 4;
  // New or size-changed levels, in current book order.
  repeated uint64 bid_px = 5;
  repeated uint64 bid_sz = 6;
  repeated uint64 ask_px = 7;
  repeated uint64 ask_sz = 8;
  // Removed levels as indices into the previous book; remove as one set.
  repeated uint32 bid_removed = 9;
  repeated uint32 ask_removed = 10;
  optional uint64 spread = 11;
}
message BboFrame {
  string coin = 1;
  uint64 time = 2;
  uint32 px_decimals = 3;
  uint32 sz_decimals = 4;
  optional uint64 bid_px = 5;
  optional uint64 bid_sz = 6;
  optional uint32 bid_n = 7;
  optional uint64 ask_px = 8;
  optional uint64 ask_sz = 9;
  optional uint32 ask_n = 10;
}
message CandleFrame {
  string coin = 1;
  string interval = 2;
  uint64 open_time = 3;
  uint64 close_time = 4;
  uint32 px_decimals = 5;
  uint32 sz_decimals = 6;
  uint64 open = 7;
  uint64 close = 8;
  uint64 high = 9;
  uint64 low = 10;
  uint64 volume = 11;
  uint32 trade_count = 12;
}
message TradesFrame {
  string coin = 1;
  uint32 px_decimals = 2;
  uint32 sz_decimals = 3;
  repeated Trade trades = 4;
}
message Trade {
  bool buy = 1;
  uint64 px = 2;
  uint64 sz = 3;
  uint64 time = 4;
  uint64 tid = 5;
  bytes hash = 6;
  bytes buyer = 7;
  bytes seller = 8;
}
```

## Appendix B: signing test vectors

Trading signature vectors below retain their original asset index and prices for offline verification; they are not BTC submission examples.


These vectors were computed independently in Python and Go with matching results. All use the same test key:

| Item | Value |
|---|---|
| Private key | `0x0101010101010101010101010101010101010101010101010101010101010101`(32 bytes of `0x01`) |
| Address | `0x1a642f0E3c3aF545E7AcBD38b07251B3990914F1` |

**This private key is public and is for offline tests only. Never transfer assets to this address.**

### B.1 bin-ws login (EIP-191)

Signing text (four lines separated by `\n`, with no final newline):

```
riverbit-ws-v2-login
host:pre-api.riverbit.io
timestamp:1792168694973
nonce:000102030405060708090a0b0c0d0e0f
```

| Item | Value |
|---|---|
| `WsV2Login.timestamp_ms` | `1792168694973` |
| `WsV2Login.nonce`(16 bytes) | `000102030405060708090a0b0c0d0e0f` |
| `WsV2Login.signature`(65 bytes r‖s‖v) | `4752b41eb2baa20a3e169891002152aa388130e129b3c8dbd4f957e03a1cddef7f1e12c690cbc3a8ec00f287739ea28e2a8d10ba6073353074735cc6fe56c23f1b` |

### B.2 Trading signature (EIP-712)

Common inputs:

- `chainId` = `12589`,`source` = `"b"`
- `nonce` = `1792168694973`
- action JSON (these exact bytes and key order):

```json
{"type":"order","orders":[{"a":0,"b":true,"p":"100.5","s":"0.2","r":false,"t":{"limit":{"tif":"Alo"}}}],"grouping":"na"}
```

- `msgpack(action)`:

```
83a474797065a56f72646572a66f72646572739186a16100a162c3a170a53130302e35a173a3302e32a172c2a17481a56c696d697481a3746966a3416c6fa867726f7570696e67a26e61
```

**Vector 1: no vault** (connectionId tail `0x00`)

| Item | Value |
|---|---|
| connectionId | `0x0260ebaee4220bfd192a49fad29e62a1f9890f5b70e89a095e8099686b4805a6` |
| EIP-712 digest | `0x724d79bc83f9bb39300af824a91d9e5a84028b1f15b90c77d1e3212f5290d272` |
| r | `0xb075d6ff3fe5bfa399a22ecfca3375af8223181b21a363bde243ce9186edd7ec` |
| s | `0x01f82100252650e8ddde4815134d4e0420afaca71b09ec03c278f0a4985cf552` |
| v | `27` |

**Vector 2: vault** (`vaultAddress` = `0x1111111111111111111111111111111111111111`, connectionId tail `0x01` plus vault address)

| Item | Value |
|---|---|
| connectionId | `0x749d2e6560886f2092bcee6423609c41dc20e47d17ad9a50d5483e4165bc3d39` |
| EIP-712 digest | `0xe61bf62709ae5db02f460316839fbd16baf772a6abc096b7d44e5773f125d272` |
| r | `0xc69888725b054488375d419e27dea48f41f2f8da5ec863adf9af1e1d31f1c071` |
| s | `0x1adb693fdc1e45c68f4c39f4f868f731bcaaa11eb159fb2a5d46ebe8843aff24` |
| v | `28` |

Vector 1 has a leading zero in `s`. Some libraries omit it (`0x1f8210…`); this is equivalent, and the node left-pads it.
