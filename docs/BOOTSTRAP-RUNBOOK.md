# 公开快照供给方引导（S3）

本会话只实现并在本地隔离链验收；未访问生产主机、未操作生产配置、未查询生产 RPC、未复制生产密钥。以下生产步骤全部由督导在另一个明确授权的生产会话执行。本流程不改变链规则、状态序列化、app hash 或 VERSION，也不授予新节点 validator 身份。

## 能力与限制

`emberchain start` 读取 home/config/config.toml 的 `[statesync]` 与 `[snapshots]`；launcher 的 validator 分支也走同一入口。供给默认 `interval=0`（关闭），`keep_recent=2`；消费默认关闭。配置示例见 config/validator.env.example。没有 `snapshot-interval` CLI 开关，也没有通用离线 tar importer。不要复制一个 state.json 冒充完整引导。

格式 1 是只读 tar 分块包：规范 State.Bytes 原文、state.archive 全部文件、state.chunk-spool 全部文件；不包含 CometBFT 数据库、node key、validator key、signing state 或任何 home 中的其他文件。元数据描述链 ID、状态格式版本、app hash、包大小和逐个 1 MiB chunk 的 SHA256，Snapshot.Hash 是完整包的 SHA256（它不是 app hash，也不是 trust hash）。archive/spool 被完整包 hash 与逐块 checksum 覆盖；archive 本身仍使用既有状态格式，未增加链上承诺。供给方必须是已核验的副本，不应把传输 checksum 当成独立共识证明。

当前硬上限：单包 256 MiB，最多 256 个传输块，metadata 32 KiB；keep_recent 范围 1–20。不压缩，解包总量也受 256 MiB 限制。超过预算会记录 `snapshot build ... exceeds ... limit`，不会发布半包。生产导出前先在授权副本上测量全 archive/spool 体积；超过上限时停止推广并安排后续容量实现，不能丢历史文件凑体积，也不能修改 State.Bytes。commit 仅固定文件 inode，打包/hash 在后台；后续 inode 替换/删除不改变已固定内容。构建忙时跳过一个周期，不拖住共识等待打包。

消费先检查 format/chain/height/大小/expected app hash；chunk 必须按序，错误/重复/损坏会拒绝整个 snapshot 并清理临时目录。最终完整 LoadState 加载 archive、校验已提交 bitmap 对应 spool chunks、重算 AppHash，与 CometBFT light client 提供的 expected 值比较。依赖先安装，state 文件的持久原子 rename 才暴露新高度。恢复失败不前进高度；重启清理中断构建/恢复临时文件。已有非零高度 home 禁止覆盖。

## 1. 督导认证真实公开锚

从 ceremony 的独立保管记录取得 **CometBFT 链 genesis.json 原始字节**，核对 chain_id、genesis_time、initial_height、初始 validators（地址、公钥、power）。不要用模型 genesis.pb，也不要用仓库中 staging 的 ledger-genesis.json。对原始文件运行：

```sh
sha256sum "$CEREMONY_GENESIS"
```

把该 exact-byte 文件公开为不可变 HTTPS 下载，下载后再算 SHA256，与独立 ceremony 记录逐字比对。RPC `/genesis` 返回 JSON 封装，重新排版/序列化后的字节摘要不能冒充 ceremony 原始摘要。本会话没有足以认证现网 exact genesis 的材料，因此这里不编造一个 SHA256 或 trust hash。

在有效期内选择近期高度 T，从至少两个不同运营方 RPC 取得 `/commit?height=T`，通过可信本地节点或 CometBFT light client、独立已信任 checkpoint 验证签名、validator set 演进及链 ID。**trust_hash = T 的 header hash**；不是块内 AppHash，也不是 Snapshot.Hash。两个 RPC 一致只是见证，不能代替最初的独立信任。快照 H 的应用状态 hash 对应 **H+1 header.AppHash**，H+1/H+2 的 headers、validators、consensus params 必须仍可用。

公开 manifest 至少记录这些真实值（值由督导填写并独立复核后才发布）：

```json
{
  "chain_id": "VERIFIED_CHAIN_ID",
  "genesis_url": "https://PUBLIC_HOST/immutable/genesis.json",
  "genesis_sha256": "VERIFIED_64_HEX_SHA256_OF_EXACT_BYTES",
  "initial_validators_url": "https://PUBLIC_HOST/immutable/initial-validators.json",
  "trust_height": 123456,
  "trust_header_hash": "VERIFIED_64_HEX_HEADER_HASH",
  "trusted_header_time": "VERIFIED_UTC_TIME",
  "trust_period": "168h0m0s",
  "expires_at": "TRUSTED_HEADER_TIME_PLUS_TRUST_PERIOD",
  "rpc_servers": ["https://INDEPENDENT_RPC_A", "https://INDEPENDENT_RPC_B"],
  "snapshot_peers": ["PUBLIC_NODE_ID@PUBLIC_P2P_HOST:26656"],
  "witnesses": ["INDEPENDENT_VERIFICATION_RECEIPT_A", "INDEPENDENT_VERIFICATION_RECEIPT_B"]
}
```

trust_period 不可超过链的信任/解绑安全窗口；此处 168h 是配置默认值，不是对生产安全窗口的认证。至少在到期前一半周期刷新，并在节点异常/供给停止时立即撤销旧 manifest。保留每次发布的 immutable receipt，发布端显示时间与剩余有效期；客户端启动前检查，而不是忽略过期错误重试。当前启动预检会拒绝错链、错误 header hash、过期/未来 anchor；随后 CometBFT light client 验证 commit 和 validator 演进。

## 2. 督导从 validator 导出可信应用快照

优先使用已追平的专用非签名副本作为 donor。如果首次只有 validator 可提供状态，由督导评估 I/O、磁盘与 quorum 预算，在**一台** validator 的受控维护窗口开启供给；不得同时重启 quorum。先在同版本隔离副本验证容量。供给开关不要求启用 statesync 消费：

```toml
[snapshots]
interval = 1000
keep_recent = 2
```

这些值是示例，按实际出块速度、包体积和发现/下载时长计算：`interval × keep_recent × block_time` 应明显大于 discovery + light verification + 完整下载时间。过短保留窗口会让已公布快照在下载前被删；增加 interval 或 keep_recent 后重新验证，不能靠重试声称成功。

现有 ABCI producer 在 commit 后导出高度 H 的不可变包到应用 state 路径旁的 `.snapshots/`；有效目录内有 snapshot.json 和数字 chunk 文件，点开头目录不表示完成。ABCI ListSnapshots 只公开完成包。核对元数据 H、chain、app_hash，并用上节信任链验证得到的 H+1 header.AppHash 比对。可保留完成目录作为审计备份，但它不能被直接离线导入新节点。不要拷贝整个 validator home；尤其不能携带 config/priv_validator_key.json、data/priv_validator_state.json、config/node_key.json。

## 3. 督导引导专用非验证供给节点

在独立主机安装经过核验的同版本二进制，新建 home（不要复用已签名 validator home）。`init-validator` 只是产生本机独立 node/consensus keys，不会加入验证集合；这些新 keys 从不从 donor 复制：

```sh
emberchain init-validator --home "$NEW_HOME"
install -m 0600 "$VERIFIED_GENESIS" "$NEW_HOME/config/genesis.json"
sha256sum "$NEW_HOME/config/genesis.json"
```

确认摘要与公开 manifest/ceremony pin 一致。设置 config.toml 的独立消费/供给配置；以真实独立 endpoints 和已验证的近期 T/hash 替换下面示例，不能复制占位值启动：

```toml
[statesync]
enable = true
rpc_servers = "https://rpc-a.example.org,https://rpc-b.example.org"
trust_height = 123456
trust_hash = "REPLACE_WITH_VERIFIED_64_HEX_HEADER_HASH"
trust_period = "168h0m0s"
discovery_time = "15s"
chunk_request_timeout = "10s"
chunk_fetchers = 4
temp_dir = ""

[snapshots]
interval = 1000
keep_recent = 2
```

至少两个 RPC 必须由独立运营方提供；程序能检查 distinct URL，不能替操作者认证运营独立性。`--peers` 必须包含可达 donor P2P peer；只有 RPC 地址不能传输 ABCI snapshot。

```sh
emberchain start --home "$NEW_HOME" \
  --p2p tcp://0.0.0.0:26656 --rpc tcp://127.0.0.1:26657 \
  --peers "$VERIFIED_SNAPSHOT_PEERS"
```

检查日志 `snapshot installed height=H app_hash=...` 及 CometBFT 完成 state sync，/status 高度持续上升、catching_up 最终 false。用独立 light-verified H+1 header 比对 H 的 hash；继续至少 20 块，在同高度比较供给方与新节点 header.AppHash。抽查历史 archive 的实际读取及 chunk spool receipt 的实际读取；本地对应门禁见下面 evidence。不能只看 RPC 通或状态文件高度非零。

完成追块/历史依赖验收后，新节点可以保持供给、关闭其 `statesync.enable`（下次维护时生效）；不向治理提交 validator 加入交易。确认其生成的快照也通过 H+1 app hash 比对，才可把它作为公开供给方。validator 上临时供给可由督导关闭，保留非签名专用供给方运行。

## 4. 公开访问与回滚

公开 P2P 地址/peer ID 与只读 state-sync RPC，不公开私钥、内部管理端口或 debug 接口。deploy/public-relay.py 核对：`status` 发现高度，`commit`/`block`/`header` 取 header/commit，`validators` 取验证集，**唯一新增的只读方法 `consensus_params`** 取恢复所需参数；`genesis`/`genesis_chunked` 取 genesis。原有广播方法未扩权，也未新增 unsafe/admin 代理。按当前 CometBFT v1.0.1 state provider/light client 的调用核对，不把任意方法放行。

失败时停止新副本，保留日志，丢弃它的恢复临时目录/新建 home 并使用独立新 keys 重试；绝不回滚已投票 validator 的 signing state。关闭供给只影响本地传输文件，不改既有 app state；旧版本仍能 LoadState（State.Bytes 从未改动）。撤销不可用公开锚、发布新 checkpoint，不伪造旧高度。

## 隔离验收与证据

测试源码：services/chain/snapshots_test.go、cmd/emberchain/statesync_test.go、deploy/test_public_relay.py。完整输出保存在 services/chain/testdata/s3 与 cmd/emberchain/testdata/s3。没有以单元测试替代真实网络传输：网络门禁运行独立 donor、witness、全新 receiver，通过真实 P2P state sync，再逐块将 donor 的实际区块重放到关闭 snapshots 的基线应用，核对 H 及 H 后至少 20 块的 app hash。

```sh
export PATH=/usr/local/go/bin:$PATH
export EMBER_AUDIT_TEST_PYTHON=/root/autodl-tmp/ember-gpu-venv/bin/python
export S3_TEST_OUTPUT=/tmp/s3-network
# 全套触及的 Go package tests，含真实本地网络测试
 go test ./services/chain ./cmd/emberchain -count=1 -v
python3 -m unittest discover -s deploy -p 'test_public_relay.py' -v
bash -n deploy/ember-role.sh
```

Python 路径按本机有效环境替换；现有 spot 跨语言门禁强制要求该变量。初次缺环境的失败输出也保留。网络初次失败暴露了极短快照保留窗口不足以覆盖 5 秒 discovery；调整测试 keep_recent=20 后通过，生产必须按实际时间预算选择保留窗口。这里没有声称已认证生产 genesis/anchor、已部署公开供给方，或已证明生产全量依赖一定小于当前包上限。
