# 做市商 API 接入手册

中文 | [English](guild-market-maker-api.en.md)

本手册说明做市商如何用 API wallet 接入 RiverBit:查询市场与账户、签名并提交下单/撤单/改单、通过二进制 WebSocket(bin-ws)订阅行情和订单推送。文中的市场(BTC)、价格、地址和订单号都只是格式示例，接入时换成你自己的账户和目标市场。

目录

1. 地址与能力
2. 接入流程
3. 签名
4. bin-ws 连接与登录
5. 订阅与强类型帧
6. 下单
7. 撤单
8. 改单
9. 查询与跟踪
10. 错误、限频与重连
11. 持续做市的执行与对账建议

- 附录 A:bin-ws Protobuf 定义
- 附录 B:签名测试向量

## 1. 地址与能力

### 1.1 接口地址(预发环境)

切换环境时，把下表的域名换成目标环境的域名。

| 接口 | 地址 | 协议 | 用途 |
|---|---|---|---|
| REST 查询 | `https://pre-api.riverbit.io/info` | HTTP POST + JSON | 查询市场、盘口、账户、挂单、订单状态。body 的 `type` 指定查询类型。 |
| REST 交易 | `https://pre-api.riverbit.io/exchange` | HTTP POST + JSON | 提交已签名的下单、撤单、改单。`action.type` 指定操作。 |
| bin-ws | `wss://pre-api.riverbit.io/bin-ws` | WebSocket,每条消息是一个 Protobuf binary frame | 登录后在同一条连接上订阅行情和账户推送、提交已签名交易、发起 info 查询。 |

### 1.2 REST 与 bin-ws 的能力对照

| 能力 | REST | bin-ws |
|---|---|---|
| 市场元数据(asset index、`szDecimals`) | `/info` `metaAndAssetCtxs` | 不支持，只能走 REST |
| 盘口 | `/info` `l2Book` | 订阅 `l2`(增量)或 `l2Book`(全量);`info_json` 查 `l2Book` |
| 最优买卖价、逐笔成交、K 线 | `/info` `candleSnapshot`(历史 K 线) | 订阅 `bbo`、`trades`、`candle` |
| 挂单、订单状态、成交、持仓 | `/info` `openOrders`、`frontendOpenOrders`、`orderStatus`、`userFills`、`clearinghouseState` | 订阅 `openOrders`、`orderUpdates`、`userFills`、`clearinghouseState`;`info_json` 单次查询 |
| 下单、撤单、改单 | `/exchange` | `action_json`,请求体与 REST 逐字节相同 |

### 1.3 本手册用到的 Info 查询

所有查询都是 `POST /info`,body 是一个 JSON 对象，由 `type` 区分。

| `type` | 其他字段 | 用途 |
|---|---|---|
| `chainIdentity` | 无 | 取签名用的 `nextAgentChainId`(第 3.4 节) |
| `userRole` | `user` | 查地址的角色；API wallet 查到所属主账户(第 1.4 节) |
| `leadingVaults` | `user`(主账户) | 查主账户领导的金库(第 1.4 节) |
| `metaAndAssetCtxs` | 无 | 取 asset index 和 `szDecimals`(第 2 节) |
| `l2Book` | `coin` | 盘口快照。**不支持** `nSigFigs`/`mantissa`(第 10.1 节) |
| `candleSnapshot` | `req`:`{"coin","interval","startTime","endTime"}` | 历史 K 线 |
| `openOrders` | `user` | 当前挂单(精简字段，第 9.1 节) |
| `frontendOpenOrders` | `user` | 当前挂单(含 `cloid`、`origSz`、`tif`,第 9.1 节) |
| `orderStatus` | `user`、`oid` | 单笔订单状态(第 9.2 节) |
| `userFills` | `user` | 成交记录 |
| `clearinghouseState` | `user` | 合约账户状态与持仓 |

示例：

```sh
curl -sS https://pre-api.riverbit.io/info \
  -H 'Content-Type: application/json' \
  -d '{"type":"metaAndAssetCtxs"}'
```

### 1.4 三种地址：API wallet、主账户、金库

- **API wallet**:只用来签名(交易签名和 bin-ws 登录签名)。签名者也可以是主账户本身。
- **主账户**:账户查询、账户订阅里的 `user` 填主账户地址。
- **金库**:代金库交易时请求顶层带 `vaultAddress`(第 3.3 节);查询、订阅金库的数据时 `user` 填金库地址。只能操作主账户**领导**的金库，签名者是金库 leader 主账户本身或它授权的 API wallet。

不要混用 API wallet 地址和主账户地址。

`userRole` 的返回值：

| 返回 | 含义 |
|---|---|
| `{"role":"agent","data":{"user":"0x…"}}` | API wallet,`data.user` 是所属主账户 |
| `{"role":"user"}` | 普通用户(主账户) |
| `{"role":"subAccount","data":{"master":"0x…"}}` | 子账户,`data.master` 是母账户 |
| `{"role":"vault"}` | 金库地址 |
| `{"role":"missing"}` | 地址不存在 |

`leadingVaults` 返回主账户领导的金库列表：

```json
[{"class":"guild","equity":"561.650358","vaultAddress":"0x8add000000000000000000000000000000000000"}]
```

`class` 取值为 `protocol`、`guild`、`priority`、`user`。`class` 为 `guild` 的金库有下单限制，见第 6.3 节。

## 2. 接入流程

1. `POST /info` 查 `chainIdentity`,记下 `nextAgentChainId`(第 3.4 节)。
2. 用 API wallet 地址查 `userRole`,确认 `role` 是 `agent`,记下主账户地址。代金库做市时，再用主账户查 `leadingVaults`,拿到金库地址。
3. 查 `metaAndAssetCtxs`:响应第一个元素的 `universe` 数组按顺序对应 asset index,找到 `name` 等于目标市场(如 `"BTC"`)的元素，它的下标就是订单字段 `a`,它的 `szDecimals` 决定价格和数量精度(第 6.2 节)。每次启动都重新查询，不要写死。
4. 建立 bin-ws 连接，第一帧登录(第 4.2 节),然后订阅行情和订单/成交推送(第 5 节)。
5. 构造 action、签名(第 3 节),通过 REST `/exchange` 或 bin-ws `action_json` 提交(第 6.4 节)。
6. 逐项检查回包(第 6.5 节),并用订单推送或 `orderStatus` 核对结果(第 9 节)。

## 3. 签名

交易签名规则与 Hyperliquid 的 L1 action(phantom agent,EIP-712)相同，区别在 chain ID 和 `source`,见第 3.4 节。附录 B 提供测试向量，可以离线验证自己的实现。

### 3.1 签名请求体

```json
{
  "action": {"type": "order", "orders": [{"a": 1, "b": true, "p": "80000", "s": "0.001", "r": false, "t": {"limit": {"tif": "Alo"}}}], "grouping": "na"},
  "nonce": 1792168694973,
  "signature": {"r": "0x<64 hex digits>", "s": "0x<64 hex digits>", "v": 27}
}
```

代金库交易时，在顶层再加 `"vaultAddress": "0x<your-vault-address>"`。

### 3.2 action 的 msgpack

节点验签时，把收到的请求里 `action` 的 JSON **原样**转成 msgpack:

- **按收到的键序**编码，不重排。签名时用什么键序，发送时就用什么键序；签名后不要重新序列化 action。
- **字符串不做规范化**,按发送时的原文参与签名。例如 `"p":"52.000"` 和 `"p":"52"` 签名镜像不同；两种写法节点都接受，但签名和发送必须是同一种。
- 编码规则：非负整数用最紧凑的 uint;字符串用 str;bool 用 bool;带小数点的数字编码为 float64。**价格和数量必须用字符串**。
- 重复键会被拒。每类 action 只允许固定的字段集合，多一个或少一个都会被拒。例如 `order` 只允许 `type`、`orders`、`grouping`、`builder`(可选),每笔订单只允许 `a`、`b`、`p`、`s`、`r`、`t`、`c`(可选)。

### 3.3 connectionId

```
connectionId = keccak256( msgpack(action) ‖ nonce(8-byte big-endian) ‖ tail )
```

- 不代金库：尾部是 1 字节 `0x00`,请求里**不要**带 `vaultAddress`。
- 代金库：尾部是 `0x01` 加 20 字节金库地址，请求顶层带 `"vaultAddress":"0x…"`。

### 3.4 EIP-712

| 项 | 值 |
|---|---|
| domain | `name="Exchange"`、`version="1"`、`chainId=<chainIdentity.nextAgentChainId>`、`verifyingContract=0x0000000000000000000000000000000000000000` |
| primaryType | `Agent` |
| 类型串 | `Agent(string source,bytes32 connectionId)` |
| message | `source="b"`、`connectionId=<第 3.3 节的结果>` |

- `chainId` 一律取 `chainIdentity` 返回的 `nextAgentChainId`(预发当前是 12589)。从 Hyperliquid SDK 移植时，要把 SDK 里写死的 chain ID 换成这个值。
- `source` 当前所有环境都是 `"b"`。
- `chainIdentity` 里的 `signatureChainId`(`"0x312d"`)只用于 user-signed action,交易 action 不用。

### 3.5 signature 格式

- `r`、`s`:hex 字符串,`0x` 可带可不带，大小写均可。不足 64 位时节点会左补 0;**超过 64 位会被截断，导致验签失败**,所以必须不超过 64 位。
- `v`:JSON 数字，只接受 `27` 或 `28`。

签名镜像算错(键序、字符串、chain ID、尾部任一不一致)时，节点会还原出一个陌生地址，整个 action 被拒，返回 code 91(第 10.2 节)。

### 3.6 nonce

- 取毫秒时间戳，必须落在 `[块时间 − 48 小时, 块时间 + 24 小时]` 内。
- 按**签名者地址**去重(用 API wallet 签名时就是 API wallet 地址)。每个签名者保留最近 100 个 nonce;集满后，新的 nonce 必须大于保留集合里的最小值。同一签名者并发提交时，每个请求用不同的 nonce。
- 不合规时整个 action 被拒：`{"status":"err","response":"action rejected by execution (code 203)"}`。各码含义见第 10.2 节。

**超时恢复规则**:本 demo 提交后超时或连接断开时保留 pending，使用 `recover` 命令查询 `orderStatus` / `openOrders`，不会自动重发。**不要换新 nonce 重新签名**——换了 nonce 就是一笔新请求，原请求若已执行会造成重复下单。`unknownOid` 或空挂单列表都不能证明未执行，不能据此重发或删除 pending。

自行实现客户端时，如收到 `action already pending; it was not submitted again` 或 code 205(nonce 重复)，仍须核对原请求的订单及成交结果；这些响应不等于原 action 的逐笔执行回执，也不授权换 nonce 重试。

## 4. bin-ws 连接与登录

### 4.1 帧格式

连接 `wss://pre-api.riverbit.io/bin-ws` 后，每条 WebSocket binary message 是一个 Protobuf 消息：客户端发 `WsV2ClientFrame`,服务端回 `WsV2ServerFrame`(完整定义见附录 A)。手册其余部分的 JSON 是这些 `bytes` 字段里承载的内容，不是 WebSocket 文本帧。

```proto
message WsV2ClientFrame {
  uint64 id = 1;                 // Request ID for action/info; echoed in the outer reply
  oneof body {
    bytes action_json = 2;       // Exact signed transaction JSON bytes
    bytes info_json = 3;         // Info query JSON
    bytes subscribe_json = 4;    // Subscription object JSON
    bytes unsubscribe_json = 5;  // Unsubscription object JSON
    WsV2Ping ping = 6;           // Heartbeat (empty message)
    WsV2Login login = 7;         // Login; must be the first frame
  }
}

message WsV2ServerFrame {
  uint64 id = 1;                 // Request ID for action/info replies; 0 for pushes, acks and pong
  oneof body {
    bytes json = 2;              // Complete JSON frame: {"channel":...,"data":...}
    L2BookFrame l2_book = 3;     // Typed market data; see section 5.2
    L2DeltaFrame l2_delta = 4;
    BboFrame bbo = 5;
    CandleFrame candle = 6;
    TradesFrame trades = 7;
    WsV2LoginResult login = 8;   // Login result
  }
}
```

- `action_json`、`info_json` 的回包外层 `id` 等于请求的 `id`;推送、订阅确认、pong 的外层 `id` 都是 0。给每个 action/info 请求分配唯一的非零 `id`,按 `id` 匹配回包。
- **用一条长连接**完成登录、订阅和下单。不要每个请求新建连接：每条新连接都要重新登录，而且连接数有配额(第 10.4 节)。

### 4.2 登录

**连接后第一帧必须是 `WsV2Login`,且在 10 秒内发出**;登录前发送任何其他帧，连接会被断开。用 API wallet 私钥签名(也可以用主账户私钥),不需要另外的 WebSocket 密钥。

登录签名是 **EIP-191 `personal_sign`**,签下面四行文本，行之间用 `\n` 分隔，末尾没有换行：

```
riverbit-ws-v2-login
host:pre-api.riverbit.io
timestamp:<milliseconds>
nonce:<16 random bytes as lowercase hex without 0x>
```

```proto
message WsV2Login {
  uint64 timestamp_ms = 1;       // Same timestamp as in the signed text
  bytes nonce = 2;               // 16 raw bytes; lowercase hex in the signed text
  bytes signature = 3;           // 65 bytes r || s || v; v is 27/28 or 0/1
}
message WsV2LoginResult {
  bool ok = 1;
  bytes user = 2;                // Resolved master account (20 bytes)
  bytes signer = 3;              // Signer address (API wallet or master account)
  string error = 4;
}
```

规则：

- `timestamp_ms` 与服务端时间相差不超过 ±30 秒；nonce 不能重复使用。
- `host:` 必须等于服务端对外域名，预发是 `pre-api.riverbit.io`。
- 登录签名的 `v` 接受 27/28,也接受 0/1(这是 bin-ws 自己的规则，和 `/exchange` 不同)。
- 服务端从签名还原签名地址并查 `userRole`:`agent` 对应到它的主账户,`user` 对应到自己；其他角色、签名错误或超时，返回 `ok=false` 后断开连接。
- 服务端每 5 分钟复查一次授权，API wallet 被撤销后连接会被断开。

登录测试向量见附录 B.1。

### 4.3 登录后的访问范围

- 账户类查询和订阅的 `user` 只能是主账户本身，或主账户领导的金库(`leadingVaults`)。
- 行情类订阅不受限制。
- 交易仍需对每个 action 签名(第 3 节),由节点验签；bin-ws 把 `action_json` 原样转发给节点。

### 4.4 info 查询(`info_json`)

`info_json` 的内容与 REST `/info` 的 body 相同。回包外层 `id` 等于请求 `id`,`json` 内容为：

```json
{"channel":"post","data":{"id":7,"response":{"type":"info","payload":{"type":"openOrders","data":[]}}}}
```

`payload.type` 等于查询的 `type`,`payload.data` 与 REST `/info` 的返回体完全相同。

`info_json` 由只读(非验证)节点应答，数据通常比 REST `/info` 晚不超过 2 个区块(不到 1 秒)。下单或撤单后立刻用 `info_json` 查，可能看到 `unknownOid` 或旧状态，按第 9.2 节稍后重查即可。

`info_json` 只开放以下类型，并且只接受该类型定义的字段(REST `/info` 没有这个限制):

- 公开类:`meta`、`spotMeta`、`exchangeStatus`、`chainIdentity`、`allMids`、`l2Book`、`candleSnapshot`、`fundingHistory`
- 账户类(`user` 须在第 4.3 节的范围内):`clearinghouseState`、`spotClearinghouseState`、`openOrders`、`frontendOpenOrders`、`orderStatus`、`userFills`、`userFillsByTime`、`historicalOrders`、`userFees`、`userFunding`、`userNonFundingLedgerUpdates`

`metaAndAssetCtxs`、`userRole`、`leadingVaults` 只能走 REST。

## 5. 订阅与强类型帧

### 5.1 订阅格式

`subscribe_json` / `unsubscribe_json` 的内容是**订阅对象本身**的 UTF-8 JSON,格式与 Hyperliquid WebSocket 的 `subscription` 对象相同，不要再包一层 `{"method":"subscribe",...}`。订阅成功后收到 `json` 帧 `{"channel":"subscriptionResponse",...}`,外层 `id` 为 0。

做市常用订阅：

| 数据 | `subscribe_json` | 推送帧 | 说明 |
|---|---|---|---|
| 盘口增量(推荐) | `{"type":"l2","c":"BTC"}` | `l2_delta` | 先推快照再推增量，见第 5.3 节 |
| 完整盘口 | `{"type":"l2Book","coin":"BTC"}` | `l2_book` | 每次推完整盘口；可选 `nSigFigs`(2–5)和 `mantissa` |
| 最优买卖价 | `{"type":"bbo","coin":"BTC"}` | `bbo` | |
| 市场成交 | `{"type":"trades","coin":"BTC"}` | `trades` | |
| K 线 | `{"type":"candle","coin":"BTC","interval":"1m"}` | `candle` | 周期可选 `1m 3m 5m 15m 30m 1h 2h 4h 8h 12h 1d 3d 1w` |
| 当前挂单 | `{"type":"openOrders","user":"<master-or-vault-address>"}` | `json` | 先推挂单快照，再推变化 |
| 订单更新 | `{"type":"orderUpdates","user":"<master-or-vault-address>"}` | `json` | 只推变化，见第 9.3 节 |
| 账户成交 | `{"type":"userFills","user":"<master-or-vault-address>"}` | `json` | |
| 账户持仓 | `{"type":"clearinghouseState","user":"<master-or-vault-address>"}` | `json` | |

`l2` 是 RiverBit 的扩展订阅(不属于 Hyperliquid 规范),市场字段是 `c`,不是 `coin`;可选 `s`(数字，含义同 `nSigFigs`),例如 `{"type":"l2","c":"BTC","s":5}`。其他订阅都用 `coin`。

支持的全部 25 种订阅类型(Hyperliquid 规范的 24 种加 `l2`):

`allMids`、`notification`、`webData3`、`twapStates`、`clearinghouseState`、`openOrders`、`candle`、`l2Book`、`trades`、`orderUpdates`、`userEvents`(推送的 channel 名为 `user`)、`userFills`、`userFundings`、`userNonFundingLedgerUpdates`、`activeAssetCtx`、`activeAssetData`、`userTwapSliceFills`、`userTwapHistory`、`bbo`、`spotState`、`allDexsClearinghouseState`、`allDexsAssetCtxs`、`outcomeMetaUpdates`、`fastAssetCtxs`、`l2`

`l2`、`l2Book`、`bbo`、`trades`、`candle` 推送强类型帧(第 5.2 节),其余都通过 `json` 字段推送，内容与 Hyperliquid 的 WebSocket JSON 帧相同。

### 5.2 强类型行情帧

价格和数量是定点整数，换算公式 **`值 = 整数 / 10^decimals`**,`px_decimals`、`sz_decimals` 由每一帧给出(完整定义见附录 A)。

- `L2BookFrame`(`l2_book`):`bid_px[i]`、`bid_sz[i]`、`bid_n[i]` 按下标对应同一档，卖方 `ask_*` 同理。
- `BboFrame`(`bbo`):最优一档；某一侧为空时，那一侧的字段不出现。
- `CandleFrame`(`candle`):`open`/`close`/`high`/`low` 按 `px_decimals` 换算,`volume` 按 `sz_decimals` 换算,`trade_count` 是成交笔数,`open_time`/`close_time` 是毫秒时间戳。
- `TradesFrame`(`trades`):一帧里是同一市场的多笔成交;`buy` 表示主动买,`buyer`/`seller` 是 20 字节地址。

精度、计数等无法用强类型表示时，这些订阅会改发 `json` 帧，内容与 Hyperliquid 的 JSON 帧相同。客户端两种帧都要能处理。

### 5.3 `l2` 增量盘口

`L2DeltaFrame` 里 `snapshot` 与 `update` 二选一：

1. 收到 `snapshot`:整本替换，以它的 `px_decimals`/`sz_decimals` 为准。服务端随时可能重发 snapshot。
2. 收到 `update`:先按**上一本**的下标，把 `bid_removed`、`ask_removed` 作为一个集合一次性删除；
3. 再按价格覆盖已有档位或插入新档位(`bid_px`/`bid_sz`、`ask_px`/`ask_sz`);
4. 最后重新排序：买方降序，卖方升序。

- `update` 里的 `px_decimals`/`sz_decimals` 总是等于最近一次 snapshot 的值。不相等说明本地状态有误，应退订后重新订阅。
- `update` 里的 `spread` 是卖一减买一，按 `px_decimals` 换算；只有订阅时带了 `s` 才会出现。

## 6. 下单

### 6.1 action 格式

单笔和批量下单都用 `type:"order"`,多笔放在同一个 `orders` 数组里：

```json
{
  "type": "order",
  "orders": [
    {"a": 1, "b": true, "p": "80000", "s": "0.001", "r": false, "t": {"limit": {"tif": "Alo"}}, "c": "0x00000000000000000000000000000001"},
    {"a": 1, "b": false, "p": "82000", "s": "0.001", "r": false, "t": {"limit": {"tif": "Alo"}}, "c": "0x00000000000000000000000000000002"}
  ],
  "grouping": "na"
}
```

| 字段 | 含义 |
|---|---|
| `a` | asset index,取自 `metaAndAssetCtxs`(第 2 节)。示例中的 `1` 是本次 PRE 查询所得 BTC 下标，接入时必须重新查询 |
| `b` | `true` 买,`false` 卖 |
| `p` | 限价，十进制字符串，精度见第 6.2 节 |
| `s` | 数量，十进制字符串，精度见第 6.2 节 |
| `r` | reduceOnly;普通做市报价填 `false` |
| `t.limit.tif` | `Alo`:只做 maker,会立即成交的单被拒(状态 `badAloPxRejected`);`Gtc`:可以吃单 |
| `c` | 可选，客户端订单号 cloid:**小写** `0x` 加 32 位 hex(16 字节)。带了 cloid 后，可以按 cloid 查单(第 9.2 节)和撤单(第 7.2 节),改单后 cloid 跟到新订单上 |
| `grouping` | 填 `"na"` |

字段只能是上表这些，键序与签名时一致(第 3.2 节)。

### 6.2 精度与最小名义金额(永续)

| 规则 | 违反时的逐笔错误 |
|---|---|
| 数量小数位 ≤ `szDecimals`(取自 `metaAndAssetCtxs` 的 `universe`,如 BTC=5、ETH=4、SOL=2) | `local execution rejection: PrecisionExceeded` |
| 价格小数位 ≤ `6 − szDecimals`,且最多 5 位有效数字；整数价格不受有效数字限制。按数值判定，末尾的 0 不计入(如 BTC 写 `"50.0000"` 与 `"50"` 都合法) | `local execution rejection: PrecisionExceeded` |
| 价格是 tick 的整数倍 | `Price must be divisible by tick size. asset=N`(orderStatus 为 `tickRejected`) |
| 十进制格式合法：不能写 `".5"`、`"-1"`、`"1e5"`、`"+1"`,不能带空格 | `local execution rejection: BadDecimal` |
| 数量不为 0 | `Order has zero size.` |
| 价格不能偏离预言机价格过远 | `Price too far from oracle asset=N` |
| 名义金额 ≥ 10 美元 | `Order must have minimum value of $10. asset=N`(orderStatus 为 `minTradeNtlRejected`) |

- **出现 `PrecisionExceeded` 时，整个 action 的 `statuses` 只有这一个元素，所有订单都不分配 oid**。批量下单时任何一笔精度超限，整批都不会挂出。
- 名义金额 = 计价价格 × 数量。计价价格：买单取 min(限价, 对手最优卖价),卖单取 max(限价, 对手最优买价)。名义金额 ≤ 9.999999 美元被拒，恰好 10 美元可以通过。改单时同样检查，错误带前缀，见第 8 节。

### 6.3 Guild 金库只接受 Alo

**代 `class` 为 `guild` 的金库下单(请求带 `vaultAddress`)时，每笔限价单的 `tif` 都必须是 `"Alo"`。** 只要有一笔是 `Gtc`,**整个 action 被拒，所有订单都不会挂出**,回包没有 `statuses`:

```json
{"status": "err", "response": "action rejected by execution (code 1160)"}
```

主账户自己下单(不带 `vaultAddress`)不受此限制。代 Guild 金库改单时，新订单同样必须是 `Alo`。

### 6.4 提交

两种方式二选一，请求体字节完全相同：

- **REST**:把签名请求体(第 3.1 节)作为 body 发到 `POST https://pre-api.riverbit.io/exchange`。
- **bin-ws**:把同样的 UTF-8 JSON 字节放进 `WsV2ClientFrame.action_json`,配一个非零 `id`。

```sh
curl -sS https://pre-api.riverbit.io/exchange \
  -H 'Content-Type: application/json' \
  --data-binary @signed-action.json
```

代金库的签名请求体：

```json
{
  "action": {"type": "order", "orders": [{"a": 1, "b": true, "p": "80000", "s": "0.001", "r": false, "t": {"limit": {"tif": "Alo"}}}], "grouping": "na"},
  "nonce": 1792168694973,
  "signature": {"r": "0x<64 hex digits>", "s": "0x<64 hex digits>", "v": 28},
  "vaultAddress": "0x<your-vault-address>"
}
```

### 6.5 回包

REST `/exchange` 的回包示例(主账户下单，第一笔是 `Alo` 买单、已挂出；第二笔是吃单的 `Gtc` 卖单、已成交):

```json
{
  "status": "ok",
  "response": {
    "type": "order",
    "data": {
      "statuses": [
        {"resting": {"oid": 123456789, "cloid": "0x00000000000000000000000000000001"}},
        {"filled": {"totalSz": "0.001", "avgPx": "80000", "oid": 123456790, "cloid": "0x00000000000000000000000000000002"}}
      ]
    }
  }
}
```

`statuses[]` 与 `orders[]` 按顺序一一对应(例外见第 6.2 节的 `PrecisionExceeded`),每个元素是以下形态之一：

| 形态 | 含义 |
|---|---|
| `{"resting":{"oid":N,"cloid":"0x…"}}` | 已挂出，且没有任何成交。没带 cloid 时没有 `cloid` 字段 |
| `{"filled":{"totalSz":"…","avgPx":"…","oid":N,"cloid":"0x…"}}` | 有成交(十进制字符串)。只要有成交就是 `filled`,剩余部分可能仍挂在簿上，以 `orderStatus` 为准。`Alo` 单不会出现此形态 |
| `"waitingForTrigger"` / `"waitingForFill"` | 字符串形态，订单尚未挂到簿上；用 `orderStatus` 跟踪 |
| `{"error":"… asset=N"}` | 该笔被拒，错误文案见第 6.2 节 |

批量下单要逐项检查 `statuses[]`,不能只看顶层 `status`。

**整个 action 被拒**(nonce、签名者、Guild 金库用 Gtc 等)时没有 `statuses`:

```json
{"status": "err", "response": "action rejected by execution (code 91)"}
```

错误码见第 10.2 节。

**回包渲染不完整**:可能返回 `{"error":"local receipt rendering incomplete: …; query the order status before retrying","localCommitted":{…}}`。此时请求可能已经执行，先查 `orderStatus` 再决定下一步，不要直接换 nonce 重发。

bin-ws 的回包外层 `id` 等于请求 `id`,`json` 内容是把 REST 回包放进 `payload`:

```json
{
  "channel": "post",
  "data": {
    "id": 42,
    "response": {
      "type": "action",
      "payload": {
        "status": "ok",
        "response": {"type": "order", "data": {"statuses": [{"resting": {"oid": 123456789}}]}}
      }
    }
  }
}
```

bin-ws 服务端或节点层面出错时，`response.type` 是 `error`,例如在途请求超限(第 10.4 节):

```json
{"channel":"post","data":{"id":42,"response":{"type":"error","payload":"429 Too Many Requests"}}}
```

## 7. 撤单

撤单 action 按第 3 节签名、按第 6.4 节提交；撤金库的单同样要带 `vaultAddress`。

### 7.1 按 oid 撤单

```json
{
  "type": "cancel",
  "cancels": [
    {"a": 1, "o": 123456789},
    {"a": 1, "o": 123456790}
  ]
}
```

### 7.2 按 cloid 撤单

```json
{
  "type": "cancelByCloid",
  "cancels": [
    {"asset": 1, "cloid": "0x00000000000000000000000000000001"},
    {"asset": 1, "cloid": "0x00000000000000000000000000000002"}
  ]
}
```

注意字段名：`cancel` 用 `a`/`o`,`cancelByCloid` 用 `asset`/`cloid`。`cloid` 是字符串，按原文参与签名(第 3.2 节)。

### 7.3 撤单回包

`statuses[]` 与 `cancels[]` 一一对应：成功是字符串 `"success"`;已撤、已成交或不存在的单返回 error 对象：

```json
{
  "status": "ok",
  "response": {
    "type": "cancel",
    "data": {
      "statuses": [
        "success",
        {"error": "Order was never placed, already canceled, or filled. asset=0"}
      ]
    }
  }
}
```

## 8. 改单

改单 action 按第 3 节签名、按第 6.4 节提交。新订单对象要完整填写，并满足第 6.2、6.3 节的规则。改单成功后，原单状态变为 `canceled`,新单使用**新 oid**;原单带 cloid 时，cloid 跟到新单上。之后撤单、改单都要用新 oid(或继续用 cloid)。

**推荐用 `batchModify`**,只改一笔也用它，因为只有它的回包带新 oid。

### 8.1 `batchModify`(推荐)

```json
{
  "type": "batchModify",
  "modifies": [
    {"oid": 123456789, "order": {"a": 1, "b": true, "p": "80100", "s": "0.001", "r": false, "t": {"limit": {"tif": "Alo"}}, "c": "0x00000000000000000000000000000001"}},
    {"oid": 123456790, "order": {"a": 1, "b": false, "p": "81900", "s": "0.001", "r": false, "t": {"limit": {"tif": "Alo"}}, "c": "0x00000000000000000000000000000002"}}
  ]
}
```

回包 `statuses[]` 与 `modifies[]` 一一对应,`resting.oid` 就是新 oid:

```json
{
  "status": "ok",
  "response": {
    "type": "order",
    "data": {
      "statuses": [
        {"resting": {"oid": 123456801, "cloid": "0x00000000000000000000000000000001"}},
        {"resting": {"oid": 123456802, "cloid": "0x00000000000000000000000000000002"}}
      ]
    }
  }
}
```

某一项的新订单被拒时，该项是 `{"error":"…"}`,文案带前缀 `Error placing new order during modify: `,例如 `Error placing new order during modify: Order must have minimum value of $10. asset=0`。

### 8.2 `modify`(单笔)

```json
{
  "type": "modify",
  "oid": 123456789,
  "order": {"a": 1, "b": true, "p": "80100", "s": "0.001", "r": false, "t": {"limit": {"tif": "Alo"}}, "c": "0x00000000000000000000000000000001"}
}
```

成功时只返回下面的回包，**没有新 oid**:

```json
{"status": "ok", "response": {"type": "default"}}
```

要拿新 oid,只能按 cloid 查 `orderStatus`(`order.order.oid` 就是新 oid),或查 `frontendOpenOrders`。

## 9. 查询与跟踪

### 9.1 当前挂单：`openOrders` 与 `frontendOpenOrders`

```sh
curl -sS https://pre-api.riverbit.io/info \
  -H 'Content-Type: application/json' \
  -d '{"type":"openOrders","user":"<master-or-vault-address>"}'
```

`openOrders` 每项只有 `coin`、`side`(`B` 买 / `A` 卖)、`limitPx`、`sz`(剩余数量)、`oid`、`timestamp`:

```json
[{"coin":"BTC","limitPx":"80000","oid":123456789,"side":"B","sz":"0.001","timestamp":1792168694973}]
```

需要 `cloid`、`origSz`(原始数量)、`tif` 时用 `frontendOpenOrders`(同样传 `user`):

```json
[{"coin":"BTC","side":"B","limitPx":"80000","sz":"0.001","oid":123456789,"timestamp":1792168694973,"triggerCondition":"N/A","isTrigger":false,"triggerPx":"0.0","children":[],"isPositionTpsl":false,"reduceOnly":false,"orderType":"Limit","origSz":"0.001","tif":"Alo","cloid":"0x00000000000000000000000000000001"}]
```

`[]` 表示当前没有挂单。

**价格按数值比较，不要按字符串比较**:同一价格可能表示成 `"80000.0"` 或 `"80000"`，和自己下单时发的字符串也可能不同。

### 9.2 单笔订单状态：`orderStatus`

```sh
curl -sS https://pre-api.riverbit.io/info \
  -H 'Content-Type: application/json' \
  -d '{"type":"orderStatus","user":"<master-or-vault-address>","oid":123456789}'
```

`oid` 填订单号(JSON 数字)或 cloid(**小写** `0x` + 32 位 hex 的字符串)。cloid 写成大写、或把订单号写成字符串，都返回 400。按 cloid 查询时返回该 cloid **当前**对应的订单，改单后就是新订单。

回包有三种形态：

- 查到订单：`{"status":"order","height":H,"order":{"order":{…},"status":"<状态>","statusTimestamp":T,"rejectionReason"?},"leg"?:…}`
- 查不到：`{"status":"unknownOid","height":H}`。刚提交的订单可能还没处理到，**`unknownOid` 不能证明订单没有提交成功**,过一会儿再查。
- `{"status":"legResult","leg":…,"height":H}`:只有最近一次操作结果、没有订单对象。

示例：

```json
{
  "status": "order",
  "height": 1143585,
  "leg": {"resting": {"oid": 123456789, "cloid": "0x00000000000000000000000000000001"}},
  "order": {
    "order": {
      "coin": "BTC", "side": "B", "limitPx": "80000", "sz": "0.001", "origSz": "0.001",
      "oid": 123456789, "cloid": "0x00000000000000000000000000000001",
      "orderType": "Limit", "tif": "Alo", "reduceOnly": false, "timestamp": 1792168694973,
      "isTrigger": false, "triggerCondition": "N/A", "triggerPx": "0.0", "isPositionTpsl": false, "children": []
    },
    "status": "open",
    "statusTimestamp": 1792168694973
  }
}
```

`order.order.sz` 是剩余数量,`origSz` 是原始数量。

**判断订单当前状态以 `order.status` 为准。** 当前会出现的取值：

| 类别 | 取值 |
|---|---|
| 挂单中 | `open` |
| 结束 | `filled`、`canceled`(含被改单替换的原单)、`rejected`(另带 `rejectionReason`) |
| 已触发，需继续核对后续订单 | `triggered`；不能据此认定撤单成功或敞口归零 |
| 下单被拒 | `tickRejected`、`minTradeNtlRejected`、`reduceOnlyRejected`、`badAloPxRejected`(Alo 单会吃单)、`iocCancelRejected`、`perpMarginRejected`、`oracleRejected`、`insufficientSpotBalanceRejected` |
| 被系统撤销 | `selfTradeCanceled`、`reduceOnlyCanceled`、`siblingFilledCanceled`、`scheduledCancel`、`vaultWithdrawalCanceled` |

**未知状态不能直接推断为终态。** 本 demo 记录查询异常并保留 pending，待确认该状态语义后再处理；撤单核对也不会把 `triggered` 当作撤单成功。

**`leg` 是该 oid 最近一次操作的结果，不是当前状态。** 例如订单已被撤，`leg` 仍可能是下单时的 `{"resting":…}`。`leg` 可能是：

- `{"resting":…}`;
- `{"filled":{"totalSz":<整数>,"avgPx":<整数>,…}}`:这里是原始整数，不是十进制字符串，不要和第 6.5 节的 `filled` 混用；
- `{"error":"…"}`:注意 `minTradeNtlRejected` 时 `leg.error` 被截断为 `"Order must have minimum value of $"`;
- `"waitingForTrigger"`、`"success"`。

只有 `legResult` 而没有当前订单对象时，`filled` 也不能证明目标已无剩余挂单。demo 的撤单恢复要求逐笔撤单回执和可解释的当前订单状态；目标订单早已成交，不等于本次撤单请求已有终局结果。恢复中保存的逐笔证据是历史观测，不是同一时刻的完整订单/账户快照。

### 9.3 实时跟踪：`orderUpdates` 与 `userFills`

订阅格式见第 5.1 节。`orderUpdates` 只推送订阅之后的订单变化，**不含初始快照**;初始状态要另外用 `openOrders` 获取(第 10.5 节)。推送帧外层 `id` 为 0,`json` 内容示例：

```json
{
  "channel": "orderUpdates",
  "data": [
    {
      "order": {"coin": "BTC", "side": "B", "limitPx": "80000", "sz": "0.001", "oid": 123456789, "timestamp": 1792168694973, "origSz": "0.001"},
      "status": "open",
      "statusTimestamp": 1792168694973
    }
  ]
}
```

按 `oid` 关联本地订单，用 `status` 更新状态(取值同第 9.2 节),记录 `statusTimestamp`。改单后旧 oid 推送 `canceled`,新 oid 以 `open` 出现。成交数量和价格以 `userFills` 为准。

### 9.4 写请求后的核对

| 操作 | 核对 |
|---|---|
| 下单 | 逐项检查 `statuses[]`;保存 `oid`/`cloid`;核对挂单、成交或明确拒绝，不要求每笔都先出现 `open` |
| 撤单 | 逐项确认 `"success"`;确认 `order.status` 已是终态、oid 不在挂单列表里 |
| 改单 | 从逐笔成功回执取新 oid；核对旧 oid 终态和新 oid 当前状态；失败腿重新查旧单，不假定它已撤销 |
| 超时 / 断连 | 按第 3.6 节保留 pending 并查询恢复；未知不是拒绝，不能换 nonce 重发或只凭挂单缺失认定未执行 |

## 10. 错误、限频与重连

### 10.1 `/info` 错误

| 情况 | 返回 |
|---|---|
| 缺字段、未知 `type`、非法地址、body 不是 JSON | 400 `{"error":"bad request"}` |
| `orderStatus` 的 cloid 不是小写、订单号写成字符串 | 400 |
| `l2Book` 带 `nSigFigs` 或 `mantissa` | 501 `l2Book price bucketing is unimplemented` |
| 未知 coin | 取决于查询类型：`l2Book` 返回 404 `{"coin":…,"error":"unknown coin"}`;`fundingHistory` 返回 200 `[]`;`candleSnapshot` 返回 400 |

需要按有效数字聚合的盘口，用 bin-ws 的 `l2Book` 订阅(带 `nSigFigs`)或 `l2` 订阅(带 `s`)。`info_json` 查 `l2Book` 转发的是同一个节点接口，同样不要带这两个字段。

### 10.2 `/exchange` 整个 action 被拒

这类响应的格式为 `{"status":"err","response":"action rejected by execution (code N)"}`。只有明确的拒绝且没有可能产生交易效果的结果时，才能结束本次请求的 pending。必须同时检查 `committedResult` 和逐笔结果；顶层 `status:"err"` 本身不能证明所有腿都没执行。重复 nonce 拒绝的是这次提交，不能据此推断原请求的结果。

| code | 含义 | 处理 |
|---|---|---|
| 91 | 签名者或用户不存在。签名镜像算错时，节点还原出陌生地址，也是这个码 | 用附录 B 核对签名实现；确认 API wallet 已授权 |
| 1160 | Guild 金库只接受 Alo | 见第 6.3 节 |
| 203 | nonce 太小 | 见第 3.6 节 |
| 204 | nonce 太大 | 检查本机时钟 |
| 205 | nonce 重复 | 该 nonce 已被使用，见第 3.6 节 |
| 97 | 待处理的 nonce 过多 | 降低并发，等待在途请求完成 |

另外，同一个签名请求还在处理中又提交一次时，返回 `action already pending; it was not submitted again`。

### 10.3 REST 限频

- 按客户端 IP 计，每分钟的总权重有上限(额度以运营通知为准)。超限返回 429,`Content-Type: text/plain`,body 为 `gateway: admission budget unavailable`,带 `Retry-After` 头。查询按该头退避；交易请求先保留 pending 并核对原结果，不能套用查询的自动重试逻辑。
- 网关并发过高时返回 503 `gateway: too many in-flight requests`。
- 请求体超过 2 MiB 返回 413。

各请求的权重：

| 接口 | 请求 | 权重 |
|---|---|---|
| `/info` | `l2Book`、`allMids`、`clearinghouseState`、`orderStatus`、`spotClearinghouseState`、`exchangeStatus` | 2 |
| `/info` | `userRole` | 60 |
| `/info` | 其他类型 | 20 |
| `/exchange` | `order`、`cancel`、`cancelByCloid`、`batchModify` | 1 + ⌊条数 / 40⌋ |
| `/exchange` | `modify` | 1 |

`userRole` 权重很高，只在启动时查一次。

### 10.4 bin-ws 配额与心跳

按客户端 IP 计算：

| 项 | 上限 | 超限时 |
|---|---|---|
| 同时连接 | 10 条 | |
| 新建连接 | 每分钟 30 次 | |
| 订阅 | 1000 个 | |
| 订阅中出现的不同 `user` | 10 个 | |
| 客户端发送的消息 | 每分钟 2000 条 | 服务端回 `json` 帧 `{"channel":"error","data":"websocket message rate limit exceeded"}`,**然后断开连接** |
| 在途的 action/info 请求(已发出、未收到回包) | 100 个 | 该请求的回包为 `429 Too Many Requests`(第 6.5 节) |
| 单条消息大小 | 1 MiB | 连接断开 |

心跳：服务端连续 60 秒没有向客户端发出任何消息时会断开连接。行情冷清时，客户端每 30 秒左右发一次 `ping`(`WsV2ClientFrame.ping`,空消息),服务端回 `json` 帧 `{"channel":"pong"}`,外层 `id` 为 0。

### 10.5 重连与对账

服务端发布时会重启，连接也可能因网络或上面的配额断开。客户端要自动重连，步骤：

1. 重连 bin-ws,第一帧登录(第 4.2 节)。
2. 重新订阅 `orderUpdates`、`userFills` 和行情，等待各自的 `subscriptionResponse`。
3. 查 `openOrders` 或 `frontendOpenOrders`(REST 或 `info_json`),重建本地挂单表。
4. 之后按 `orderUpdates` 推送更新本地挂单表。
5. 断线期间结果不明的请求，按第 3.6 节保留 pending 并查询恢复；不得重发未知请求，确认结果后再决定下一步。

## 11. 持续做市的执行与对账建议

本节提炼自做市执行器的订单生命周期、持久请求恢复、快照检查和风险预算逻辑。它是接入方实现持续做市的建议，不代表 demo 已实现自动报价、完整资金对账或所有内部执行能力。内部旧通道的接口、数值错误码和运维参数不适用于本公网协议。

### 11.1 请求状态与订单状态分开保存

| 本地请求阶段（不是 API 状态枚举） | 处理 |
|---|---|
| 已准备 | 分配唯一 nonce/cloid，先持久化 action、signer、主账户/金库、网络、端点和操作目标，再发送；落盘不等于已经发送 |
| 已发送、结果未知 | 超时、断连、无法解析回包或查询落后时保留 pending；暂停会与该请求冲突的新写入，仍计算可能产生的敞口 |
| 逐腿执行已确认 | 保存每腿成功/拒绝结果、oid/cloid 和查询高度；部分成功不回滚，不能整批重发 |
| 当前订单已核对 | 查询最新订单、成交、持仓与可用资金后重新计算目标；曾经确认成功不等于订单现在仍挂单 |
| 明确拒绝 | 修复参数/身份/资金等根因，再构造新请求与新 nonce；原请求的审计记录保留 |

一笔 action 可以创建多张订单，一张订单也可以先后有下单、改单和撤单请求。不要把“请求执行完毕”和“订单已结束”合并成一个布尔值。`filled` 的逐笔回执可能仍有挂单余量，当前状态以 `order.status` 和剩余 `sz` 为准；成交价格、数量以成交记录为准。

同一 signer 的 nonce 分配必须统一协调；同一交易账户/市场建议由一个串行执行 owner 决定撤补，避免行情事件、定时刷新和恢复线程重复操作。demo 的共享 `STATE_DIR` 与本机文件锁不能替代多主机协调。

### 11.2 下单、撤单、改单与补单

| 场景 | 执行与核对要点 |
|---|---|
| 正常报价 | 保留价格、方向、剩余数量仍满足目标的旧单，只处理差额；价格数量按精确十进制或整数定点运算 |
| 下单立即成交 | 不再等待不存在的 `open`；核对成交及可能剩余的挂单，再更新库存与资金 |
| 部分成交后补量 | 用已确认剩余量衡量深度，不能按订单张数或原始数量补单；先重新计算整侧风险额度 |
| 撤单与成交竞争 | 发出撤单后旧单仍按可能成交计风险；目标已成交不证明这次撤单请求已结束，还要保留/核对本次逐腿回执 |
| 改单 | 记录旧 oid → 新 oid 与 cloid 关系；选择同方向旧单替换，不按 oid 排序位置配对买卖；失败腿重新查询旧单当前状态 |
| 批量部分失败 | 成功腿与失败腿分别落账；只为重新评估后的缺口构造后续 action，不重发成功腿 |
| 结果未知 | 保留 journal 与已确认的逐腿证据；`unknownOid`、空挂单列表、等待超时和 nonce 已使用都不能证明未执行 |

风险撤单优先于补单，为撤单和恢复查询预留独立 API 预算。可按买卖侧设置成交后的固定批次冷却：首次确认剩余量下降/订单消失时启动，后续通知不再后推截止时间；到期后仍须重新读库存、资金和行情。订单消失只是深度缺口，不能直接记成成交。冷却、最大单龄、重报价阈值由接入方按策略配置，本 demo 不提供策略默认值。

### 11.3 快照、推送与风险敞口

推送用于及时唤醒对账，不单独作为资金账本。重连后重新登录/订阅，缓存恢复期间到达的更新，查询挂单、订单状态、成交和账户状态，再按身份与可用事件标识去重、核对。不要假定快照与实时事件天然无缝，也不要把查询返回的 `height` 当作原交易执行块高度。

保存已接受执行证据的最低查询高度，拒绝用更旧的投影覆盖本地新状态。普通 `openOrders` 没有快照高度，多次独立查询也不保证同一提交态；缺少足够证据时暂停新增敞口并继续对账，不能自行给响应补高度。内部一致性快照和执行证明不应被当作已对外提供的能力。

整侧风险预算同时考虑现有持仓、全部存活订单、待撤订单、结果未知的新增/改单以及准备提交的订单。普通卖单不必然减仓；不能对每一档重复使用同一份仓位或保证金额度。行情过期、盘口交叉、交易禁用、账户/价格身份不符或资金不可用时暂停新增报价；按已知订单和可用授权执行风险撤单，结果未知仍保留记录。

### 11.4 错误后的下一步

| 信号 | 分类与下一步 |
|---|---|
| `91` | 核对 API wallet 授权、所属主账户、签名字段顺序与动态 chainId；不能一律解释为缺资金或自动调用 faucet |
| `1160` | Guild 报价/替换单改用 `Alo`，修复整批中的违规腿后重新评估提交 |
| `203` / `204` | 核对毫秒时钟、nonce 窗口与分配器；只处理明确拒绝的当前请求，不覆盖其他未知请求 |
| `205` / `action already pending` | 查询原 nonce 对应 action 的订单结果，不换 nonce 复制该交易 |
| `97` | 降低在途并发，优先恢复已有请求；继续保留原请求状态 |
| `PrecisionExceeded` / `BadDecimal` / tick / 最小金额拒绝 | 修正精度、tick、step 或名义金额；`PrecisionExceeded` 的单元素回包不能当作普通逐腿数组解析 |
| `badAloPxRejected` / `oracleRejected` | 读取最新盘口、参考价及交易状态后重新定价；不是直接改成 taker 单 |
| `perpMarginRejected` / `reduceOnlyRejected` / 余额不足 | 重查持仓和可用资金并缩量/调整方向，停止按原数量循环提交 |
| 系统撤单状态 | 核对自成交、仓位、关联单或金库状态等原因，再决定是否补单；不要统一当成普通成交 |
| HTTP 429/503/5xx、超时、断连、回包损坏 | 查询可退避重试；交易保留 pending 并恢复查询，不能只凭 HTTP 状态证明没有执行 |
| `status:"err"` 带混合 `committedResult`、`localCommitted` 或要求查询的提示 | 可能已有部分效果；保存完整回包并逐腿核对，不能整批判失败 |
| 未知错误码或订单状态 | 保存诊断信息、暂停冲突写入并请求支持，不推断为成功、终态或可安全重发 |

详细字段、错误字符串和适用范围仍以第 6–10 节为准。日志记录请求标识、nonce、账户/市场、操作、oid/cloid、逐腿结果、观测高度和阶段耗时；不记录私钥或可重放的完整签名请求。

### 11.5 重启、退出与监控

重启先校验原 signer、账户、端点、网络与持久目录，恢复未知请求，再重建当前挂单和风险状态。切换 WS/REST、账户或部署实例不能绕过 pending。demo 的 `recover` 只查询，不重新执行 `ACTION_FILE`。

持续做市程序退出时应先停新增报价，保留未知请求，核对并撤销本实例管理范围内的已知挂单，确认结果后再报告清理完成。仅退出进程或关闭 WS 不会自动撤单；demo 不带持续策略的退出自动撤单，手动 trade 后的挂单需显式撤销。撤单失败或查询不可用要报告未完成，不能把进程退出成功当成账户已无敞口。

建议监控 pending 数量与年龄、行情/账户数据年龄、逐腿拒绝原因、限频、断线、剩余深度与库存、所需撤单未确认时长，以及提交到结果确认、结果确认到订单可见的分阶段延迟。只统计成功请求的延迟会掩盖停滞，须同时记录未知请求与暂停报价时间。

## 附录 A:bin-ws Protobuf 定义

`syntax = "proto3"; package riverbit.api.v1;`。以下是 bin-ws 用到的全部消息，可以直接用 protoc 生成客户端代码。

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

## 附录 B:签名测试向量

以下交易签名向量保留原始资产索引与价格，仅用于离线验签，不是可提交的 BTC 订单示例。

以下向量由两套独立实现(Python、Go)算出，结果一致。所有向量使用同一把测试私钥：

| 项 | 值 |
|---|---|
| 私钥 | `0x0101010101010101010101010101010101010101010101010101010101010101`(32 字节全 `0x01`) |
| 地址 | `0x1a642f0E3c3aF545E7AcBD38b07251B3990914F1` |

**这把私钥是公开的，只能用于离线自测，不要往这个地址转任何资产。**

### B.1 bin-ws 登录(EIP-191)

签名文本(四行，`\n` 分隔，末尾无换行):

```
riverbit-ws-v2-login
host:pre-api.riverbit.io
timestamp:1792168694973
nonce:000102030405060708090a0b0c0d0e0f
```

| 项 | 值 |
|---|---|
| `WsV2Login.timestamp_ms` | `1792168694973` |
| `WsV2Login.nonce`(16 字节) | `000102030405060708090a0b0c0d0e0f` |
| `WsV2Login.signature`(65 字节 r‖s‖v) | `4752b41eb2baa20a3e169891002152aa388130e129b3c8dbd4f957e03a1cddef7f1e12c690cbc3a8ec00f287739ea28e2a8d10ba6073353074735cc6fe56c23f1b` |

### B.2 交易签名(EIP-712)

公共输入：

- `chainId` = `12589`,`source` = `"b"`
- `nonce` = `1792168694973`
- action JSON(就是这个字节序列，键序不能变):

```json
{"type":"order","orders":[{"a":0,"b":true,"p":"100.5","s":"0.2","r":false,"t":{"limit":{"tif":"Alo"}}}],"grouping":"na"}
```

- `msgpack(action)`:

```
83a474797065a56f72646572a66f72646572739186a16100a162c3a170a53130302e35a173a3302e32a172c2a17481a56c696d697481a3746966a3416c6fa867726f7570696e67a26e61
```

**向量 1:不代金库**(connectionId 尾部 `0x00`)

| 项 | 值 |
|---|---|
| connectionId | `0x0260ebaee4220bfd192a49fad29e62a1f9890f5b70e89a095e8099686b4805a6` |
| EIP-712 digest | `0x724d79bc83f9bb39300af824a91d9e5a84028b1f15b90c77d1e3212f5290d272` |
| r | `0xb075d6ff3fe5bfa399a22ecfca3375af8223181b21a363bde243ce9186edd7ec` |
| s | `0x01f82100252650e8ddde4815134d4e0420afaca71b09ec03c278f0a4985cf552` |
| v | `27` |

**向量 2:代金库**(`vaultAddress` = `0x1111111111111111111111111111111111111111`,connectionId 尾部 `0x01` + 金库地址)

| 项 | 值 |
|---|---|
| connectionId | `0x749d2e6560886f2092bcee6423609c41dc20e47d17ad9a50d5483e4165bc3d39` |
| EIP-712 digest | `0xe61bf62709ae5db02f460316839fbd16baf772a6abc096b7d44e5773f125d272` |
| r | `0xc69888725b054488375d419e27dea48f41f2f8da5ec863adf9af1e1d31f1c071` |
| s | `0x1adb693fdc1e45c68f4c39f4f868f731bcaaa11eb159fb2a5d46ebe8843aff24` |
| v | `28` |

向量 1 的 `s` 有一个前导 0。有的库输出 `0x1f8210…`(去掉前导 0),这和上表的值等价，节点会左补 0(第 3.5 节)。
