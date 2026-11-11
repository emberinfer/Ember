# Experimental CUDA execution and numerical audits

## Second model: SmolLM2

`ember-smollm2-360m-gpu-v1` runs ungated
`HuggingFaceTB/SmolLM2-360M-Instruct`, revision
`a10cc1512eabd3dde888204e902eca88bddb4951`. It uses the Llama architecture,
32 decoder layers and the SmolLM tokenizer (49,152 entries), rather than the
Qwen model/tokenizer family. The signed execution version is
`transformers-smollm2-gpu-v1`. The manifest commits original weights, model
configuration and tokenizer artifacts, including special-token mappings. The
gateway applies this release's own chat template. Publishing uses the
permissionless bonded-publishing transaction, with a publisher bond and an
explicit endorsement.

All nine directed GB10/Thor/Orin CUDA BF16 bands are enforced. The comparator,
top-16 logits and sampled layers 0/14/27 retain their existing meaning. Thresholds
were frozen at 0.001 for the same class and 0.5 across classes before capture.
Each class produced 128 development and 128 fresh honest three-token requests;
all **768 distinct honest requests** passed replay on all three reference
classes. The largest cross-class honest distance was **0.164203**.

The **960 distinct cheating captures** comprise smaller-model substitution,
four-bit weight substitution, truncated execution, fabricated logits and
wrong-prompt replay: 128 per category on GB10 and 32 per category on each other
class. All were rejected against all three reference classes. The minimum
cheating distance was **0.966576**, above every frozen band. The smaller model
was the real SmolLM2-135M; its activation vectors were padded to the advertised
width, modeling an adversary attempting to disguise the incompatible shape.
Quantized weights used a real symmetric four-bit grid with BF16 kernels.

The signed release and aggregate counts, distance ranges and capture hashes
are in `chain-genesis/ember-smollm2-360m-gpu-v1/`. The serving acceptance uses
physical GB10 and Orin executors, signed CUDA replay and audited chain
settlement, including a fresh consumer identity submitting through a public TLS
gateway from an external host. These measurements cover short deterministic
requests and one operator's physical fleet. They do not establish population
false-positive bounds, long-context calibration or independent organizational
custody. Shared GB10 load makes these runs unsuitable as controlled benchmarks.

`ember-qwen2.5-1.5b-gpu-v3` is the current experimental GPU profile. It covers
measured GB10, Thor and Orin runtimes with all nine directed class pairings
enforced. The corpus covers short deterministic three-token requests; larger
workloads need additional calibration. Earlier immutable versions remain valid
for their narrower measured scope.

The execution version `transformers-qwen2.5-gpu-v1` requires explicit
`device=cuda`, BF16 Qwen2.5-1.5B-Instruct, raw top-16 logits, layer openings
0/14/27, and forced-prefix replay. CPU fallback cannot register as this profile.
Model and tokenizer artifact hashes are inherited from the existing model
manifest; changing the chain ID requires a new signed manifest binding.

Runtime fingerprints include Transformers version, full torch build version,
CUDA toolkit version, and GPU class, for example:

```
transformers/5.20.2|torch=2.14.0+cu130|cuda=13.0|gpu=gb10
```

Recognized class identifiers are `gb10`, `thor` and `orin`.
Recognition alone does not authorize enforcement. Every directed pairing
requires an exact signed tolerance band and calibration digest. Unmeasured
classes/versions and missing bands remain inconclusive. A cross-class band may
be enforced only after measuring honest and cheating corpora for that pairing;
the protocol permits explicit signed bands without inheriting another class's
tolerance. Existing CPU/union profiles retain their previous validation rules.

## Comparator

`rank-probability-v1` uses the maximum over all audited positions of:

- `(1 - Spearman correlation) / 2 / 0.4` over the top-k union, using average
  ranks for tied scores and a tied last rank for absent candidates;
- total variation between each side's softmax conditional on its reported
  top-k set, divided by 0.5;
- the existing censored logit-union distance, divided by 0.15;
- relative L2 of each required layer opening.

The component scales are constants of the metric version, chosen using a
development corpus; they are not unsigned per-job overrides. The last two
checks constrain changes that preserve rank or conditional
probabilities, including additive logit shifts and invented layer evidence.
Token order in a supplied top-k array does not change the comparison. Missing
logits remain censored for the value statistic. Conditional top-k probabilities
are not full-vocabulary probabilities, and this TV is not full-vocabulary TV.
The generated greedy token must already match the maximum of the committed
logits. Output agreement alone cannot prove correct model execution.

Request acceptance uses a signed tolerance on the maximum, after validating
the signed receipt and its committed openings. Numerical rejection still
requires two matching-runtime local reference executions. This is empirical
screening of the committed evidence, not a proof that every layer executed or
that the runtime/hardware self-report is remotely attested. Correct cached
results for the same request cannot be distinguished from fresh computation
by a numerical comparator.

## Auditor routing

`python -m auditor.router` reads operator configuration from
`EMBER_GPU_AUDIT_ROUTES`. It contains candidate `node_id`, `engine_id`,
`model_path`, and a command argv list that invokes the audit worker locally
or through an operator-controlled transport. The coordinator's
`--auditor-python` may point to a launcher for this module. Keep machine
coordinates, credentials and deployment launchers outside the public tree.

The router excludes the executor identity, prefers the same runtime/class,
and selects only explicit enforced bands. It returns inconclusive when no
eligible candidate exists. The worker measures its runtime independently and
selects the signed band again; routing configuration cannot override policy.
Operators remain responsible for independent auditor placement: distinct
node identities on one GPU are not distinct physical GPUs or trust domains.

## Published profile and calibration limits

`chain-genesis/ember-qwen2.5-1.5b-gpu-v1/` publishes the signed genesis,
manifest, policy and aggregate calibration record. The only enforced band is
GB10 to GB10 with the exact runtime above, at request tolerance 0.001.
Two independent process captures passed all 128 short requests. All 512
tested cheats were rejected: 128 each using quantized weights, 16 of 28
layers, fabricated evidence, or another prompt's evidence. The smallest
cheat distance was 1.180781, compared with honest maximum below 3e-16.
This does not establish statistical guarantees or physically independent
same-class agreement.

The comparator was also evaluated against a CPU reference, using the same
weights but a different torch/Transformers runtime. These diagnostic results
do not authorize a CPU or cross-class GPU band:

| Comparator | Threshold | Development honest / 128 | Fresh honest / 64 | Cheats rejected / 128 |
| --- | ---: | ---: | ---: | ---: |
| Raw union/layer baseline | 0.939644 | 128 | 64 | 98 |
| Top-k Spearman distance | 0.356820 | 128 | 64 | 127 |
| Conditional top-k softmax TV | 0.309611 | 125 | 63 | 125 |
| Exact generated sequence | 0 | 108 | 60 | 123 |
| Rank/probability/value combination | 0.99 | 128 | 62 | 128 |

The development corpus was used to select component scales; fresh prompts
were captured after that selection. The combination's 96.875% fresh honest
pass rate falls below 99%. Raising its threshold to 1.879296 to pass all
fresh requests rejects only 111/128 cheats. Rank alone lets a quantized
substitute pass at distance 0.323634. Output agreement also lets quantized
substitutes pass. Those overlaps are measured limits, not evidence that no
better comparator can exist.

The initial published profile has no measured Thor/Orin band. No
cross-class GPU band was published in v1. Later immutable versions below add
measured pairings. Follow-ups include
larger independent honest and cheat corpora, a separately calibrated
vLLM+CUDA profile, and an external-operator Colab T4 GPU-profile retest.


Orin memory admission uses two physical-device BF16 Qwen2.5-1.5B pilots:
peak CUDA allocation 3.14 GB, CUDA reservation 3.37 GB, peak RSS below 3.50 GB,
and minimum remaining system memory 6.08 GB. The measured short-request budget
requires 7 GiB available before loading, caps the CUDA allocator at 4 GiB and
RSS at 6 GiB, and fails the worker if the 3 GiB system reserve is exhausted.
Other GPU classes retain the 16 GiB admission guard and 8 GiB caps. This budget
does not authorize an audit pairing; signed calibration bands govern that.


`ember-qwen2.5-1.5b-gpu-v2` adds measured Orin enforcement while preserving v1.
The Orin runtime is Transformers 4.45.2, NVIDIA torch
2.5.0a0+872d972e41.nv24.08, CUDA 12.6; the GB10 runtime remains unchanged.
The signed policy permits both GB10↔Orin directions at tolerance 0.5 and
Orin↔Orin at 0.001, using the existing `rank-probability-v1` metric. Runtime
fingerprints, device admission and selected-reference checks remain mandatory.

| Producer → reference | Threshold | Development honest | Fresh honest | Cheats rejected | Minimum cheat | Margin |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| GB10 → Orin | 0.5 | 128/128 | 128/128 | 512/512 | 1.205419 | 0.705419 |
| Orin → GB10 | 0.5 | 128/128 | 128/128 | 128/128 | 1.193809 | 0.693809 |
| Orin → independent Orin | 0.001 | 128/128 | 128/128 | 128/128 | 1.219903 | 1.218903 |

The GB10→Orin result was independently repeated on a second physical Orin
with identical scores. There are 512 distinct GB10-produced cheat items and
128 distinct Orin-produced cheat items, including real quantized weights,
truncated computation, fabricated evidence and wrong-prompt replay. References
replay each producer's committed prefix. Thresholds were frozen from the
development data before scoring fresh prompts. No tested cheat passes the
selected bands. Short three-token requests do not establish a population
false-positive bound or validate larger workloads.

On the same GB10→Orin corpus, other candidates show why tolerance selection
must include both fresh honest execution and cheats. Scalar thresholds below
are fitted on even development IDs; the combination uses its frozen threshold.

| Comparator | Threshold | Development honest /128 | Fresh honest /128 | Cheats rejected /512 |
| --- | ---: | ---: | ---: | ---: |
| Raw union/layer baseline | 0.041258 | 127 | 125 | 512 |
| Top-k Spearman distance | 0.025584 | 128 | 126 | 512 |
| Conditional top-k softmax TV | 0.059349 | 120 | 121 | 509 |
| Exact generated sequence | 0 | 125 | 123 | 489 |
| Rank/probability/value combination | 0.5 | 128 | 128 | 512 |

Aggregate candidate ROC tables, margins and capture digests are published in
`chain-genesis/ember-qwen2.5-1.5b-gpu-v2/calibration.json`. Thor
and CPU pairings remain outside this profile's enforced bands. Those v2 results
left Thor outside the profile; v3 below adds the measured Thor pairings.


The v2 profile completed a serving/settlement acceptance run on two physical
GPU classes: GB10 and Orin. Each executor served three gateway requests; a
separate GB10 auditor identity replayed on CUDA, using the signed cross-class
band for Orin execution. All six requests settled on the isolated
chain without refunds. Consumer balance fell by 18 subunits, each executor
earned 9, stakes stayed unchanged, and balance plus stake was conserved.
Settlement times were 25.972, 31.126, 24.602, 23.603, 26.800 and 23.313 seconds
(median 25.287), compared with the known approximate 53-second CPU baseline.
These timings are not a controlled matched benchmark. This acceptance closes
the physical-host serving gap; v3 below closes the remaining class-coverage gap.


## Three-class calibration in v3

The signed v3 policy retains the measured v2 GB10/Orin bands and adds both
GB10↔Thor and Orin↔Thor directions at tolerance **0.5**, plus Thor repeats at
**0.001**. Thor uses Transformers 4.45.2, NVIDIA torch 2.11.0 and CUDA 13.0.
The cross-class threshold and component scales were frozen before scoring Thor
numeric data. Each reference replays the producer’s committed token prefix.

| Producer → reference | Threshold | Development honest | Fresh honest | Cheats rejected | Minimum cheat | Margin |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| GB10 → Thor | 0.5 | 128/128 | 128/128 | 512/512 | 1.174263 | 0.674263 |
| Orin → Thor | 0.5 | 128/128 | 128/128 | 128/128 | 1.187108 | 0.687108 |
| Thor → GB10 | 0.5 | 128/128 | 128/128 | 128/128 | 1.204628 | 0.704628 |
| Thor → Orin | 0.5 | 128/128 | 128/128 | 128/128 | 1.230122 | 0.730122 |
| Thor → repeat Thor | 0.001 | 128/128 | 128/128 | 128/128 | 1.198127 | 1.197127 |

All required GPU-class pairings meet the observed ≥99% honest acceptance and
full tested-cheat rejection requirements. GB10↔Thor has the largest fresh
honest distance, **0.440781**, below the frozen 0.5 threshold. The smallest new
cheat margin is **0.674263**. No tested cheat passes the selected bands.

The complete corpus has **768 distinct cheat items**: 512 produced on GB10,
128 on Orin and 128 on Thor. Rechecking an item on several reference devices
does not make it a distinct cheating example. Honest repeats cover each class;
Orin includes two independent physical devices. Aggregate candidate ROC tables,
per-category distances and capture digests are in
`chain-genesis/ember-qwen2.5-1.5b-gpu-v3/calibration.json`.

CPU diagnostic enforcement remains unsupported. The measured corpus does not
establish a population
false-positive bound or validate long generation. Larger independent corpora,
a vLLM+CUDA profile and the external Colab T4 retest remain follow-ups.


The final v3 profile also completed the physical GB10/Orin serving acceptance:
three gateway requests per executor, CUDA replay by the selected GB10 auditor,
and six signed accept verdicts settled on the independently registered v3
profile. Orin execution used the signed Orin→GB10 band. Balances moved by −18
consumer subunits and +9 per executor, stakes stayed unchanged, all holds were
released and no refunds occurred. Settlement times were 24.913, 32.542, 23.901,
23.295, 26.004 and 24.300 seconds (median 24.606), versus the known approximate
53-second CPU baseline; this remains an uncontrolled comparison. The completed
campaign establishes the requested GPU calibration, profile registration and
physical-host serving/settlement gates within the stated experimental scope.
