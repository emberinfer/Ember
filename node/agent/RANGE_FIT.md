# 动态范围注册前 fit 自检（FLR-6）

动态范围注册入口是 `shard_rpc.register_stage`（生成
`shard_dynamic_range=true` 的 ResourceProfile），不是通用 `agent.join`。
`shard_rpc serve` 在加载引擎前检查，注册入口也检查；旧声明段不走此检查。
先完成既有清单签名/哈希及 artifact 校验，再按同一模型 config 和清单内
safetensors 的张量字节数估算。分片 index 提供 tensor→file 映射，实际字节数
从每个文件 header 的 data_offsets 取得；兼容无 index 的单文件权重。

演练启动命令原有参数后追加：

```sh
--range-start 0 --range-end 14 --fit-memory-bytes 17179869184 --fit-reference-sequence 2048
```

也可设置 `EMBER_RANGE_MEMORY_BYTES=17179869184`，旗标优先。
动态范围未提供正预算时拒绝启动/注册。预算是运营者为该进程分配的本机
内存或显存额度，须扣除其他进程和系统占用，不能直接填机器总容量。

独立检查（不加载权重、不注册、不接链）：

```sh
PYTHONPATH=node:core-py:proto /root/data/ember-gpu-venv/bin/python \
  -m agent.range_fit --manifest /path/to/manifest.pb \
  --model-path /root/models/Qwen2.5-1.5B-Instruct \
  --range-start 0 --range-end 14 --fit-memory-bytes 17179869184
```

成功输出 JSON 字节明细；超预算退出 1，报告 required/available。
独立检查验证 artifact 内容，档案签名与链锚仍由实际 serve 路径验证。

估算公式见 `range_fit.py`：逐层真实存储字节 + 范围端点的 embedding/head，
权重乘 2 留源数据/加载副本余量；batch=1、BF16、默认参考序列长 2048，
对 hidden/MLP、稠密 attention、逐层 KV、FP32 logits 总和留 4 倍余量，
另留 512 MiB runtime/allocator。尾段 head 按当前 Qwen2ShardEngine 的
独立投影计算（来自 tied embedding），即使范围也持有 embedding 仍单独计费。
此为参考工作负载的保守准入估算，不是硬件实测峰值；更长序列或更大 batch
必须重算预算，演练仍须核对实际峰值。既有运行时内存检查继续生效。

真实目录回归可用 `EMBER_TEST_1_5B_PATH` 覆盖默认模型位置：

```sh
PYTHONPATH=node:core-py:proto /root/data/ember-gpu-venv/bin/python \
  -m pytest -q node/test_range_fit.py
```
