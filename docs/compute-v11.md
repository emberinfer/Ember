# V11 shape-derived compute

`core.DeriveCompute` uses only integer and exact rational arithmetic. T is the
sum of logical tensor elements. Classification priority is vision/visual (T
only), embed_tokens/embedding lookup (T+A, excluded from matmul), lm_head or
root output projection (T+A, head), routed `experts.<index>` or expert-major
packed tensors (T, top_k/N of A), then everything else (T+A). Shared experts,
routers, norms, MLA/KDA/DSA and shared input/output projections are fully active.
F = 2*(A-E). Absent separate head means zero additional head parameters,
including tied-weight models. Declared totals do not influence derivation.

Routed contributions accumulate as exact fractions; nonintegral final A or
per-layer FLOPs reject, never round. Indexed experts must cover [0,N) per layer;
packed experts must have leading dimension N and an exactly divisible element
count. Config supplies structural scalars only, including nested text_config.
Layer count, input attention projection dimensions and embedding dimensions are
checked against config. Duplicate names, zero dimensions, unsupported dtypes,
contradictions and uint64 overflow reject.

For fp4/int4 (and *_packed aliases), stored final dimensions contain two logical
elements per byte. Logical numel is twice stored numel; byte count is stored
numel. Rank < 2 is ambiguous and rejected. BF16/FP16 use two bytes, FP32 four,
FP8 one. TensorBytes uses stored shapes; it must fit the sum of anchored .bin
and .safetensors sizes. There is no useful completeness lower bound without
parsing authenticated headers (artifact sizes also include headers/padding).

Nonboundary tensors without layer indices, such as final norm, are attributed
to the last observed decoder layer. Without any layer indices, return totals
only; supplied per-layer declarations then reject. There is no inferred even
split. Layer FLOPs plus head must equal F. EmbeddingFlops in a verified manifest
may be 0 or 1: both yield the existing one-unit lookup settlement floor.
A tied head may be zero under v11; v10 validation is untouched.

## Architecture and sampling config keys

For routed experts per token (TopK), the architecture keys
`num_experts_per_token`, `num_experts_per_tok`, and `num_selected_experts`
are authoritative synonyms. Conflicting values on this same architecture
surface fail closed. If any is present, the HF GenerationConfig sampling
field `top_k` is completely ignored for TopK: it is neither parsed, checked
for conflicts, nor range-checked. Only when all architecture keys are absent
is `top_k` accepted as a backward-compatible fallback. Nested and lookalike
keys such as `dsa_config.index_topk`, `topk_group`, and `topk_method` do not
supply TopK.

The real Kimi fixture retains the nested text config and sampling fields,
including architecture TopK 16 alongside sampling `top_k` 50. Its governance
inventory has 93 layers (layer zero dense) and 931 tensors, with the same
exact T/A/F/E and TensorBytes totals as the synthetic fixture below.

## Trust boundary

The chain recomputes the formula and canonical sorted-name JSON SHA256 from
publisher-signed shapes and checks config hashes, shape consistency, exact
claims and the artifact byte upper bound. ShapesRoot anchors the supplied
inventory, **not a cryptographic proof that it equals the safetensors headers**.
A malicious publisher could omit tensors or fabricate a plausible inventory.
Header-to-artifact inclusion verification remains outside this task's payload.
No network, clock, randomness or floating point enters the new derivation path.

Version >= 11 gates weighted publishing, corrections and settlement overrides.
New state/action fields omit when absent, preserving historical JSON. The
AppVersion constant and accepted LoadState versions remain unchanged. All
layout schemes of a new verified publication must agree with the derived table.
The raw signed artifacts and all identity hashes remain unchanged by correction.

## Correction scheme

Scheme A is `correct_model_compute`, using the existing governor check. The
handler reuses the stored manifest's exact config bytes, checks their anchored
hash, re-derives the supplied shapes and compares all corrected T/A/F claims.
It appends height, governor, old/new F and T, ShapesRoot and optional reason.
Old values use prior VerifiedCompute; without it old F is manifest layer+head
sum (no lookup floor), and unknown old T is recorded as zero. The original
Publication.ShapesRoot, manifest/profile roots, bytes, registrations, stakes
and pricing are retained; corrected shape roots live in VerifiedCompute/history.

Duplicate model IDs are rejected by publish. `identityFor` computes canonical
model identity and publish rejects a second identity hash. Re-publishing under
another profile would strand registrations/stakes and price records anchored
to the old ID/root. Correction retains one model ID and auditable history.

The shared `verifiedComputeManifest` helper switches these pipeline sites:

- `freezePipeline`: reservation RangeCompute and TotalCompute denominator.
- `submitPipeline`: receipt work and initial credit split.
- `pipelineExecutorCredits`: later executor charge redistribution.

MoE settlement reads MoeLayout constants, not LayerFlops, and is unchanged.

## Fixtures and comparisons

Kimi synthetic headers reproduce T=2,780,110,363,392, A=105,542,546,176,
E=1,174,405,120 and F=208,736,282,112 exactly; stored TensorBytes is
1,476,109,481,472. The old 250,580,697,088 table differs by 41,844,414,976.
There are 92 synthetic routed blocks; layer zero enumerates 896 experts, each
with fp4 w1/w3 [3072,1792] and w2 [3584,1536]. Remaining blocks pack the expert
dimension; nonexpert tensors are class aggregates, not the original 96 shard
headers. Vision contributes 447,358,976 to T only. Shape-based expert width and
fully counted MLA/KDA/DSA/shared projections exercise both old error sources.
The routed quotient is integral:
2,722,740,830,208 * 16/896 = 48,620,371,968 exactly. No rounding workaround exists
or is needed; an independent nonintegral fixture rejects.

The checked-in Qwen2.5 manifests have no LayerFlops tables (not zero-cost
models). Qwen3.6 and OLMoE published tables are absent from this repository.
Therefore the permitted fallback comparison below is 2*A from config-derived
weight shapes, **not a claimed observed on-chain total**. Differences are
exactly the excluded 2*E lookup counts.

| Model | Fixture T | Fixture A | Derived F | On-chain table | Fallback 2*A | Derived minus fallback |
|---|---:|---:|---:|---|---:|---:|
| Qwen2.5-1.5B | 1,543,714,304 | 1,543,714,304 | 2,620,681,216 | absent | 3,087,428,608 | -466,747,392 |
| Qwen3.6-35B-A3B | 34,660,610,688 | 3,454,988,928 | 5,892,859,136 | absent | 6,909,977,856 | -1,017,118,720 |
| OLMoE-1B-7B | 6,919,161,856 | 1,282,017,280 | 2,357,989,376 | absent | 2,564,034,560 | -206,045,184 |

Config snapshots originate from Qwen/Qwen2.5-1.5B-Instruct,
Qwen/Qwen3.6-35B-A3B-FP8 and allenai/OLMoE-1B-7B-0924, available as local
public-model copies. Qwen3.6 reports the qwen3_5_moe architecture: 40 blocks,
256 experts, top-8, 512 intermediate, hidden 2048; 30 linear-attention blocks
and 10 full-attention blocks. Its regression inventory is main-text weights
only, omitting optional vision/MTP and quantization scale metadata; do not
interpret its T as a full-checkpoint header recount. Its F is in the requested
6 GFLOP class (107,140,864 below 6 billion); A includes the 508,559,360 lookup
parameters. OLMoE includes Q/K norms and reproduces the exact known T.
Qwen2.5 includes QKV biases and the final norm, with no separate tied head.

Tests assert derived truth rather than legacy tables, exercise correct and
incorrect signed Kimi publications, config/expert/hidden contradictions,
fractions, overflow, packing, byte caps, lookup floors, tied heads, canonical
shape ordering, v10 JSON compatibility, correction authorization/history and
pipeline weights before/after correction. No live-chain action is performed.

The architecture-first TopK parsing above is enabled only at AppVersion 12 or
later. Earlier versions preserve the ordered aliases `num_experts_per_tok`,
`top_k`, and `num_selected_experts`, including conflict rejection; they do not
recognize `num_experts_per_token`. Publication and compute correction use the
state version. Core callers select it with `DerivationInput.AppVersion` or
`ParseConfigScalarsForVersion`; zero and `ParseConfigScalars` retain legacy
semantics. No other scalar parsing changes across this gate.

## Live correction

The live chain is `ember-testnet-1`, with genesis dated 2026-11-14 and five Ledger nodes. Live AppVersion 12 activated at height 116582. The source base constant is 11; the coordinated upgrade ladder advances the running state to 12.

Kimi is published, not deployed. Its current T is 2,780,110,363,392 total
parameters, A is 105,542,546,176 active parameters, and F is 208,736,282,112
FLOPs/token. AppVersion 12 corrected the earlier derivation on-chain (correction
count 0 to 1). Old-value constants in rejection/correction tests are intentional
negative fixtures, not live compute claims. Shape declarations do not prove
header inclusion; retain the trust boundary above.
