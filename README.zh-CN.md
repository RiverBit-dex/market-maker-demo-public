# RiverBit 做市商接入 Demo

中文 | [English](README.md)

RiverBit 给做市商的通用做市参考 demo(Go)。它演示如何用 API wallet 接入预发环境:订阅行情和订单事件,下单、撤单、改单,并核对订单状态。主账户自己交易和代金库交易是同一套代码的两种模式,金库只是其中一种用法。

**本 demo 不包含自动报价策略**。它按你给的 action JSON 文件逐个提交,不会自己决定价格和数量。

适合验证公网协议、签名和订单流程；持续做市还需要行情新鲜度门禁、库存/资金约束、暂停撤单、订单与成交补查，以及真实负载验收。内部 offchain maker 使用的原生 `/ws/v2` 与本 demo 的登录式 `/bin-ws` 协议不同，不能只替换 URL。

协议、签名、字段和错误码以 [`docs/guild-market-maker-api.md`](docs/guild-market-maker-api.md) 为准;本文只写 demo 怎么用,不复述协议细节。

开发持续做市程序前，请阅读[执行与对账建议](docs/guild-market-maker-api.md#11-持续做市的执行与对账建议)：请求/订单状态分离、部分成交、撤单竞争、改单、风险预算、错误处理及退出清理。

## 功能

- 行情:`l2` 增量盘口(默认,本地回放后打印买一卖一)或 `l2Book` 全量盘口、`trades`、`candle`(1m)。
- 订单和成交推送:`orderUpdates`、`userFills`。
- 下单、批量下单(`order`)。
- 按 oid 撤单(`cancel`)、按 cloid 撤单(`cancelByCloid`)。
- 单笔改单(`modify`)、批量改单(`batchModify`)。
- 提交后自动核对:结合逐笔回执和 `orderStatus` 核对已观测订单结果,最后查询 `openOrders` 并检查撤单目标是否仍在挂单列表中。
- 两种传输方式:`ORDER_TRANSPORT=ws`(默认)或 `rest`。
- 两种交易模式:主账户自己交易(API wallet 代所属主账户),以及代金库交易(设置 `GUILD_VAULT_ADDRESS`)。
- nonce 与待确认请求持久化到 `STATE_DIR`:回包超时、连接断开等结果未知时,不重发,先对账。
- `recover` 命令只恢复待确认请求并查询挂单，不读取 `ACTION_FILE`、不提交交易；批量改单完整回执落盘后可跨重启核对部分失败。
- bin-ws 行情连接断线后自动重连(重新登录并重新订阅)。

## 地址

| 用途 | 地址 |
|---|---|
| REST 查询 | `https://pre-api.riverbit.io/info` |
| REST 交易 | `https://pre-api.riverbit.io/exchange` |
| WebSocket(bin-ws) | `wss://pre-api.riverbit.io/bin-ws` |

## 准备工作

1. Go 版本:见 `go.mod`,最低为 `go 1.26.9`（包含本轮检查所需的标准库安全补丁）。使用维护中分支的最新补丁版本。
2. API wallet 私钥文件:默认读取当前目录的 `key`,可用 `KEY_FILE` 指定。文件内容是一行私钥。该文件已被 `.gitignore` 排除,不要提交,不要把私钥写进命令行、日志或 action 文件;程序不会打印私钥。
3. API wallet 必须先由主账户授权。**每条 bin-ws 连接都要登录,所以只看行情也需要私钥文件。**
4. 代金库交易:签名者必须是该金库的 leader 或 leader 的 API wallet。金库地址用 `/info` 的 `leadingVaults` 查(传主账户地址,见接入手册第 1.3、1.4 节)。

## 快速开始

先克隆本仓库，再执行下面的命令：

```sh
git clone https://github.com/RiverBit-dex/market-maker-demo-public.git
cd market-maker-demo-public
```


以下命令都在仓库根目录执行,默认已准备好 `./key`。输出为 `slog` 文本日志。

### (a) 只看行情和订单推送

```sh
go run ./cmd/market-maker-demo
```

会看到:

- `starting market maker demo ...` 启动日志,随后 `HTTP /info is reachable`(不通只告警,继续跑)。
- `bin-ws login ok user=... signer=...`:服务端识别出的主账户和签名者。
- `subscribed to bin-ws streams ...`。
- 持续滚动的 `l2`(买一卖一)、`trade`、`candle`,以及有订单变化时的 `order update`。

换成全量盘口:`BOOK_STREAM=l2Book`。看金库的订单事件:加 `GUILD_VAULT_ADDRESS=0x<金库地址>`;不设置则订单事件跟随登录识别出的主账户。`Ctrl+C` 退出。

### (b) 主账户 REST 下单

先把 `examples/btc-batch-order.sample.json` 复制一份,按实时盘口改价格和数量(见下文"action 文件格式"),再执行:

```sh
cp examples/btc-batch-order.sample.json my-order.json
# Edit the price and size in my-order.json
ORDER_TRANSPORT=rest \
ACTION_FILE=my-order.json \
go run ./cmd/market-maker-demo trade
```

会看到:

- `Verified BTC asset: index=... size_decimals=... max_leverage=...`:已校验 action 里的资产索引与 `COIN` 一致。
- `trading account resolved via POST /info userRole ...`:`signer_role=agent` 时交易账户为所属主账户。
- `Submitting action 1/1 (*hlexchange.Place) for <账户> via REST (...)`。
- `Resolved order IDs: [...] (execution height=...; statuses=[...])`:本次分配到的 oid。
- `Actions accepted via REST ...; current open orders for <账户>: N`,以及每个挂单一行 `coin=... oid=... side=... price=... remaining=...`。

`rest` 模式不连 bin-ws,所以没有订单推送。

### (c) 主账户 WebSocket 下单

```sh
ORDER_TRANSPORT=ws \
ACTION_FILE=my-order.json \
go run ./cmd/market-maker-demo trade
```

`ORDER_TRANSPORT` 默认就是 `ws`,可省略。输出与 (b) 相同,区别是:

- 多出 `bin-ws login ok ...` 和订阅日志,提交和对账都走这条长连接。
- 会同时打印该账户的 `order update` 等推送。
- `via` 显示为 `bin-ws (action_json; ...)`。

### (d) 代金库下单

```sh
GUILD_VAULT_ADDRESS=0x<your-vault-address> \
ACTION_FILE=my-order.json \
go run ./cmd/market-maker-demo trade
```

会看到 `trading for vault vault=0x... ...`,其余同 (c)(加 `ORDER_TRANSPORT=rest` 即同 (b))。启动时会校验签名者对该金库的授权,不满足则直接报错退出。

**Guild 金库只接受 `Alo`**:action 里每一笔限价单的 `tif` 都必须是 `"Alo"`,有一笔是 `Gtc` 整个 action 就被拒(`code 1160`)。示例文件用的就是 `Alo`。主账户自己下单没有这个限制。

### (e) 下单 -> 改单 / 撤单 的完整流程

下两笔单，改其中一笔，再分别按 oid 和按 cloid 撤掉。

1. 准备下单文件 `flow-order.json`,两笔都带 `cloid`(`c` 为 `0x` + 32 位小写十六进制，自己生成且不重复;价格数量按盘口改):

   ```json
   {
     "type": "order",
     "orders": [
       {"a": 1, "b": true, "p": "80000", "s": "0.001", "r": false,
        "t": {"limit": {"tif": "Alo"}},
        "c": "0x11111111111111111111111111111111"},
       {"a": 1, "b": true, "p": "79900", "s": "0.001", "r": false,
        "t": {"limit": {"tif": "Alo"}},
        "c": "0x22222222222222222222222222222222"}
     ],
     "grouping": "na"
   }
   ```

2. 下单，从输出的 `Resolved order IDs: [<oid1> <oid2>]` 记下两个 oid(顺序与 `orders` 一致):

   ```sh
   ACTION_FILE=flow-order.json go run ./cmd/market-maker-demo trade
   ```

3. 改第一笔，文件 `flow-modify.json`。**`order` 里要带上原来的 `c`**,改单后的新订单才沿用这个 cloid;不带的话新订单换成 demo 生成的随机 cloid,第 5 步就按原 cloid 撤不到:

   ```json
   {"type":"batchModify","modifies":[
     {"oid": <oid1>, "order": {"a": 1, "b": true, "p": "79950", "s": "0.001", "r": false,
      "t": {"limit": {"tif": "Alo"}}, "c": "0x11111111111111111111111111111111"}}
   ]}
   ```

   ```sh
   ACTION_FILE=flow-modify.json go run ./cmd/market-maker-demo trade
   ```

   改单会撤掉旧订单、挂出一笔新订单，新订单的 oid 不同。输出的 `Resolved order IDs` 是新 oid。用单笔 `modify`(`{"type":"modify","oid":<oid1>,"order":{...}}`)也可以：节点回包不带新 oid,demo 会按 cloid 查 `orderStatus` 补出新 oid。

4. 按 oid 撤第二笔，文件 `flow-cancel.json`:

   ```json
   {"type":"cancel","cancels":[{"a":1,"o":<oid2>}]}
   ```

   ```sh
   ACTION_FILE=flow-cancel.json go run ./cmd/market-maker-demo trade
   ```

5. 按 cloid 撤第一笔(改单后的那笔),文件 `flow-cancel-cloid.json`:

   ```json
   {"type":"cancelByCloid","cancels":[{"asset":1,"cloid":"0x11111111111111111111111111111111"}]}
   ```

   ```sh
   ACTION_FILE=flow-cancel-cloid.json go run ./cmd/market-maker-demo trade
   ```

第 4、5 步结束时，程序会核对被撤的订单已不在挂单列表里，否则报错;最后一行 `current open orders ...: 0` 表示这两笔都已撤掉。示例里的 `<oid1>`、`<oid2>` 要换成第 2 步的实际值。

`ACTION_FILE` 可用逗号分隔多个文件,按顺序提交,每个都先对账再提交下一个,例如 `ACTION_FILE=flow-cancel.json,flow-cancel-cloid.json`。

### (f) 超时、断线或重启后的恢复

每个 action 的提交与结果等待合计最多 30 秒。超时不代表失败，也不会清除待确认记录。保留原 `KEY_FILE`、`STATE_DIR`、账户/金库和端点配置，用原传输方式运行：

```sh
# Keep ORDER_TRANSPORT=rest if originally used; retain the original vault address.
go run ./cmd/market-maker-demo recover
```

此命令不读取 `ACTION_FILE`，不会下单、改单或撤单。每轮恢复最多等待 30 秒；仍未知时保留 pending 并返回非零，可以稍后再次执行。没有 pending 时只查询当前挂单。`trade` 遇到已有 pending 会拒绝新提交，不会恢复后自动重放 action 文件。

大批次的逐笔查询按 5 秒一轮推进。已取得且通过高度检查的正向证据写入原 journal，后续查询变慢、历史结果被淘汰或进程重启时继续核对剩余项。保存的 `statuses` 表示曾观测到的结果，不代表订单此刻仍挂单；未知项不会由已知项推断出来。

批量改单可能部分成功、部分拒绝。demo 保留完整逐笔回执：拒绝腿不等待不存在的新 cloid，也不要求旧订单被撤掉；成功腿按回执中的新 oid 查状态。确认部分失败后仍以非零退出，成功腿不会回滚，不应整批重发。核对挂单、订单状态及成交后再构造下一笔 action。

撤单也保存逐笔回执，并独立核对目标的当前 `order.status`。仅有 `legResult.filled`、目标已成交、未知状态或 `action already pending`，都不能证明本次撤单已结束；回包丢失时保留 pending。尤其不能在未知 `cancelByCloid` 后复用 cloid，以免迟到的撤单影响新订单。部分撤单被拒时返回错误，不把剩余目标说成已撤销。

`unknownOid`、等待时间和空 `openOrders` 都不能证明原请求未执行（订单可能已成交）。若响应丢失、旧版本 journal 没有逐笔回执，且公开查询无法提供充分证据，demo 会保留 pending；需要取得原请求的执行证据后人工核对。**不要删除 `STATE_DIR`、切换目录或换 nonce 重发来绕过 pending。**

恢复范围：本 demo 提供订单证据恢复；末尾的 `openOrders` 没有快照高度，不能证明它与逐笔证据或账户余额属于同一时刻。内部 maker 额外校验完整签名交易的终局证明、跨过读水位的完整新鲜订单/账户快照，并提供旧 journal 查找提示及受控迁移。demo 未包含这些能力，不会仅凭交易 hash、区块高度、nonce 或目标订单已结束来释放缺证据的请求，也不会根据订单推送推算库存或资金。

BTC 示例使用 `a`/`asset` = `1`、数量 `0.001` 和整数价格作为格式参考；提交前必须重新查询 `metaAndAssetCtxs` 并按实时盘口调整价格。

## action 文件格式

`ACTION_FILE` 的每个文件是一个 JSON action,`type` 支持 `order`、`cancel`、`cancelByCloid`、`modify`、`batchModify`。字段含义见接入手册第 6 到 8 节。`examples/` 下的文件:

| 文件 | 用途 |
|---|---|
| `examples/btc-batch-order.sample.json` | 批量下单:一买一卖两笔 `Alo` 限价单 |
| `examples/batch-modify.sample.json` | 批量改单(`batchModify`,推荐):按 `oid` 改两笔 |
| `examples/cancel-by-cloid.sample.json` | 按 `cloid` 撤单 |

注意:

- 示例里的价格、数量、资产索引(`a`/`asset`)、`oid`、`cloid` 都只是占位值,**提交前必须按实时盘口和真实订单修改**。资产索引必须与 `COIN` 对应市场一致,否则程序在提交前就报错。
- 每笔订单名义金额要 ≥ 10 美元。
- 订单里填了 `c`(cloid)就按原值提交，之后可以用它撤单或查 `orderStatus`;不填时 demo 自动生成一个随机 cloid,用于提交后的对账。
- 改单推荐用 `batchModify`,回包直接带新 oid;单笔 `modify` 的回包不带新 oid。改单时要沿用 cloid,就在 `order` 里带上原 `c`。
- 按 oid 撤单、单笔 `modify` 没有示例文件,格式见上面 (e) 第 3、4 步。

## 环境变量

以 `internal/config/config.go` 为准。

| 变量 | 默认值 | 作用 |
|---|---|---|
| `BIN_WS_URL` | `wss://pre-api.riverbit.io/bin-ws` | bin-ws 地址(行情、订单事件、`ws` 模式交易共用) |
| `BIN_WS_LOGIN_HOST` | `pre-api.riverbit.io` | 登录消息里签名的 `host:`,必须等于服务端配置的对外域名 |
| `API_GATEWAY_URL` | `https://pre-api.riverbit.io` | HTTP 基址,请求 `POST <基址>/info`;`rest` 模式还请求 `POST <基址>/exchange` |
| `ORDER_TRANSPORT` | `ws` | `trade` / `recover` 的传输方式:`ws` 或 `rest`,其他值启动即报错 |
| `COIN` | `BTC` | 市场名称,用于查资产索引并校验 action |
| `BOOK_STREAM` | `l2` | 盘口订阅:`l2`(增量)或 `l2Book`(全量),其他值启动即报错 |
| `GUILD_VAULT_ADDRESS` | 空 | 金库地址。设置后代金库交易,行情模式下订单事件订阅金库 |
| `KEY_FILE` | `key` | API wallet 私钥文件(登录和交易签名都用) |
| `STATE_DIR` | `.guild-mm-state` | nonce 与待确认请求的持久化目录;同一个 signer 的多个进程必须共用 |
| `ACTION_FILE` | 空 | `trade` 命令要提交的 action 文件,可逗号分隔多个;`trade` 必填，`recover` 不读取 |
| `SIGN_CHAIN_ID` | 空 | 可选。签名 chainId 总是取 `/info` `chainIdentity` 的 `nextAgentChainId`(当前为 `12589`);设置后与它核对,不一致则启动报错 |

已废弃的 `WS_URL`、`TRADE_WS_URL` 不再读取;设置了会打印警告,请改用 `BIN_WS_URL`。

## 各操作走哪条通道

`ORDER_TRANSPORT` 影响 `trade` 和 `recover` 命令;不带子命令的行情模式始终走 bin-ws。`recover` 只执行账户解析和订单查询，跳过提交前的签名域、市场参数与金库执行权检查。

| 操作 | `ws` 模式(默认) | `rest` 模式 |
|---|---|---|
| 启动查询:`chainIdentity`、`metaAndAssetCtxs`,金库校验 | HTTP `POST /info` | HTTP `POST /info` |
| 确定交易账户(不配金库时) | bin-ws 登录结果里的 user | HTTP `POST /info` 查 `userRole` |
| 订阅 `orderUpdates`、`userFills` | bin-ws 订阅 | 不订阅(不连 bin-ws) |
| 下单、撤单、改单 | bin-ws `action_json` | HTTP `POST /exchange` |
| 对账 `orderStatus`、`openOrders` | bin-ws `info_json`(同一条连接) | HTTP `POST /info` |
| 行情(不带 `trade`) | bin-ws | bin-ws |

两种方式提交的是同一份已签名请求体。

## 常见问题排查

- **`code 91`**:签名者或用户不存在,或签名算错。确认 API wallet 已由主账户授权;自己改过签名实现时,用接入手册附录 B 的测试向量核对。
- **`code 1160`**:代 Guild 金库下单用了 `Gtc`。该金库每笔限价单都必须是 `Alo`,整个 action 会被拒,没有任何订单挂出。
- **不足 10 美元**:报 `Order must have minimum value of $10`。提高价格或数量,使每笔名义金额 ≥ 10 美元。
- **`PrecisionExceeded`**:数量或价格小数位超限。数量位数不得超过 `metaAndAssetCtxs` 里该市场的 `szDecimals`(程序启动时会打印 `size_decimals`),价格规则见接入手册第 6.2 节。批量下单时只要一笔超限,整批都不会挂出。
- **超时或连接断开,结果未知**:保留 `STATE_DIR`，运行上文的 `recover`；**不会自动重发，也不会按等待时长清除 pending**。查询仍未知时不能认定没生效。
- **切换 `ORDER_TRANSPORT`**:待确认记录绑定提交方式。有未确认的请求时不要切换,先用原方式把它对账完,再换。
- **bin-ws 断线**:行情模式会自动重连(日志 `bin-ws connection ended ... retry_in=...`,退避从 1 秒起、上限 15 秒),重连后重新登录并重新订阅。重连期间错过的订单事件需要补查；`openOrders` 仅反映当前挂单，不能证明断线期间的成交或未知请求结果。
- **盘口解码失败**:无效增量会清空盘口并重新订阅；无法解码的外层帧会关闭连接并由行情循环重连，不继续沿用可能漏更新的盘口。

## 本地验证

```sh
go test -race ./...
go vet ./...
```

本仓库用于展示 PRE API 接入流程；交易示例需要使用自己授权的测试账户，并按实时市场修改参数。

## 许可证

本项目采用 [Apache License 2.0](LICENSE)。第三方依赖保留各自的许可证。
