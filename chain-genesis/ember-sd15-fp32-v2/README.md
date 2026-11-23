# SD1.5 fp32 v2

模型型档 ID：ember-stable-diffusion-1.5-v1。执行型档：diffusers-sd1.5-fp32-v2。

T2H 修正标准正态 init、scaled_linear beta、DDIM leading/alpha_prev、Euler 浮点插值与首步 sigma 准备。第0叶是 unit Gaussian sentinel，Euler 缩放在首个 transition 内由生成与审计共用。

本修正提交暂不具备发布资格：range_band 全零是明确无效的待标定占位；禁止沿用 v1 的标定带/精度区。新配对标定完成后独立提交替换，才可发布。固定权重及 arch_manifest 不变。
