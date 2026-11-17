# Dynamic range fit admission

Dynamic range registration enters through `shard_rpc.register_stage`, which
creates a `shard_dynamic_range=true` ResourceProfile. The serve path checks fit
before loading, and registration checks again. Legacy declared fixed stages do
not use this dynamic-range check. Verify manifest signatures, hashes and artifacts
through the serving path before admission.

Supply a positive process memory/VRAM allocation, not total machine capacity:

```sh
--range-start 0 --range-end 14 --fit-memory-bytes 17179869184 --fit-reference-sequence 2048
```

`EMBER_RANGE_MEMORY_BYTES` also supplies a budget; the explicit flag wins. Missing
or nonpositive dynamic-range budgets fail. Subtract other process and system use.

For a standalone estimate, without loading weights, registering or contacting a
chain, run from the repository root with the node dependencies installed:

```sh
PYTHONPATH=node:core-py:proto python -m agent.range_fit \
  --manifest "$MANIFEST" --model-path "$MODEL_PATH" \
  --range-start 0 --range-end 14 --fit-memory-bytes 17179869184
```

The standalone tool verifies artifact contents; signed-chain anchor verification
remains the serving path's responsibility. Success reports JSON byte components;
insufficient budget exits 1 with required/available bytes.

The estimator reads tensor sizes from safetensors header `data_offsets`, using
an index when present and accepting unindexed single-file weights. Only declared
files count; a differing local inventory fails. It sums held layer bytes and
owned embedding/head bytes, doubles weights for loading copies, reserves four
times reference BF16 activation/attention/KV and FP32-logit storage, and adds
512 MiB runtime overhead. Reference batch is one and sequence length is 2048 by
default. The Qwen2 tail's separate tied-weight projection is counted even if the
range also owns the embedding.

This is a conservative reference-workload estimate, not measured peak memory or
a guarantee for longer contexts or larger batches. Recalculate and measure those
workloads; runtime memory guards still apply. `EMBER_TEST_1_5B_PATH` can select
verified local weights for `python -m pytest -q node/test_range_fit.py`.

Source: `node/agent/range_fit.py:46`, `node/agent/range_fit.py:96`,
`node/agent/range_fit.py:141`, `node/shard_rpc.py`.
