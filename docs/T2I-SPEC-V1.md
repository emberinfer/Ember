# T2I v1：初始锚、prompt、latent 与确定性采样器

状态：**T0 冻结规范**，2026-11-23。规范 ID `philox-gauss-v1`；首发型档
`ember-stable-diffusion-1.5-v1`，执行型档 `diffusers-sd1.5-fp32-v1`。
后续工作包必须遵守本文件；任何字节、舍入、常数、序列或公式的变化必须另起规范版本，
不得通过重算本版向量来接受变化。本文只定义参考语义，不激活协议、不改链上状态。

设计依据：施工图 DESIGN-t2i §1.3、§2.1–2.3、§3.2–3.3；实验报告仅证明
同精度跨硬件单步复算可行。实验的 torch RNG 哈希不属于本版向量；实验测得的
RangeBand 也不能替代新型档的发布标定。SD1.5 / fp32 / 无内容审核为全车既定口径。

## 1. 数值基础与型档

- `F(x)` = IEEE 754 binary32，roundTiesToEven；每个下述指定的基本运算后立即舍入。
  不允许 FMA、重结合、fast-math、FTZ/DAZ 或扩展精度保留中间结果。整数到 fp32
  的转换同样 roundTiesToEven。加减乘除的操作数均为 fp32；参考实现用足够精度计算
  单次操作，再立即收敛到 fp32。负号是翻转符号位，不引入新舍入。
- 以下记 `a ⊕ b = F(a+b)`、`a ⊖ b = F(a-b)`、`a ⊗ b = F(a*b)`、
  `a ⊘ b = F(a/b)`；运算括号是强制求值顺序，不允许消去看似多余的运算。
  本文件中循环必须按升序执行；所有累加均为顺序累加。
- SD1.5：C=4，VAE 空间因子=8；图像高×宽只允许 512×512、512×768、768×512。
  完整 latent shape 分别为 `(1,4,64,64)`、`(1,4,64,96)`、`(1,4,96,64)`。
  所有 shape 都含 batch=1，不能在哈希时省略 batch 维。
- 执行 dtype=`f32`；本包双实现及向量只认证该 dtype。以后若发布不同 dtype，必须
  有独立执行型档、dtype 编码和向量；不能混用审计池。转换点见 §2.4。
- seed 是 uint64，无符号十进制输入，范围 `[0,18446744073709551615]`；禁止
  有符号溢出、取绝对值、取模接受越界输入。向量 JSON 的 seed 用十进制字符串，
  避免 JavaScript 的 53 位整数精度损失。

常数由下列 **fp32 位模式**定义，不由十进制解析器或平台 π 决定：

| 名称 | 位模式（数值 uint32，不是字节序） | 用途 |
|---|---|---|
| PI | `40490fdb` | π |
| TWO_PI | `40c90fdb` | 2π |
| HALF_PI | `3fc90fdb` | π/2 |
| LN2 | `3f317218` | ln 2 |
| INIT_SCALE | `3e3a8588` | F(0.18215) |

## 2. `philox-gauss-v1`

### 2.1 Philox4x32-10

对每个元素下标 `i` 单独执行一次十轮 Philox。不是每四个元素共享一个 counter，
不跨元素缓存 Gaussian 对，不依赖设备、线程数、分块方式或之前生成了多少元素。

- 128 位 counter 是数值 i 的零扩展：`(c0,c1,c2,c3)=(i低32,i高32,0,0)`；
  支持的元素下标 `< 2^64`。64 位 key 为 `(k0,k1)=(seed低32,seed高32)`。
- 无符号 word 序列是 `c0,c1,c2,c3`，每 word 编码成 4 个小端字节；从 RNG
  字节流取 uint32 时必须小端解码，不允许宿主端序 reinterpret 改变数值。
- 乘数 `M0=0xd2511f53`、`M1=0xcd9e8d57`；Weyl 增量
  `W0=0x9e3779b9`、`W1=0xbb67ae85`。
- 每轮先算两个完整的 uint64 乘积 `p0=M0*c0`、`p1=M1*c2`，同时更新：
  `(c0,c1,c2,c3)=(hi32(p1) XOR c1 XOR k0, lo32(p1),
  hi32(p0) XOR c3 XOR k1, lo32(p0))`；然后 key 分别加 W0/W1、模 `2^32`。
  恰好 10 轮，首轮使用原 key，返回第十轮 counter（最后的 key 增量不再使用）。
- 已知答案：seed=0，i=0 的输出四 word 为
  `6627e8d5 e169c58d bc57ac4c 9b00dbd8`（Random123 Philox4x32-10 KAT）。

### 2.2 均匀数与 Box–Muller 极坐标变换

对本元素十轮后的前两个 word x0、x1，各计算
`u_j = F(F(x_j >> 8) * 2^-24)`，故 `u_j ∈ [0,1)`。丢弃后两个 word。
右移后的整数最多 24 位，转换及乘二的幂均精确。

Box–Muller 的极坐标公式是 `r=sqrt(-2 ln(1-u0))`、`θ=2πu1`、`z=r cos θ`。
这里明确采用半径/角度式，而不是需要拒绝采样的 Marsaglia 变体；每元素恰好一个
counter、两个均匀数、一个输出。第二个 Gaussian `r sin θ` 不输出、不缓存。
使用 `1-u0`（范围 `[2^-24,1]`）避免 `log(0)`；不得换成 u0、epsilon clamp、
`nextafter` 或 `(x+0.5)/2^32`。

**逐操作定义**：`v = 1 ⊖ u0`；`l = log32(v)`；`a = (-2) ⊗ l`；
`r = sqrt32(a)`；`θ = TWO_PI ⊗ u1`；`z = r ⊗ cos32(θ)`。
即使 `l=+0` 产生 `a=-0`，sqrt32 也返回 +0。其余零保留 IEEE 符号位。

### 2.3 平台无关的 log32 / sqrt32 / cos32

这些名称专指本节的有限算法，不是系统 libm、Go math、Python math、NumPy 或
GPU 内建函数的别名。钉死基本运算顺序仍不足以钉死库函数，因此连近似算法也必须
固定。未来用其他近似或正确舍入超越函数替代，均需要新规范 ID。只要求下述调用域，
不提供任意负数、NaN 或无穷大的通用数学函数接口。

`log32(x)`，输入为 §2.2 的正正规 fp32：

1. `b=bits32(x)`，整数 `e=(b >> 23)-127`；
   `m=frombits32((b AND 0x007fffff) OR 0x3f800000)`，所以 `1≤m<2`。
2. `z=(m ⊖ 1) ⊘ (m ⊕ 1)`；`z2=z ⊗ z`；`p=z`、`s=z`。
3. 对整数 k=1..12：先 `p=p ⊗ z2`，再 `s=s ⊕ (p ⊘ F(2k+1))`。
4. 返回 `(F(e) ⊗ LN2) ⊕ (2 ⊗ s)`。

`sqrt32(x)`，输入为 §2.2 的非负有限 fp32（含 -0）：

1. 若 x 等于零，返回位模式 `00000000`。
2. `y=frombits32((bits32(x) >> 1)+0x1fc00000)`，这里加法为整数加法。
3. 恰好 8 次（不提前收敛退出）：`y=0.5 ⊗ (y ⊕ (x ⊘ y))`，返回 y。

`cos32(θ)`，θ 为 §2.2 的 fp32，范围 `[0,TWO_PI)`：

1. `x=θ`。若 `x>PI`，设 `x=x ⊖ TWO_PI`。初始 `sign=1`。
2. 若 `x>HALF_PI`，同时设 `x=PI ⊖ x`、`sign=-1`；否则若 `x<-HALF_PI`，
   同时设 `x=(-PI) ⊖ x`、`sign=-1`。比较是严格大于/小于。
3. `x2=x ⊗ x`；`term=1`、`total=1`。
4. 对 k=1..12：`term=((-term) ⊗ x2) ⊘ F((2k-1)(2k))`；
   `total=total ⊕ term`。分母先做整数乘法再转 fp32。
5. 返回 `sign ⊗ total`。

### 2.4 布局、缩放、dtype 转换点

行优先 NCHW，最右 w 连续。`i=(((n*C+c)*h+y)*w+x)`，n 恒为 0。
按 i 从零升序生成 fp32 z；初始 latent 元素是 `init_i=z ⊗ INIT_SCALE`。
**先在 fp32 完成缩放，然后且仅然后按执行型档 dtype 转换**；当前 f32 是恒等转换。
禁止先将 z 转低精度、禁止 torch/numpy RNG 替代、禁止再乘 scheduler 的
`init_noise_sigma`。型档在本版钉死 `init_noise_sigma=1`；这不是 diffusers Euler
的默认管线初始化。SD1.5 的 0.18215 是本规范明确要求的 init 缩放，不能因为某库
通常只在 VAE 边界使用它而删除、移位或重复应用。

## 3. Prompt 编码

- 分词器资产（vocab、merges、special_tokens_map、tokenizer_config 等全部运行时
  读取文件及其配置）由型档文件 SHA-256 清单固定。不得联网自动更新、自动替换为
  同名 tokenizer、由库缺省选择另一 normalization / tokenizer 版本。
- 输入必须是有效 UTF-8。交给型档固定的 CLIP tokenizer 内容分词路径，关闭
  `add_special_tokens`、`padding`、`truncation`；禁止预先截断字符或 token。
  返回的内容 token ID 必须在 0..49405（含端点）。本包接口接收此内容分词回调；
  不下载或另造一份 BPE 词表。真实 tokenizer 的资产固定和载入由型档/客户端包负责。
- SD1.5：序列长 77，BOS=49406 (`<|startoftext|>`)，EOS=49407
  (`<|endoftext|>`)，**pad token ID=49407**。不是 0，也不是文本模型 config
  的其他 pad ID。输出 `[BOS] + content_tokens + [EOS] + [PAD]*剩余长度`。
- 先计完整内容 token 数 K。K+2>77 就拒绝整请求；没有任何截断模式。
  K=75 恰满 77 可接受；K=76 必须拒绝；K=0 输出 BOS 后跟 76 个 EOS/PAD。
- 正 prompt 与负 prompt 必须调用同一函数、同一 tokenizer 和配置。空 negative
  是 `""`，照常分词、加 BOS/EOS、pad、跑负向文本编码器；不得省略负向前向或用
  零 embedding 替代。两条路径产生的 token ID 都必须包含在首步锚的上游输入中。
- CLIP 按完整 77 个位置编码，采用 SD1.5 CLIP 原生 causal attention mask；
  不另加 padding attention mask，不做 clip-skip，不取 pooled embedding 替代
  最后一层逐 token hidden state。正负各前向一次，不共用空 prompt 特例。

## 4. Latent 规范序列化与哈希

**复用已有审计 `ETV1` / `raw-le-v1`**（`node/auditor/spot_binding.py`），不是新造
另一种容器。整个哈希输入为 header 紧接 payload，无分隔符、无 JSON、无长度前缀。

| 偏移（字节） | 长度 | 内容 |
|---|---|---|
| 0 | 4 | ASCII `ETV1`，hex `45545631` |
| 4 | 1 | dtype tag，f32=1（已有审计 f16=4、bf16=3；本版只准 f32） |
| 5 | 1 | rank=4 |
| 6 | 32 | 四个 uint64 **大端**维度，顺序 1,C,h,w |
| 38 | 4*C*h*w | NCHW 行优先 fp32 原始位模式，每元素 4 字节**小端** |

维度都是正整数，batch 必须为 1；长度必须精确匹配，不接受尾随数据。规范 init
必须是允许的三档 shape；通用序列化可处理其他 batch=1 正维度以支持参考测试。
拒绝 NaN/Inf；保留 ±0 的位模式，不做数字文本化或归一化。后续轨迹、审计入出证据
与 init 必须使用相同序列化。裸 payload 可以作为证据文件单独存放，但承诺哈希
始终是 `sha256(header || payload)`；审计 tensor_hash 会重建 header 后读取裸文件。

`latent_hash = SHA-256(规范序列化字节)`；文档和 fixture 的 `sha256` 是该值。
`raw_sha256` 仅用于诊断，不得误作承诺。张量描述符沿用
`f32|1,4,h,w|raw-le-v1`；shape 不省 batch。

## 5. SD1.5 scheduler 静态配置与整数序列

以下配置对 DDIM 与 Euler 共同生效，覆盖任何库默认：

| 字段 | 冻结值 |
|---|---|
| num_train_timesteps | 1000，下标 0..999 |
| beta_schedule | `linear`，直接对 β 线性插值；不是 `scaled_linear`、不是 cosine |
| beta_start / beta_end | fp32 位模式 `3a5ed289` / `3c449ba6`（F(0.00085) / F(0.012)） |
| trained_betas | 无外部数组覆盖 |
| prediction_type | `epsilon` |
| rescale_betas_zero_snr | false |
| steps_offset | 0 |
| timestep_spacing | 本节整数端点规则，不调用 linspace/round 的库默认 |
| clip_sample / thresholding / guidance_rescale | false / false / 0 |
| DDIM eta / set_alpha_to_one / use_clipped_model_output | 0 / true / false |
| Euler interpolation / churn / noise injection | 无插值 / 0 / 无 |
| init_noise_sigma | 1；init 已由 §2.4 构造 |
| model dtype / scheduler state dtype | f32 / f32 |

训练 β 与累积 α 也按 §1 的操作边界产生，b0=F(0.00085)、b1=F(0.012)：
对整数 j=0..999，`β_j=b0 ⊕ ((b1 ⊖ b0) ⊗ (F(j) ⊘ F(999)))`；
`α_j=1 ⊖ β_j`；`A_0=α_0`；j=1..999 依次 `A_j=A_{j-1} ⊗ α_j`。
不得先用 float64 算完整数组再一次转 f32，或将累积乘法换成 log/exp。
这里的 linear β 是设计稿要求，不沿用某 SD1.5 仓库可能附带的 scaled_linear 配置。

请求步数 N 必须是整数 1..100。推理步号 i=0..N-1：

- N=1 时 `t_0=999`。
- N≥2 时 `t_i=((N-1-i)*999) div (N-1)`，`div` 是非负整数向下整除。
- 例：N=2 → `[999,0]`；N=3 → `[999,499,0]`；N=20 →
  `[999,946,893,841,788,736,683,630,578,525,473,420,368,315,262,210,157,105,52,0]`。
- DDIM 直接使用当前 `A_{t_i}` 和下一步 `A_{t_{i+1}}`，末步的下一 A=1。
  不能用某库的固定 step_ratio 推导一个并不在列表里的前一步。
- Euler 的 `σ_i=sqrt32((1 ⊖ A_{t_i}) ⊘ A_{t_i})`，末尾追加 `σ_N=+0`。
  不用 log-linear/karras/exponential/beta σ、不做 σ 插值、不从 σ 反推 timestep。
  UNet 收到的 timestep 是精确整数 t_i（可无损转换 fp32）。

## 6. CFG、DDIM 与 Euler 逐步更新

每步依次执行 uncond 和 cond **两次独立 UNet 前向**，不拼接成批前向；使用相同
当前 model_input、t_i 和对应 prompt embedding。记输出为 ε_u、ε_c。
请求 guidance 按既有定点口径存储整数 G，1000≤G≤30000，
`g=F(G) ⊘ F(1000)`；逐元素严格 `ε=ε_u ⊕ (g ⊗ (ε_c ⊖ ε_u))`。
即使 g=1 也执行相同路径，不跳过负向前向，不重写成别的代数式，不做 CFG rescale。

### 6.1 DDIM，eta=0

当前输入 x_i 就是 model_input（不额外缩放）。设 `a=A_{t_i}`、
`b=A_{t_{i+1}}`，末步 b=1；所有下列操作对元素独立，标量共享：

1. `sa=sqrt32(a)`；`sna=sqrt32(1 ⊖ a)`；
   `pred_x0=(x_i ⊖ (sna ⊗ ε)) ⊘ sa`。
2. `sb=sqrt32(b)`；`snb=sqrt32(1 ⊖ b)`。
3. `x_{i+1}=(sb ⊗ pred_x0) ⊕ (snb ⊗ ε)`。

等价实数公式：`x̂_0=(x_i−√(1−a)ε)/√a`；
`x_{i+1}=√b x̂_0+√(1−b)ε`。不裁剪 x̂_0、不动态阈值、不添加方差项、
不消费额外随机数。末步仍按三条指令执行，不用另一个舍入路径特判返回 x̂_0。

### 6.2 Euler，确定性 ODE

记 s=σ_i>0、s_next=σ_{i+1}。先算
`denom=sqrt32((s ⊗ s) ⊕ 1)`；每元素 `model_input=x_i ⊘ denom`。
用该 model_input 做两次 UNet、CFG，随后用**未归一化的 x_i**更新：

1. `pred_x0=x_i ⊖ (s ⊗ ε)`。
2. `d=(x_i ⊖ pred_x0) ⊘ s`。
3. `dt=s_next ⊖ s`；`x_{i+1}=x_i ⊕ (d ⊗ dt)`。

等价实数公式：`x̂_0=x_i−sε`、`d=(x_i−x̂_0)/s`、
`x_{i+1}=x_i+d(s_next−s)`。不能因为实数代数上 d=ε 就省去第 1、2 步；
fp32 舍入不同。不执行 ancestral 分解，不使用历史状态，不用 s_hat/churn。
初始化之后不再乘 σ_max；本节更新严格接收 §2.4 的 init。

两采样器都保存 `x_0=init` 及每步更新后的 `x_1..x_N` 共 N+1 个 latent 快照。
本包不定义叶结构或实现链状态机。
VAE 解码、像素锚和数值标定不在本包冻结范围；不得把 init 的精确一致要求扩展为
UNet/VAE 跨硬件输出必须逐比特一致，后者按发布标定带审计。

DDIM eta>0、Euler ancestral、DPM++ SDE 及其他随机注入类均排除：它们每步
消费随机数，当前实验未验证其设备无关逐步随机流。在独立 RNG 规范和发散标定完成
前不得进入可审计型档。DPM-Solver++(2M) 虽确定性，因历史项复算未纳入也不收。

## 7. 一致性向量（首算冻结）

权威 fixture：`testdata/vectors/t2i-v1.json`，每行含 seed、完整 shape、
`raw_sha256`、`sha256`、前 8 个元素的 fp32 位模式。以下是**含 ETV1 标头**的
init latent SHA-256。首算工具 `testdata/vectors/generate_t2i.py` 使用 Python
参考实现；Go 独立生成相同全部字节并验证，不使用 Python latent 作为 Go 的输入。

| seed | shape（省略恒定 batch=1 的显示） | sha256（规范序列化） |
|---|---|---|
| 0 | 4×64×64 | `e98e2a594e16b852712167b09b3cf46894e2def67360df61a0456f2ab9d12b3c` |
| 0 | 4×64×96 | `b1202b8e92d0c7b002724daccaf64f9075422b899930b63d9e475c6b3517f756` |
| 0 | 4×96×64 | `05ccd64d2f0ed2ebf0902ce2dc92b03f509c5c524885ee81afae5215b572b0d1` |
| 1 | 4×64×64 | `d06a29c9f027a214634ac91cd3d1606df357125d3ba8aafec43c65ef794f8725` |
| 1 | 4×64×96 | `9a9d73dbf8c1ac4718b052b2d8943571bf1ab833be3d321a47bcdc2a2c1bf2b4` |
| 1 | 4×96×64 | `f5ef51cfc4b16c0c049483f3d5c63c8cd72925c1e389d36b4c59b456a580c759` |
| 42 | 4×64×64 | `c0335c8fdcd134b8b392a485e30488ab3bde90a0d539d24a6f6a1f90aa93ef37` |
| 42 | 4×64×96 | `0548d34296507f4297e397378584dcc2240ceaf56bb17855438b234d8c583b72` |
| 42 | 4×96×64 | `916014aac06af0cca809296f8127bf2bfc1acf7b707aa26143d79fc07c692973` |
| 9223372036854775808 | 4×64×64 | `5768625a26a487a632d7968665887603b22758e63688a16ac79ba6f7be88dcb3` |
| 9223372036854775808 | 4×64×96 | `5f75b6d41a2cda60d7dc396a4d138cd1aa927a5d88c0b7e2405d580ddeef74a2` |
| 9223372036854775808 | 4×96×64 | `f0438a99ff1aab279592609c10e4b7a933a14f8755b4f112b93ec860bdccb261` |
| 18446744073709551615 | 4×64×64 | `53aa7546ed613bb49087cf24394e3966c2eb342cb1907b326df44cde81654a15` |
| 18446744073709551615 | 4×64×96 | `70f48c2dc6a998ec182eabd794aa109cdfe1ac2a8fb9c4826654fee3ad019db5` |
| 18446744073709551615 | 4×96×64 | `ea505965e259306331a4f5d47da4ced226497dd25dd01140f424ce870b9bbca4` |

相同元素总数的两种非方形 shape 拥有相同的 raw_sha256（RNG 只取决于线性
下标），但规范 sha256 不同（标头固定完整 shape）。这也是 shape 锚的判别性测试。

## 8. 参考实现与验收

- Go：`core-go/t2i.go`，接口 `T2IPhilox`、`T2IInitLatent`、
  `T2ISerializeLatent`、`T2IEncodePrompt(s)`。
- Python：`core-py/ember_core/t2i.py`，对应接口 `philox`、`init_latent`、
  `serialize_latent`、`encode_prompt(s)`；无 torch/numpy/tokenizer 依赖。
- 测试：`core-go/t2i_test.go`、`core-py/test_t2i.py`。Go 测试读取固定 fixture，
  并运行 Python helper 直接比较 15 个完整规范序列化张量；Python 测试额外用已有
  审计 `tensor_header` / `tensor_hash` 独立验证证据格式。两个套件都覆盖空 negative、
  K=75 恰满、K=76 超长拒以及无效输入。
- 运行 Go 跨语言测试可设 `EMBER_AUDIT_TEST_PYTHON` 指向 Python 解释器；RNG helper
  只需标准库。仓库其他跨语言测试还要求该解释器装有既有测试依赖。
- 首发仅 f32。本包不引入网络请求、模型下载、生产服务调用、链交易、发布或激活。
