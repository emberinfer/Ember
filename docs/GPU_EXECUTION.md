# GPU execution and audit policy

GPU is a node attribute, not a deployment-form name. A signed profile fixes its
evidence, supported runtime pairs and policy. Unknown devices or uncalibrated
pairs do not inherit a tolerance from a similarly named device.

## Current deployment

| Profile | Model | Context tokens | Availability |
| --- | --- | ---: | --- |
| `ember-qwen2.5-1.5b-unified-v1` | Qwen2.5-1.5B-Instruct | 32768 | Serving; unified Whole model and Sharded forms |
| `ember-qwen3.6-35b-a3b-fp8-unified-v1` | Qwen3.6-35B-A3B FP8 | 262144 | Serving via vLLM on GPU nodes |
| `ember-olmoe-1b-7b-moe-v1` | OLMoE-1B-7B | 4096 | Serving; Sharded by layer + expert; sampled segment-replay audits pass |
| `ember-kimi-k3.1-pro-v1` | Kimi K3.1 Pro | Not specified | Published on-chain; not deployed; no serving nodes |


A model is one set of weights; a profile is a deployment form. Forms are **Whole model** and **Sharded** (by layer, by expert, or layer + expert). A unified profile covers whole-model and sharded forms under one card. CPU/GPU describes a node, not a form. The 1.5B `-segmented` listing is the unified profile's sharded entry point, not another profile.

The 35B path uses vLLM on GPU nodes. Its adapter supports default incremental SSE
and teacher-forced scoring through `prompt_logprobs`. This does not supply raw
hidden-layer openings required by other full-evidence profiles. OLMoE uses its
own attention/expert RPCs, segment evidence and replay. See [adapters](ENGINE_ADAPTERS.md),
[audit design](AUDIT_DESIGN.md) and [MoE serialization](moe-serialization.md).

## Legacy numerical policies

The source retains `rank-probability-v1`: the maximum of normalized rank distance,
conditional top-k total variation, censored logit-union distance and layer relative
L2. The rank, TV and union scale divisors are 0.4, 0.5 and 0.15. Conditional top-k
TV is not full-vocabulary TV. Signatures and opening checks precede comparison.
Enforcement requires a supported signed band; runtime labels are not attestation.
See `node/auditor/numeric.py` and `node/auditor/policy.py`.

Historical signed GPU card artifacts and calibration aggregates remain immutable.
They do not establish current registration coverage, live latency, independent
ownership, long-context behavior or population false-rejection rates. Do not
select a retired card merely because its old calibration passed. Use the exact
currently registered unified profile and query effective policy on-chain.

## Resource admission

Provision memory for weights, runtime, KV cache and evidence at the intended
context and concurrency. Signed GPU budget fields are declarations, not physical
hardware enforcement. See [GPU budget declarations](GPU_BUDGET.md),
[contribution limits](CONTRIBUTION_CAPS.md) and [hardware](HARDWARE.md).
