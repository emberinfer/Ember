# T2G — NO-GO：冻结数值规范错误且与链上初始承诺耦合

2026-11-23 UTC；684 CPU，torch 2.5.1+cu124（CUDA 隐藏），diffusers 0.32.2。独立目录 `/root/autodl-tmp/t2i/work2`，由本地 shared clone 建立独立 Git 索引和分支，再从真正 origin fetch 核验 `train/2026-11-23-t2i` 的 HEAD 为 `5e37ebfe51cccaf096353eec582932b90b2d070d`。未修改 `/root/autodl-tmp/t2i/work`；未重置它的分支。

## 结论先行／NO-GO

适配器忠实实现了错误的冻结规范，并非仅有适配器某个算子的实现错误。规范 §2.4 将 Gaussian 初始 latent 乘 VAE 缩放常数 0.18215；§5 另强制 linear beta、整数 floor 端点时间步、offset 0、DDIM 最终 alpha=1，并禁用 Euler 初始 sigma 缩放。这些均与固定 SD1.5 资产的参考 diffusers 配置不一致。相同 init 的参考 DDIM 自身也退化，不能把 scheduler 对齐宣称为已解决画质。

**停车触发器：规范层与链耦合。** `core-go/t2i.go:T2IInitLatent` 将每个 Gaussian 乘位模式 0x3e3a8588（0.18215）；`services/chain/image.go:submitImage` 由该函数重建初始 tensor 哈希，强制第 0 叶的 input/output 均匹配，并强制后续邻接连续。`core-go/image_profile.go:ValidateImageProfile` 只允许 `diffusers-sd1.5-fp32-v1`。直接改初始 tensor 为标准正态或乘 Euler sigma 会破坏当前链端初始锚；静默复用该型档标识则改变已冻结数值语义。依任务书停止修复及后续验收，保留诊断材料，等待督导裁定。

这里不宣称修改链是唯一技术路径：保留 raw sentinel、在第 1 transition 中预缩放也可能实现，但需要明确生成、单步审计、开证边界、型档版本、规范及旧标定材料的迁移语义，不能自行选择并伪装成普通适配器 bugfix。

## 严格按序的实测

1. `/root/workspace/ember-supervisor/t2i-divergence-test-2026-11-22/runs/` 本机不存在，按任务书跳过旧工件查阅。没有以旧自对拍替代参考真值。
2. 先运行真正 `StableDiffusionPipeline.from_pretrained`，将资产的 scheduler 配置交给 `DDIMScheduler.from_config`；fp32、CPU、prompt 1、空负面、20步、guidance 7.5、512×512。通过 `latents=` 注入适配器 seed42 Philox init；标准参考 scheduler、pipeline 编码/CFG/UNet/解码路径，未用 Formula 实现替代参考 scheduler。reference/000.npy 为输入，callback_on_step_end 保存 001..020；终图 final.png。
3. 参考完成后才加载原样适配器，同配置保存 adapter/000..020.npy 和 final.png。每个 Python 进程 torch 4线程、OMP_NUM_THREADS=4、nice15，CUDA_VISIBLE_DEVICES 为空。未运行 GPU、未接触 27B。
4. 对拍与中间量见 `comparison.json`、`first-step.json` 及相应 .npy；diffusers scheduler 源码逐方法落盘，配置见 `reference-schedulers.json`。诊断脚本 `diagnose.py`、`first_step.py` 均在 `/root/autodl-tmp/t2i/t2g-logs/`。

## 首个不一致与公式核对

- DDIM init 两端逐元素相同；首个完成 transition 的偏离为 step1，相对 L2=0.054130137337861234（分母参考范数，float64 归约），已不是舍入噪声。
- 首个 UNet 调用前已经不同：参考 leading 时间步为 `[951,901,...,51,1]`（1000//20=50，steps_offset=1）；适配器为 `[999,946,893,841,...,52,0]`（floor 端点）。因此首步 eps 预测不同不能归因于同输入跨设备误差。
- beta 的第一个元素同为0.00085，第二元素起不同。参考 `linspace(sqrt(beta_start),sqrt(beta_end),1000,float32)**2`（scaled_linear），适配器直接线性插值 beta；累积 alpha 因而系统偏离，影响每一步 x0 与更新。
- DDIM 参考 alpha_prev 由 `t - 1000//steps` 索引；末端 set_alpha_to_one=false，使用 alpha[0]，适配器末端使用1。eta=0，不注入逐步噪声。
- Euler 参考时间步为浮点 linspace，如946.4210815、893.8421021；sigma 按训练 sigma 数组线性插值，首 sigma=14.61464691，末端追加0。pipeline.prepare_latents 对传入的 latents 也乘 init_noise_sigma；适配器不乘，sigma 也来自错误 linear beta 和整数时间步。两者不能直接视为同一 ODE 网格。
- 两端 CFG 语义都是 `eps_u + 7.5*(eps_c-eps_u)`，prediction_type 都是 epsilon；没有证据表明 CFG 合成方向或 v_prediction 分支是首因。参考批量 CFG，适配器分别调用 UNet；可能的批形状舍入差异不能解释不同时间步/alpha 引起的大幅偏离。
- init 标准差0.18363014、均值0.0005467611；应区分“为隔离 scheduler 注入相同 init”与“验证正确生成初始化”。0.18215 是 VAE latent 解码除数，不能直接当成 Gaussian 标准差。参考注入低幅 init 的终图也呈棕褐色低细节，RGB均值[120.86707306,80.48270798,41.04592133]、RGB标准差[3.17717554,2.65671929,1.88625285]，不含可辨认的宇航员/马。

## 首步实测中间量与终态

| 项目 | 参考 DDIM | 原适配器 |
|---|---:|---:|
| t | 951 | 999 |
| alpha_bar(t) | 0.008154998533427715 | 0.0015789621975272894 |
| alpha_bar(prev) | 0.014004888944327831 | 0.0029477758798748255 |
| sqrt((1-alpha)/alpha) | 11.0283352783 | 25.1461206803 |
| eps_uncond norm | 31.45288849 | 31.50308609 |
| eps_cond norm | 31.73301125 | 31.71580505 |
| CFG eps norm | 33.91660690 | 33.61856461 |
| x0 norm | 128.46350098 | 301.28839111 |

逐元素相对 L2：eps_uncond=0.01503079，eps_cond=0.01922357，CFG eps=0.07364782，x0=1.38257332。参考独立重建的 step1 与 pipeline 已保存 step1 距离为0；完整轨迹 step0=0，step1=0.054130137337861234，step20=2.1988239733011787。所有21个点在 comparison.json，没有只挑选有利步。

## 未执行范围与裁定事项

未修改冻结规范、适配器、链代码、profile、T0向量或标定带；没有宣称修复完成。由于 NO-GO，未添加以未批准语义为基准的回归测试，未运行全量 Python/Go 验收，未生成修复后的6图/CLIP总览，未运行 bjb1 GPU/CPU 标定带或 fp16 抽查，未改 Base=7.7563e-05。当前参考/适配器两张诊断图不能冒充六图画质验收。

需督导裁定：新的执行型档与旧冻结语义的迁移策略；是否改链端初始锚或保留 raw sentinel 并定义首步预缩放；允许修正规范的 init/beta/DDIM/Euler 配置，并确认后续回归与标定重新验收范围。未经裁定不推行上述方案。

## 初始化因果对照（诊断，非修复后验收）

在完成“参考→原适配器→逐步对拍”后，使用同一个参考 DDIM、同样提示词和参数，唯一改变为 `latents = frozen_init / float32(0.18215)`。这保持 Philox 样本与种子，撤销规范强加的 VAE 初始缩放；未修改仓库源码。`reference/unit-noise-control.png` 清晰出现宇航员骑马、火星地面；直接观察与低幅 init 的棕褐色平面对照，支持错误初始化幅度是关键画质根因。此实验同时保留标准参考 scheduler，因此不宣称单改适配器 init 即可完成全部参考对齐，也不宣称该单张控制图满足六图验收。

三张诊断原图、`diagnostic-overview.png` 和逐图像素统计 `pixel-statistics.json` 均在 t2g-logs；总览明确标注原适配器、参考+冻结init、参考+unit-noise control，没有将控制图冒充已修复适配器输出。

| 诊断图 | RGB均值 | RGB标准差 | 像素范围 |
|---|---|---|---|
| adapter/final.png | [128.684258, 40.177773, 0.0] | [5.476801, 1.97759, 0.0] | 0..135 |
| reference/final.png | [120.867073, 80.482708, 41.045921] | [3.177176, 2.656719, 1.886253] | 16..127 |
| reference/unit-noise-control.png | [152.611343, 102.979519, 68.594219] | [48.725408, 45.926308, 41.619197] | 0..255 |

## 留证与交付

诊断两个进程均退出0，无残留 GPU 工作；`unchanged-source-sha256.json` 记录7个未修改源文件并逐文件与5e37ebf验证。运行证据在 `/root/autodl-tmp/t2i/t2g-logs/`，源码/PNG/latent/配置/比较结果均保留；不包含私钥或访问凭据。仓内只新增本诊断报告 `docs/T2G-NO-GO.md`，一次 commit、一次 push 后由外部报告补记提交号与 ls-remote 核验结果。
