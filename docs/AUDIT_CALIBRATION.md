# Numerical audit policy and calibration

Auditor operators and Profile publishers can use this document to interpret
numerical bands and identify the measurements needed before enforcing a new pair.
Implementation: [policy validation](../node/auditor/policy.py),
[comparators](../node/auditor/numeric.py) and [GPU rules](GPU_EXECUTION.md).

## Version boundary and authority

Published genesis/manifest bytes remain immutable. Absence of a numeric policy
selects legacy ranked-ID comparison; it must not silently acquire new tolerances.
A new signed Profile pins its metric, top-16 evidence, layers 0/14/27, calibration
digest and exact producer/reference runtime bands. A calibration digest binds a
record; it is not evidence that the record has been independently reviewed.

The worker verifies signed genesis, producer identity, manifest and Profile
bindings before selecting a band. Runtime selection includes the actual reference
runtime. An unsigned job tolerance cannot override a signed policy. Unknown pairs,
nonfinite or incompatible references and observation-only bands yield inconclusive
results rather than a numerical fraud penalty. A changed runtime on the confirming
reference cannot confirm a numerical rejection. Invalid signed commitments or
objectively invalid required openings are handled separately from numerical drift.

## Sparse top-k union statistic

For a token reported by one side, the score is the singleton interval [z,z]. An
unreported score is only known to be at most that side's kth score. The
`union-interval-v1` comparator takes the L2 norm of the minimum gaps between these
intervals, normalized by the larger observed vector norm with kth-score censoring
and a 1e-12 floor. Shared tokens contribute their score difference; a token present
only on A contributes max(0, z_A - min(B)), and conversely for B.

This is a screening statistic, not full-vocabulary relative L2. Missing logits are
not zero or measured values; ties and tail substitutions can have zero gap. The
worker also compares every required float32 little-endian layer opening using
relative L2 against forced-prefix replay. The union-policy request score is the
maximum across positions, logits and layers. Signed opening verification precedes
numeric comparison. Logprob-only adapters cannot satisfy a raw-logit Profile.

## Bands and interpretation

Tolerance is a signed per-pair parameter, not a universal hardware constant.
Union-policy enforcement requires the same engine build and device, except for
explicitly supported pipeline comparisons. Cross-runtime union bands are
observation-only. The GPU metric has separate fingerprint and enforcement rules
in [GPU execution](GPU_EXECUTION.md), including calibrated cross-class bands.

Historical short-request captures are not distributed in this source tree, so
this document provides no independently reproducible historical acceptance rate
or population false-positive bound. A runtime match alone does not establish
calibration on long contexts, quantization, new kernels or independent hardware.

## Reproduction and publication requirements

The public [numeric regression tests](../node/test_audit_numeric.py) exercise
metric behavior and signed-policy handling. Some tests in that file and the
[Profile builder tests](../chain-genesis/test_audit_profile.py) additionally need
historical artifact sets that are not distributed in this tree. With reference
Python dependencies installed, these fixture-only checks run from the repository
root without those artifacts:

```sh
PYTHONPATH=core-py:proto:node python3 -m unittest \
  test_audit_numeric.NumericAuditTest.test_union_is_id_aligned_and_ties_are_censored \
  test_audit_numeric.NumericAuditTest.test_activations_count_shapes_and_nonfinite_cannot_accept \
  test_gpu_audit test_pipeline_audit test_pipeline_gpu -v
go test ./services/verifier ./services/chain ./services/ledger ./services/auditqueue
```

These tests use fixtures and do not measure model-level evasion or hardware drift.
To publish a new enforced band, supply signed source artifacts, exact runtime facts,
prompt/token corpus, forced-prefix captures, hash inventory, collection and scoring
commands, development/held-out separation, honest and injected-cheat distances, and
memory/context limits. Measure full replay cost and confirmation cost. Report
sample counts and residual failures; do not infer a general error bound from a
small successful corpus. [build_audit_profile.py](../chain-genesis/build_audit_profile.py)
accepts explicit source, output and policy paths; its historical defaults require
artifacts supplied separately and are not a source-only reproduction command.
