# Whole-model equality audit execution

RC registered auditors default to teacher-forced scoring for whole-model
`token-equality-v1` audits. `EMBER_AUDIT_TEACHER_FORCING=1` explicitly enables
this path; `EMBER_AUDIT_TEACHER_FORCING=0` selects regeneration. An adapter
raising `UnsupportedCapability` automatically falls back to regeneration.
Other engine failures retain their existing error handling.

The worker logs `audit request=<id> path=teacher-forcing` or
`path=regeneration` at INFO level. Configure the node's logging level to retain
these operational records. Both paths produce the same signed challenge and
verdict schema; the execution-path label is not a consensus field.

Teacher forcing scores the committed prefix, then applies the shared
first-divergence verdict rule. Epsilon remains 1.0. Sampling remains
30%/5%/2%: selection and SamplingRecord recomputation happen independently of
the selected request's audit execution. Pipeline/range evidence continues
through range replay before the whole-model equality dispatch.

Transformers and mock adapters implement scoring. The 35B HTTP/vLLM
`score_forced` adapter is not delivered; it continues through regeneration.
Do not infer adapter availability from the upstream HTTP scoring probe.

Upstream reconciliation reported 137/141 matching rows, no honest rejection,
and four stricter TF outcomes from one floating-point epsilon-boundary case.
The estimated network audit cost reduction from +100% to about +7% assumes
adapter coverage, including the still-undelivered 35B adapter. It is not an RC
measurement or a reason to change sampling rates.

## RC validation

The isolated synthetic drill submitted actual Agent receipts and signed
registered-worker verdicts to a single-validator scratch chain. Both the default
TF request and the environment-disabled regeneration request settled with
`accept`, billed 20 tokens, and identical verdict/challenge field structure.
Their persisted SamplingRecords independently recomputed to sampled. A 4,000
subunit hold used the existing amount weighting to ensure sampling without
changing the 30%/5%/2% ladder. This validates execution and settlement mechanics,
not physical-model calibration or the undelivered 35B scoring adapter.

The final RC regression passed all 13 tested Go packages and the Python suite
(177 tests plus 19 subtests); generated genesis pricing remained current.
