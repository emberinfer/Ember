# fix3 / v0.10.0：P0 侦察与机制设计

日期：2026-11-25。基线：master `b48246e5c34fc517d9ec6a8f67008a5eeaf4d200`（v0.9.3）。本次只读代码与本机公开资料，只新增本文；没有访问生产主机、改服务、提交交易、动资金或注册。任务书指定的 `~/workspace/ember-supervisor/vultr-newuser-test-2026-11-25/REPORT-TOTAL.md` 本机不存在，测试现象采用任务书摘要，不能冒充本次复测结果。

证据路径：无前缀者相对此仓；`WEB:` = `/root/ember-web-push/`；`CMT:` = `/root/go/pkg/mod/github.com/cometbft/cometbft@v1.0.1/`（版本锁定见 `go.mod:8`）。以下 `file:line` 为入口，连续逻辑必要时给出多个入口。

**结论：D1 用现有链上完整签名工件作权威源，按 profile ID 分发并隔离网关运行上下文；D2 优先补齐 ABCI state sync，短期可提供经轻客户端锚定的完整一致性引导包。两者均不需要改变链规则、重置链或重新发布现有型档。** 链应用增加快照传输接口属于节点能力，不应改变交易有效性、状态序列化或 app hash。

## A. join 材料流

### A1. Python configure / join 的实际来源

1. 发布包路径：`release/build.sh:65` 按架构复制 CPU requirements；`:66` 把 `chain-genesis/ember-*` 拷到包根 `profiles/`；`:73` 复制 `site/network.json`。安装器源文件实际是 **`deploy/install-release.sh`**，不是 `release/install-release.sh`（打包重命名见 `release/build.sh:75`）；安装器 `:48` 复制 profiles/lib/docs，`:49` 复制 network.json。
2. configure：`node/agent/network.py:133` 的 `main` 读取 `--network-parameters` **本地 JSON 文件**（`:141`），`node_config` 在 `:46` 固定选择 `current_profile`，`:53`–`:55` 生成 profile ID 和两个 SHA256 pin；`:74` 读取 `profiles/<id>/{genesis,manifest}.pb` 验 SHA256。这些文件用于 configure 校验，不作为 JOIN 时加载工件的来源。`:72` 还要求 `access.gateways[*].profiles` 精确包含目标 ID（选择器 `:12`，无匹配 `:33` 报错）。默认 device 为 CPU（`:77`）。
3. JOIN：`node/agent/join.py:165` `_join_single` 内 `authenticate`（`:192`）从种子的 challenge JSON 取 base64 `genesis`、`manifest`，`:195`–`:200` 解码、验签、校验 ID 和 manifest digest；`:204`、`:207` 校验可选 release SHA256 pin；`:210` 防重连材料替换。`deploy/ember-role.sh:26` 把配置 pin 传给 join。工件实际由种子握手送来，configure 的本地工件不会自动替换种子的错误响应。
4. URL / 其他：network.json 的 RPC、seed、gateway 是访问端点，不是 configure 自动下载工件的 URL；当前 configure 没有 URL fetch，只有 `Path.read_text/read_bytes`（`node/agent/network.py:75`, `:141`）。运行时权重来自本地 model path 或 manifest 的 artifact/source 获取机制（`node/agent/join.py:246` 起）；模型权重与两个小型签名工件不能混同。JOIN 的链查询用于 trust（`:212`–`:223`），当前不是用 `/model/<id>` 返回材料替代 challenge。

### A2. Go 种子 / JOIN 发什么、按什么索引

握手实现主要在 `services/tunnel/`，由 `cmd/emberd` 接线，非 `services/gateway/` 独立材料服务。`services/tunnel/tunnel.go:25` 的 JoinData 含 Genesis、Manifest、Nonce、Questions；`:53`–`:56` 的 Server 只有单份 Genesis/Manifest 指针和字节；`:84` 每次发这同一对材料及随机挑战。`:100` 把回应的 ResourceProfile 与此单份 genesis 的 ID、root、execution version 对比。

来源是启动时本地 `--genesis` / `--manifest` 文件：`cmd/emberd/main.go:351` 解析并验 genesis，`:366` manifest 默认同目录，`:370` 读文件，`:378` 验签，`:385` 验 root，`:388` 赋给 relay。`:390`–`:393` 同时把 API 的 ChainID/Profile/Manifest 固定为这份工件。不是按任意 requested profile ID 查表，也不是 JOIN 时从链上拉取。demo 构造材料（`cmd/emberd/main.go:440`）不属于生产发布路径。

### A3. 发布在链上存什么、genesis 如何产生

**不是只有根哈希：两份完整签名 protobuf 都上链并进入状态。** `services/chain/publishing.go:50` 的 Publication 定义含 `Genesis []byte`（`:55`）、`Manifest []byte`（`:56`）、publisher、height、manifest_root、origin、previous_profile_hash、bond 与释放条件、invalid。`:210` 的 Publication 赋值把 action 两份原始字节复制保存；`modelArtifacts`（`:268`）优先返回 publication 工件。读取入口 `services/chain/app.go:518` `/model/<id>` 返回模型记录。

`publish`（`services/chain/publishing.go:72`）校验同 publisher 签名、两份 chain_id、execution profile；`:91` 起重新算 manifest digest、genesis 原字节 SHA256、genesis 协议 digest；`:97` 起检查 artifact/config/tokenizer 和 HTTPS pinned Git origin。还派生 weight root、verified compute、model identity 并拒绝相同 model identity 重复发布（`:162` 起）。`node/agent/publish.py:79`–`:91` 显示客户端 action 字段和输出工件。

因此任务书的 0.5B `D5E4F8B9 @409016` 若走该 register_model 路径，至少提交 model_id、genesis_hash、profile_hash、完整 genesis、完整 manifest、origin(repository/revision)，可有 previous_profile_hash、pricing / compute 相关字段；链派生 publisher=交易签名者、registered_height、manifest_root、bond/release_height/required_requests、模型身份。具体字段值和交易完整哈希未查链，不能把结构推断说成交易取证。

`genesis.pb` 是 **型档** ChainGenesis：ID、manifest root、tokenizer hash、quant/dtype、execution profile version、价格、audit params / numeric audit policy、signer/signature；不是 CometBFT 链 genesis.json。基础工具 `chain-genesis/build.py:24` `build(model, output, chain_id, execution_profile_ver, content_policy)` 从本地 config/tokenizer/safetensors 和可选 policy 生成 manifest（`:37`–`:52`）及 genesis（`:74`–`:84`），输出两份 pb + 描述 JSON（`:93`–`:95`）。它随机生成模板签名 key（`:72`）。审计策略派生入口 `chain-genesis/build_audit_profile.py:29`，输入既有工件 + policy JSON，`:43` 写入 numeric audit policy，`:56` 重新签名。最终 publisher prepare 用自己的 `state-dir/identity.key` 重签（`node/agent/publish.py:48`, `:71`–`:91`）。

### A4. 第三方能否确定性重建

**已发布的新型档可以从公开链状态恢复 exact bytes，不需要发布者私钥或本地遗漏信息。** 从 `/model/<id>.publication.{genesis,manifest}` base64 解码即可（存储证据 A3），然后独立校验签名、publisher、ID、哈希。链上模型元数据 + 模型材料 + 工具也能核验 artifact 哈希与计算派生，但“重新运行 builder 重签得到完全相同 pb”是另一回事：builder 随机 key（`chain-genesis/build.py:72`），价格、calibration、layout 是发布选择，不能仅凭权重推断；publisher 私钥在 `state-dir/identity.key`（`node/agent/join.py:57`–`:71`），prepare 输出在 `output/{genesis.pb,manifest.pb,registration.json}`（`node/agent/publish.py:87`–`:91`）。私钥无需公开，也不应为修复分发问题而索取。

历史非 publication 模型可能仅有 hash；`services/chain/publishing.go:268` 的 `modelArtifacts` 回退 `Model.Genesis/Manifest`，`:274` 的 `validateModelArtifacts` 允许两者皆空。因此不能承诺所有历史 hash-only 记录均可凭哈希逆推出工件；此类需公开原始签名文件并比对既有锚。任务书 0.5B 是新 register_model，不能套用该历史限制。

## B. 可用性、报价与派单

### B1. 网关口径

不是“全静态”或“全动态”二选一：**型档运行上下文固定，候选节点、存活、stake 与价格动态。** `cmd/emberd/main.go:388`–`:393` 固定一份型档；`services/gateway/gateway.go:247` 拒绝不同 `q.Model`（unknown pricing profile）。本库通用文本 Gateway 并无网站式 `available` 布尔 catalog API；用户所见 available:false / model not available 必须继续追网站层，不能全部归因于 Go 返回值。

候选生成 `cmd/emberd/main.go:487`：遍历 Tracker.Alive（`:498`），排除 contribution unavailable，链读余额、SchedulableNodeProfile（`:503`–`:505`），并要求 tunnel 在线（`:516`）。`services/scheduler/scheduler.go:71` `Rank` 的 `:79` 同时匹配 chain/profile/manifest、online、stake/exposure、整模或分片、内存；`:107` Choose 无候选则报错；pipeline 要完整连续覆盖（`:124`）。故“注册未过期”不等于当前可路由。

报价：`services/gateway/pricing.go:152` `serveQuote` 整模先选在线候选，`:162` tokenize，`:167` context 检查，`:171` quoteFee；`services/ledger/dynamic_pricing.go:19` resolver 缓存最后成功的链报价而不使用 genesis 价格，`:42` QuoteFee。派单走 `services/gateway/gateway.go:262` MoE、`:267` pipeline 或之后整模 scheduler；现行固定运行上下文还包含 tokenizer、audit policy、pipeline 设置，不是加一个模型列表便够。镜像使用同一二进制时具有同样限制；这里未检查两台机器实际启动参数。

### B2. 网站 /api/models 是否同源

**不是同一 ready/schedulable 真值。** `WEB:backend/app.py:1944` build_models 从链模型/节点派生，`:2072` 遍历 models，`:2073` 过滤 RETIRED_PROFILES；`:2095` compute_deployment；`:2132`–`:2142` available = executors>=1 且 serving_now。普通型档 serving_now 看 fresh executor liveness，ONLINE_WITHHELD_PROFILES 则看 live pricing/context 查询。还有分片 alias 另行聚合（`:1904`）。

请求路由却有静态 `GATEWAY_BY_MODEL`（`WEB:backend/app.py:45`）、`GATEWAY_POOLS`（`:2588`），未知型档可回落默认网关（`:2822`）；`/api/models` endpoint `:3363`，下单 `:3734`–`:3735` 拒绝 unavailable。因此链上有节点可能网站仍没正确网关；网站显示可用也不保证实际 scheduler 能派到完整覆盖。需统一 readiness 定义，不要把节点计数当路由承诺。

### B3. 满足任意已发布型档的最小改动面

1. `services/ledger/` 增加按 ID 获取并验证完整 model publication 的客户端接口；缓存键至少 ledger chain ID + model ID + genesis_hash + manifest_root；查询失败明确 stale 状态。
2. `services/tunnel/tunnel.go` / `node/agent/join.py`：首帧显式请求 profile ID，种子按 ID 选工件发 challenge，后续签名注册绑定同一上下文；保留旧首帧走固定默认型档，绝不自动把未知 ID 改成 current_profile。
3. `cmd/emberd/main.go` / `services/gateway/`：按 profile ID 建独立 Gateway/审计/tokenizer/pipeline context，共享网络连接和链客户端，不共享可变型档字段；动态创建、限额缓存、并发去重。上线首期整模优先，分片必须覆盖全图、MoE/image 需相应能力，不能声称单个任意分片足够。
4. `node/agent/network.py` / `deploy/ember-role.sh` / network schema：增加显式 profile 选择，未打包者从链或工件缓存获取并验锚；gateway directory 从静态精确白名单改成可发现的 profile registry 能力，保留 default 提示；trust 配置与 transport membership 分开。
5. WEB backend 动态发现 route catalog，`/api/models`、quote、下单使用同一 profile readiness / route 结果。自动接纳合法 publication 是公共网关的**链下运营策略变更**，不是放宽链验证。客户端保留自己的 publisher trust 决策。

严格地说，“≥1 在役执行节点”仅对能完整执行且满足现行注册/签名/资源/审计约束的整模节点成立。单个局部分片、未支持 engine、失效 publication、无可达 tunnel、无请求 exposure 都不能变成 available；应返回具体原因。分发工件不应先要求已有执行节点，否则新型档永远无法完成第一个 JOIN。

## C. 账本节点引导

### C1. 快照接口与当前配置

`services/chain/app.go:27` Application 嵌入 `abci.BaseApplication`；全 `services/chain/` 未发现 ListSnapshots/OfferSnapshot/LoadSnapshotChunk/ApplySnapshotChunk 自定义方法。继承的实现是 `CMT:abci/types/application.go:68`, `:72`, `:76`, `:80`，全部返回空 response：无可用快照、无恢复能力。代码中 audit sampling snapshot 与 ABCI state sync 无关。

`cmd/emberchain/main.go:556` DefaultConfig + timeout 白名单加载；白名单说明 `:391`–`:413` 不含 statesync。`CMT:config/config.go:1039` 默认 StateSync 未设 Enable（Go 零值 false）。没有应用快照生产周期/保留数的实现；**snapshot-interval 不是本应用现有可用开关**，写进 toml 不能自动生成快照。根据此启动代码推断仓库版本启动的节点不提供 app snapshots，且无法仅编辑 toml 开启消费 state sync；不是对生产进程配置的现场认证。

### C2. 块 1 错误的确切层级与 state sync

校验点 `CMT:state/validation.go:24`–`:28`：block.Header.Version.App/Block 与本地 `state.Version.Consensus.App/Block` 比较，失败返回任务书的原字符串。这属于 CometBFT state validation（block execution 前），不是 protobuf 型档 hash mismatch，也不是 relay 自己拒绝注册。

本地 App 值来自 ABCI Info：`services/chain/app.go:65`–`:69` 空状态返回 AppVersion 常量（`services/chain/state.go:22` =11）；`CMT:internal/consensus/replay.go:262`–`:264` 在初始 LastBlockHeight=0 时以 Info.AppVersion 设置 `initialState.Version.Consensus.App`。对端块 Header 携带创世时实际协议版本，摘要观测为 App=5；Block=11 是 CometBFT block protocol，两边相同。故 Expected {11 11} vs got {11 5} 的第二位是历史 AppVersion 与新二进制初始声明不匹配。后续版本还受共识参数更新影响（`CMT:state/execution.go:656`），不能随意把常量改成 5 后宣称全历史可重放。

**正确 state sync 可以绕过从块 1 重放**：恢复可信高度 H 的应用状态和 CometBFT state，随后从 H+1 同步；state provider 用验证过的 H+1 header 的 Version/AppHash 构建 state（`CMT:statesync/stateprovider.go:160`–`:180`），app hash 从 H+1 取（`:103`–`:124`）。但当前空接口和未接线配置令此路径不可用；仅 `statesync.enable=true` 不是修复。不得跳过 header version 校验或替换历史块。

### C3. 链 genesis 的公开来源与真实性

必须区分三类：模型 `genesis.pb`；builder 的描述性 `chain-genesis/*/genesis.json`（`chain-genesis/build.py:86`）；CometBFT `config/genesis.json`（`cmd/emberchain/main.go:207` 创建 GenesisDoc，`:595` 默认 genesis provider）。后两者虽同名，不能互用。

现有公开链渠道是 network.json 发布的 RPC（`site/network.json:124`）的 `/genesis` 或 `/genesis_chunked`；代理明确允许这两种方法（`deploy/public-relay.py:12`–`:14`）。仓库可直接下载的 `site/profiles/ember-qwen2.5-1.5b-publisher-cpu-v1/ledger-genesis.json:1` 是 **ember-s25-publishing-staging，2026-11-11，单 validator**，绝不是当前 ember-testnet-1 的真 genesis；HTML `site/profiles/ember-qwen2.5-1.5b-publisher-cpu-v1/index.html:16` 的 Ledger verification anchor 仅属于该 staging release（`docs/PROFILE_RELEASES.md:10` 亦明确）。`site/network.json:8` / WEB baseline `:68`–`:71` 只给现网 chain ID，没有完整链 genesis pin。`docs/PUBLIC_NETWORK.md:3` 称现网 genesis 日期 2026-11-14，但文档自述不构成 exact bytes 证明。

因此答案为：**公共 RPC 可提供其当前配置的链 genesis；本地公开仓无足以认证该响应就是现链真 genesis 的独立原始字节锚，不能在本次只读本机侦察中确认摘要所用那一份 exact genesis。** 摘要中已收到块 1 并不足以独立证明 genesis 全文一致。实施时须公开 ceremony 原始 genesis、SHA256、初始 validator set、链 ID/time/initial height，并与可信 header 链核对；不能用 staging 文件或新造 genesis 补洞。

`WEB:site-src/content/start-ledger.md:20` 先要求 join inference/auditor，`:26` 转 Ledger Guide / 治理，`:34`–`:38` 明确前置 **ember-qwen2.5-1.5b-gpu-v1** 运行注册。不是可操作的非验证账本节点自助引导。`docs/VALIDATOR_GUIDE.md:9`–`:20` 只描述新网络 ceremony，不提供现网 bootstrap；`:7` 又正确禁止造新 genesis。gpu-v1 registration_frozen 是任务书观测，未在本次查链复核；网站 retired 列表确含该 ID（`WEB:backend/app.py:80`），更应删除它作为同步前置条件。

### C4. 未有快照时的替代路径（按短期实现代价）

以下均是可实现并形成受支持流程的选项；**当前 CLI 没有一条已经完整支持的现网自助 bootstrap 命令**（命令列表 `cmd/emberchain/main.go:41`–`:86`）。

1. **一致性全量引导包**：从专用非签名副本在同一 H 导出 app state、完整 archive/chunk 持久材料、CometBFT state/block stores 和必要公共 metadata；受支持 importer 核验 SHA256 清单、链 ID、H、app hash、commit 和 validator set 演进，再在全新 home 恢复；新建自身 node/consensus keys，绝不携带 donor 私钥/签名高水位。省去实现通用 ABCI 传输，但不是只拷一个 state.json。现有 `SaveState` / `LoadState`（`services/chain/state.go:1696`, `:1734`）、archive 随快照转移机制（`services/chain/history_archive.go:89`）可复用。实现成本低到中，数据库格式耦合与体积高。
2. **受验证 checkpoint importer**：只分发应用快照 + 历史依赖，使用 CometBFT light client / StateProvider 获取 H/H+1/H+2 的 validator、consensus params、commit 等，并通过 CometBFT bootstrap API 安装状态；避免分发整块 DB。与 state sync 共用恢复校验内核。成本中，必须证明上下文/版本/归档完整，不能简单写 JSON + 手改 height。
3. **历史兼容重放**：公开真 genesis、全历史块、各升级版本/activation manifests，先用历史初始 AppVersion 对应二进制启动，再逐步升级验证每个 app hash。当前空 Info=11 和升级桥证据在 C2、`services/chain/app.go:95`；不是改一个配置即成。成本和耗时最高，若历史版本/升级输入不全则不可承诺支持。

共同校验：H 的 app state 对应 H+1 header.AppHash；用独立 pin 的可信 height/hash + 有效 trust period，经轻客户端验证 validator set 演进和 >2/3 voting power commit。仅两个 RPC 返回相同 JSON、压缩包 checksum、发布者签名或“链 ID 一样”均不足以证明链上锚。现 `emberchain query --verify-state`（`cmd/emberchain/main.go:713`–`:752`）能算 whole-state hash/验 commit，但 `:740` 要求当前 validators hash 等于 genesis，**不能验证任意经历 validator set change 的现网 checkpoint**；需复用/扩展 light client，不可直接承诺该旧命令足够。

## D. 安装器与发布工具

### D1. 安装器结构与最小修改点

源路径纠正见 A1。`deploy/install-release.sh:6` 默认 python3.12，`:7` profile_family=cpu，`:12` 已有 --profile-family 参数，但 `:21`–`:24` 所有非 cpu 都立即拒绝；无 CUDA 安装分支。`:26` 检查 Python 命令，`:30`–`:40` **当前 master 已有 3.12 + import venv/ensurepip 预检**，不是完全无预检。`:61` 真正创建 venv，`:62`–`:65` 安装 requirements-cpu.txt 和 SPI。torch 固定项在 `node/requirements-cpu-amd64.txt:2`–`:4`（CPU wheel index, 2.5.1+cpu）；arm64 在 `node/requirements-cpu-arm64.txt:4` 使用 pin 的 wheel URL；架构选择在 `release/build.sh:65`，不是安装时 CUDA 自动检测。

最小修改：在 `:21` family 校验增加显式、受支持的 CUDA family（不静默自动换 torch），打包独立架构/runtime requirements，在 `:62` 根据 family 选择。在 `:30` 现有预检基础上检查 ensurepip bundling/可实际建带 pip venv 的能力，并在任何 prefix 拷贝前失败，给 Ubuntu `python3.12-venv` 明确修复命令；失败清理临时探针、不留半安装。摘要中的首次失败可能来自旧发布包或预检覆盖不足；本次未重现，不能宣称当前 master 完全缺该检查。CPU 安装路径必须保持可用。

### D2. torch 声明与生产规格核实边界

声明来自实际运行库，不是链配置写死：`node/engine/adapters/transformers.py:36` 取 torch.__version__，`:38` 取 torch.version.cuda，`:132` capabilities 输出；分片 `node/engine/shard.py:98` 同样取值；`node/agent/join.py:309` 写 ResourceProfile.torch_version。

校验不限定 `2.5.1+cu124` 枚举：`services/tracker/tracker.go:55`–`:62` 要求 torch/version 非空，CUDA 需 cuda_version/accelerator；链注册入口 `services/chain/state.go:821`–`:835` 校验签名/注册条件、published profile 和 GPU budget，并未在此设 torch 版本枚举；`services/chain/routes.go:38` RuntimeID 拼接 `engine/version|torch=...`，`:65`–`:67` 审计兼容性可要求 runtime/device 一致；range audit 对 torch 等值匹配（`services/chain/range_audit.go:25`）。真正可审计组合还受签名 numeric policy / engine capabilities 约束，装任何非空 torch 都被 tracker 接受不等于可生产服务。

本地证据 `docs/T2G-NO-GO.md:3` 记录 **2026-11-23 的 684 CPU 进程使用 2.5.1+cu124（CUDA hidden）**；`WEB:evidence/accept-split-golive-2026-11-15.md:39` 也记录 pipeline 的 cu124 runtime。这能证明曾在生产相关环境使用该 wheel，**不能确认它是 2026-11-25 所有生产 GPU 节点的现行统一规格**；`services/chain/testdata/audit_replay_gb10.json:8` 还有 2.14.0+cu130 的测试样本，更不可将 GPU 一律等同 cu124。测试样本不是在役认证。

准确结论：2.5.1+cu124 是应优先验证的 x86 transformers 兼容候选，当前 CPU release 则是 CPU wheel；本次未读取生产节点注册快照，不把候选规格写成已确认事实。实施验收应读链 active ResourceProfile（验签/高度/expiry）、检查 cuda/torch/engine/policy 组合并记录取证高度；每个 CUDA/架构 family 单列，GB10 等不可照抄 cu124。无需 SSH 或改生产来完成该读取核验。

### D3. 从模型材料到提交读回的现有命令面

当前文档 `docs/PUBLISHING.md:7` 从“已有 signed template”开始；`node/agent/publish.py:104` prepare 仍强制输入 genesis/manifest；不会从任意模型自动构建。基础 builder `chain-genesis/build.py:35` 限制 num_hidden_layers>=28，且要求 tokenizer.json/safetensors（`:31`, `:75`）；因此 **不能把它宣传为可原样构建 0.5B（通常24层）或任意架构的一键命令**。对于这种模型目前必须手工/专用脚本构造合规 signed template、audit policy、layout/compute 表，再 prepare；不可为了通过而伪造层数。

以下是现有 CLI 的完整组合示例（仅说明，不执行；`$MODEL/$RPC/$CHAIN_ID/$ID/$EXEC_PROFILE/$ORIGIN/$REV/$PUBLISHER_STATE` 均由调用者明确提供；输入权重已按固定 Git revision 下载）。适用于通过基础 builder 校验的 Qwen 类模型：

```sh
# 0. 预先固定 REV、下载完整 MODEL，准备经测量的 POLICY；不使用浮动 revision。
# Python 环境须有项目 protobuf、core、node 依赖。
export PYTHONPATH="$PWD/node:$PWD/proto:$PWD/core-py"
python chain-genesis/build.py --model "$MODEL" --output ./template-base \
  --chain-id ember-template-base --execution-profile-ver "$EXEC_PROFILE"
python chain-genesis/build_audit_profile.py --source ./template-base \
  --output ./template-audit --policy "$POLICY" \
  --chain-id ember-template-audit --execution-profile-ver "$EXEC_PROFILE"
# 若需要分片/MoE，另用 build_shards.py / build_moe.py 构造布局，不能直接套整模例子。
ember-node profile prepare --state-dir "$PUBLISHER_STATE" \
  --genesis ./template-audit/genesis.pb --manifest ./template-audit/manifest.pb \
  --model-path "$MODEL" --model-id "$ID" --profile-version "$EXEC_PROFILE" \
  --origin "$ORIGIN" --revision "$REV" --output ./published
ember-node profile verify --genesis ./published/genesis.pb \
  --manifest ./published/manifest.pb --model-path "$MODEL" --conformance --device cpu
emberchain query --rpc "$RPC" --path /publishing
# 在已明确授权的发布会话才资助 publisher 并提交；本次侦察不执行。
ember-node profile submit --state-dir "$PUBLISHER_STATE" \
  --chain-rpc "$RPC" --ledger-chain-id "$CHAIN_ID" \
  --action-file ./published/registration.json
emberchain query --rpc "$RPC" --path "/model/$ID"
```

读回后还要检查 tx deliver 成功、registered_height、publisher、两个 base64 原字节与本地逐字节一致，重算 genesis raw SHA256/profile digest/manifest root；不能只看 broadcast 成功。prepare 支持 canonical-config（`node/agent/publish.py:60`–`:64`）；submit 从账户 nonce+1 签名提交（`:128`–`:144`），但没有自动读回/恢复工件闭环。

手工缺口：resolve/pin origin；任意架构模板构造（尤其 0.5B）；数值校准与 policy 选择；拆分/compute 表；运行时兼容矩阵；prepare/verify/submit/readback 拼接；profile 分发/路由配置。`WEB:site-src/content/start-publish.md:147` 的裸 tx 示例还缺 model_id 和 origin，不能作为可执行完整 action。最低发布工具应把现有验证函数组合进 `profile publish`，同时提供 dry-run/inspect 与幂等读回，不掩盖需人工决策的 calibration。

### D4. 未转义尖括号占位符清单

扫描范围 `WEB:site-src/content/start-{use,executor,ledger,publish}.md`；以下为源码所有原始尖括号占位符（包括 backticks / fenced code 内者）。当前构建器不是 Markdown->HTML：`WEB:build_site.py:174`–`:178` 将替换 token 后的原文写入无扩展名 `/start/<role>`，故实际是否吞掉还取决于响应 Content-Type / 浏览器解释，不能仅凭 Markdown backticks 宣称安全。

| 页 | file:line | 原始占位符 |
|---|---|---|
| use | `WEB:site-src/content/start-use.md:38` | `<address>`, `<model>`, `<sha256hex(canonical messages JSON)>`, `<max_tokens>` |
| use | `WEB:site-src/content/start-use.md:42` | `<model>` |
| use | `WEB:site-src/content/start-use.md:87` | `<address>`, `<request_id>` |
| use | `WEB:site-src/content/start-use.md:92` | `<your-address-hex>` |
| executor | `WEB:site-src/content/start-executor.md:63` | `<your-user>` |
| executor | `WEB:site-src/content/start-executor.md:113` | `<profile-id>`, `<account>` |
| executor | `WEB:site-src/content/start-executor.md:128` | `<your-address>` |
| ledger | `WEB:site-src/content/start-ledger.md:1` 起全页 | 无尖括号占位符 |
| publish | `WEB:site-src/content/start-publish.md:147` | `<publisher.key>` |
| publish | `WEB:site-src/content/start-publish.md:148` | `<stored-nonce-plus-one>` |
| publish | `WEB:site-src/content/start-publish.md:150` | `<base64 of genesis bytes>` |
| publish | `WEB:site-src/content/start-publish.md:151` | `<base64 of manifest bytes>` |
| publish | `WEB:site-src/content/start-publish.md:152` | `<sha256 hex of the genesis bytes>` |
| publish | `WEB:site-src/content/start-publish.md:153` | `<protocol digest of the genesis>` |

修复优先让机器指令页响应 `text/plain; charset=utf-8` 并保持 .md 镜像；若提供 HTML 视图必须统一 escape 全文/代码节点后渲染。shell 例子优先使用具名变量和真实赋值说明，不在纯文本指令页替换成会被直接复制的 `&lt;...&gt;`。签名格式中的真实占位语义不得误改。

## D1 机制设计：链上工件 registry + 按型档隔离的服务上下文

### 权威数据与发布者一条命令

新增 `ember-node profile publish`（拟议命令，当前不存在）：输入模型目录、固定 origin/revision、ID、execution profile、经过验证的 policy/layout、publisher state；完成构建、哈希/完整性/运行兼容性预检、conformance，输出人可审阅摘要；在调用者明确授权提交时复用 prepare/submit，等待 commit 后读回并验证。支持 `--dry-run`；恢复时若同 ID 已存在且字节相同只读回，不重复支付 bond，不自动选择新 ID 绕过 model 唯一性规则。

材料已经存链上 publication，发布命令输出本地 release 备份及一个引用 ledger chain ID/model ID/anchor 的 receipt；可选内容寻址 HTTP/CDN 镜像仅为加速。**不新增必需上传服务、不要求 publisher 常驻 HTTP、不索取其私钥。** 对历史 hash-only profiles 使用已锚定的文件镜像回退；找不到 exact bytes 明确拒绝，不能重新签名替代。

### 种子取材与客户端验证

种子提供 `GET /profiles/{id}` metadata / 工件下载以及有界 registry cache；权威 fetch `/model/<id>`，校验 publication 非 invalid、genesis SHA256、profile digest、manifest root、双方签名/共同 signer、publisher 账户、ID 和 layout。不能混用 legacy 的 raw manifest SHA256 profile_hash 与 publication 的 genesis digest（`docs/PUBLISHING.md:33`–`:37`）。缓存保存原始字节，不 parse/re-serialize 改变 exact-byte hash。缓存 stamp 含查询高度及可信链身份；失去链连通时短期已验工件可分发，但 available/报价必须标 stale 并遵守现有链报价失败策略，不把缓存冒充当前链状态。

JOIN 首帧携带 model/profile ID（可选预期 hash），服务端选 registry entry 后发原有 challenge；所有 profile 验证及后续 job/verdict 都使用该 entry。限 ID 长度、拒绝路径穿越、限原文/解压/缓存大小、fetch 并发去重，未知 ID 不退回默认材料。

客户端显式选择 profile，使用可信账本锚下的 model record 校验收到的 **原始** pb；校验 root/signatures/publisher 与本地 trust，再核验 weights/tokenizer/config。保留 release pin 与 reconnect pin。当前 `_chain_json`/普通 RPC 查询本身不是 Merkle/light-client 证明，不能把“从链 RPC 读到”说成无需信任的已验证链；最低要求 operator pin 的 RPC/可信本地账本，强化路径用 whole-state + light-client verified header，沿用 D2 的 trust anchor 机制。

### 自动可服务与网站一致性

Gateway registry 为每个 ID 产生隔离 context（ChainID/Profile/Manifest、policy、tokenizer/context、价格引用、pipeline/MoE/image handler）。不把全局 `api.ChainID` 在请求间来回改写。最小第一阶段可按 registry 自动管理独立单型档 worker，并由统一 router 分发；它复用既有单型档逻辑，代价是多进程与资源限额。后续可进程内 map，验收要求相同。对于新 0.5B 整模路径应无需手工加 endpoint/白名单。

公共网关将“合法且未 invalid 的 publication 自动发现”设为明确链下配置；支持运行时能力的型档只要有至少一合格在线整模执行者便能报价派单，分片以完整覆盖为准。保留用户自己的 trust policy；公共服务是否要求额外审计资源须显示原因，不能仍用无入口 vetting 阻断所有新 profile。网关返回动态 catalog（supported/ready/reason/height/last_seen/route），网站与镜像按同一 schema 消费；quote 与 dispatch 使用同一资格判断并处理节点中途离线，不承诺 quote 后资源永远存在。

### 是否需要动链规则（独立决策）

**不需要。** 工件全文已上链；发布可验证且节点注册已校验 profile；链下按 ID 分发、改公共网关 admission policy、客户端校验、state sync 传输都不需要修改 register_model、model 唯一性、审计/价格规则或治理 timelock。本方案无待用户批准的链规则变更，因此没有标红的“必须改链”提案。若实施者遇到特定 engine 规则不兼容，应作为单独变更报告原因，不借本次分发修复放宽链校验。

## D2 机制设计：受锚定 state sync，分离同步与治理

1. 快照内核：在 commit 后取一致高度 H，导出规范 State.Bytes 原文、全部 archive 依赖、后续验证/裁决所需 chunk spool 数据及版本描述；逐块 checksum、总量限制、格式版本、H、chain ID、app hash/依赖根清单。快照只读不可变，保留最近多个高度，后台压缩，绝不持 ABCI 全局锁做长时间压缩/传输。现 hash 是 State.Bytes SHA256（`services/chain/state.go:473`–`:495`），不得为“方便快照”改序列化或 app hash。
2. 实现四个 ABCI 方法：List/Load 提供可用不可变快照；Offer 验 format/chain/height/上限，接收 CometBFT 已验证的 expected app hash；Apply 在独立临时目录校验每块和完整状态，LoadState 完整加载归档后重算 AppHash，与 expected 比对，原子替换空 home 的应用状态；失败拒绝并清理，不进入高度伪成功。需要 H+1/H+2 可用 header/validators/params 和有效 trust period。
3. launcher 显式解析/验证 statesync enable、至少两个独立 RPC、trust height/hash/period、发现/超时、temp dir；与 genesis chain ID/可信 checkpoint 绑定，禁止静默忽略。快照**提供**与状态同步**消费**是不同开关；快照提供方不需要对自身启用消费 sync。新增 app snapshot interval/retention 配置，默认保守，不能沿用不存在的 SDK snapshot-interval 假设。
4. relay 的 `/commit` `/validators` `/block` 等已在 allowlist（`deploy/public-relay.py:12`）；state provider 的 consensus params 等 RPC 需求须逐项核对，必要时增加**只读**方法，不扩大成任意管理代理。P2P snapshot 需要可达的供给 peer，不能只公布 RPC。
5. 公共锚：发布真 genesis 原始下载 URL/SHA256、初始 validators、近期 trust height/header hash、checkpoint 有效期与独立见证来源。trusted block hash 是 header hash，不是 app hash；快照 H 的 app hash 锚在 H+1。RPC 互相见证仍需独立初始信任，不以 donor 签名代替共识证明。定期刷新 checkpoint 防过期，新用户在导入前检查而非导入后忽略错误。
6. 若 state sync 尚未交付，发布 C4 的一致引导包 + 校验 importer，明确数据库版本/依赖清单；优先从专用副本导出，避免为此停生产 validator。没有完整验证前，指南明确“bootstrap 尚不可用”，不给必定卡高度0的假命令。保留历史重放作为维护者恢复工具，非首选用户路径。

指南改写分为两条独立路径：

- **任何人可自助同步非验证账本节点**：下载验证 release/真链 genesis/近期 checkpoint，初始化新 home、独立 node key，state sync 或导入已验证 bootstrap，连公开 peers，检查 status height/catching_up、Info app hash 与独立 RPC 一致，持续追块。无需先注册 inference、更不要求 frozen gpu-v1，无需先获现任 validator 提案。
- **治理准入为 validator**：同步完成后说明 validator key、operator 身份、readiness proof、当前集合、现任 operator 提案、timelock、激活高度与监控。现任 validator 权限与时延见 `services/chain/validators.go:198`–`:205`，不把可自助同步写成可自助加入投票集合。密钥安全与签名高水位沿用 `docs/VALIDATOR_GUIDE.md:24`–`:27`。

## 实施任务清单（后续会话实施，本次不改功能）

| 断档/任务 | 修改文件/组件（新增文件明确标注拟新增） | 风险 | 验收 |
|---|---|---|---|
| D1-a 工件 registry | `services/ledger/chain.go`，拟新增 `services/ledger/model_artifacts.go`；`cmd/emberd/main.go`；缓存与 HTTP handler | 中：混用 hash convention、stale/invalid、缓存滥用 | 单测 publication/legacy 双路径、错误 signer/root/ID/bytes、invalid、并发 fetch；已有 0.5B 从链恢复 exact bytes，无 publisher 本地文件仍成功 |
| D1-b 多型档 JOIN | `services/tunnel/tunnel.go`、`node/agent/join.py`、`node/agent/network.py`、`deploy/ember-role.sh`、`site/network.json` schema/目录与 release checker | 高：首帧兼容、多型档身份串用 | 两不同 profile 同时 join 各取正确工件，未知 ID fail closed；旧客户端默认路径可用；跨 profile 重放/切换/重连材料替换拒绝；未打包 profile configure 可用 |
| D1-c 路由/自动接纳 | `cmd/emberd/main.go`、`services/gateway/gateway.go`、`pricing.go`、`availability.go`、pipeline/审计 job context、`config/trust-policy.json` 对应链下策略 | 高：并发串上下文、unsupported engine、资源耗尽 | 新 profile + 1 合格在线整模节点，自动 catalog/quote/执行/结算；零节点/失联/过期/invalid 不 available；两 profile 并发无混淆；分片缺任一阶段失败；unified/镜像无回归 |
| D1-d 网站一致性/指南 | `WEB:backend/app.py` 的 build_models/GATEWAY_POOLS/quote/下单，`WEB:site-src/content/start-publish.md`、`docs/PUBLISHING.md`、`docs/QUICKSTART.md` | 中：默认 gateway 回落、缓存口径 | catalog 与下单同源，无手工模型表也可服务 0.5B；停节点后按公开 TTL 变 false，恢复后 true；没有无入口 vetting 指令 |
| D2-a 快照核心 | 拟新增 `services/chain/snapshots.go`；`app.go`/`state.go`/`history_archive.go`/chunk spool 复用；不改 State.Bytes 协议 | 高：遗漏历史依赖、app hash、阻塞共识 | 本地多节点快照/恢复到 H 后跨 H+1 持续追块，对照 app hash；归档历史裁决/appeal、chunk读取可用；损坏/乱序/重复/恶意大小/中断/过期锚拒绝；重启恢复一致 |
| D2-b 接线与公开锚 | `cmd/emberchain/main.go`、`deploy/ember-role.sh`、`config/validator.env.example`、`deploy/public-relay.py`、`site/network.json`、拟新增公开 bootstrap manifest/verify 命令 | 高：忽略配置、信任错链、validator set 演进 | 从空 Ubuntu/home 用新二进制成功 state sync；覆盖 validator set change；无块1版本循环；验证 H+1 app hash，错误 genesis/trust hash/过期锚 fail closed；无生产签名 key 出包 |
| D2-c 暂替 bootstrap | 拟新增导出/导入命令与一致性包 schema，`services/chain/history_archive.go`、`cmd/emberchain/main.go`，复用 D2-a 校验内核 | 高：Comet DB/app 高度不同、泄漏 signer、数据库兼容 | 全新独立 home 验包后追块，损坏/错高度/错 validator 演进拒绝；目录 allowlist 无私钥/凭据；不将原始复制流程冒充已支持命令 |
| D2-d 指南 | `docs/VALIDATOR_GUIDE.md`、`docs/VALIDATORS.md`、`docs/PUBLIC_NETWORK.md`、`WEB:site-src/content/start-ledger.md` | 低：错误承诺准入 | 新人不注册 inference 即可同步；治理部分明确现任提案/timelock；不指向 frozen gpu-v1；链 genesis 与型档工件逐一分清 |
| D3-① 发布命令 | `chain-genesis/build.py`、`build_audit_profile.py`、架构 builder、`node/agent/publish.py`、release 打包入口、`docs/PUBLISHING.md`、WEB publish 页 | 高：随机模板/架构假设、不可逆 bond/唯一模型槽 | 0.5B/1.5B 各在隔离测试链完整 build→verify→submit→readback，原字节/身份/compute 正确；dry-run 不提交；重复运行不二次支付；失败原子退出；policy 未测量明确拒绝或要求输入 |
| D3-② CUDA 安装 | `deploy/install-release.sh`、拟新增架构 CUDA requirements、`release/build.sh`、`node/engine/device.py` 所需预检、`docs/HARDWARE.md`、WEB executor/publish 页 | 中到高：wheel/驱动/架构/审计矩阵不兼容 | CPU 路径不变；x86 CUDA 指定 family 实装，torch/cuda/transformers 声明与实际一致；无 CUDA 或 unsupported arm64 明确拒绝；conformance + policy 中 CPU/GPU replay，不靠版本字符串假验收 |
| D3-③ venv 预检 | `deploy/install-release.sh:30`、release 安装文档/现有安装测试 | 低：旧包与源码不一致、半安装 | 纯 Ubuntu 只有 python3.12 无 venv 时，在写 prefix 前一次性说明安装 python3.12-venv；修复后首次正式安装成功；有完整环境直接成功 |
| D3-④ 占位符 | `WEB:site-src/content/start-*.md`、`WEB:build_site.py`、响应 headers 配置/HTML 视图 | 低：内容被吞或实体被复制进签名/命令 | 构建产物与浏览器 DOM/textContent/HTTP Content-Type 同时检查；`<model>` 与 `<your-address-hex>` 可见；plain/.md/HTML 三路径复制命令正确；无 token 遗漏 |

SHA256SUMS.sig 缺失不在本列车本任务处理范围；本次未补签或修改 release 资产。v0.10.0 bump/发布由后续实现会话统一完成，不在侦察提交中提前改 VERSION。

## 风险、发布次序与回滚

本节是未来实施运行手册设计，不表示已获准本次操作生产。先隔离测试链验收，再 canary 非签名节点，最后滚动网关/种子/快照供给；不要同时更换链状态规则和安装路径。

| 涉及处 | 上线约束 | 回滚方法 |
|---|---|---|
| 生产种子多 profile / 新握手 | 加兼容首帧和 registry feature flag，旧 current_profile 路径保留；一次一台 | 关闭 registry/JOIN-v2，路由切回旧 seed endpoint，恢复旧二进制与 env；保留旧工件 exact bytes 和缓存；不撤销链上 publication 或改其签名 |
| unified / 镜像网关与自动接纳 policy | profile context 隔离，动态路由先 canary；新旧实例并存，镜像逐个切换；新接纳 profile 限额 | 先停止接新单，排空/完成或按现有协议取消 in-flight holds，再切回旧路由和 trust policy/旧二进制；不要直接杀进程遗留冻结，也不通过重新发币弥补 |
| 网站 backend 动态目录/网关池 | available/quote/dispatch 共用 catalog，缓存有 schema version | 切回先前 backend 配置/版本并失效新 catalog 缓存，保留现有订单记录与 traces；恢复公开状态文案，不删除链注册 |
| 生产 validator / 专用副本快照供给 | 快照功能默认关闭，先副本验证 I/O/锁/磁盘预算；不得同时重启 quorum；无状态格式升级 | 关闭 snapshot producer/清理未引用快照，恢复旧启动参数/二进制；已有 app state 必须仍能被旧版本加载。若不满足该兼容性，必须在上线前另立迁移/回滚设计，不能现场覆盖旧 DB |
| 新同步客户端 / bootstrap importer | 只装入新建独立 home，donor 不复制身份；验证完原子启用 | 停止该新节点，丢弃临时恢复 home，重建独立 keys/home 并使用旧受支持路径；绝不向已投票 validator 回滚 signing state，绝不复用 donor signer |
| relay RPC/P2P 与公开锚 | 只读 allowlist 最小扩充；明确 trust hash/有效期，镜像多源 | 恢复旧 allowlist/端口映射与前一版 metadata；使不再供给的 checkpoint 明确失效，发布新 anchor，不伪造旧高度；现有同步节点不退回旧链状态 |
| CUDA 安装与 venv | 新 prefix/venv 安装，禁止覆盖运行中共享环境；runtime 家族显式 | 停节点并等待在途处理后切回旧 prefix/env/CPU family，原 identity/registration state 留存；runtime 变更需按现行注册更新规则处理，不谎报旧 torch |
| WEB 指令页 | 保留 raw 文本与正确 Content-Type，HTML 输出统一 escape | 回滚网站构建和 headers 到已验证版本，清缓存；这只影响文档，不修改线上节点配置 |

最高风险是把“材料存在链上”误当“任意 RPC 返回可信”、把“一个分片在线”误当“整模可服务”、以及只恢复 state.json 遗漏归档而在后续争议时分叉。验收必须覆盖信任锚、完整执行覆盖、历史依赖与 H+1 app hash 四者。生产 GPU 统一 runtime、真 genesis exact-byte pin 仍需后续以公开只读证据完成认证；本文已给出不能凭代码直接断言的边界，未以猜测填充。
