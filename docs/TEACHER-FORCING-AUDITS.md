# Teacher-forcing equality audits

Registered workers default to teacher forcing for `token-equality-v1`.
`EMBER_AUDIT_TEACHER_FORCING=0` selects regeneration. `UnsupportedCapability`
falls back to regeneration; other errors retain failure handling. Workers log
`path=teacher-forcing` or `path=regeneration`; these labels are not consensus fields.

Transformers implements `score_forced`; vLLM implements forced scoring using
`prompt_logprobs` over prompt tokens plus committed tokens. A 4xx response on the
scoring request or missing prompt-logprob capability raises `UnsupportedCapability`.
A malformed or unavailable service must not be represented as a successful audit.

The shared first-divergence rule uses epsilon 1.0. Matching token IDs proceed;
a divergent known token whose margin is at most epsilon can accept. A margin
above epsilon rejects; an absent alternative inside the bound is inconclusive.
These semantics are more permissive than unconditional exact token equality.
Both execution paths use the same signed challenge and verdict bindings.

Sampling is independent of replay implementation: the live 30%/5%/2% ladder,
amount weighting, audit pool and cooldown continue unchanged. Pipeline/range
and MoE evidence use their respective replay paths. See [audit design](AUDIT_DESIGN.md)
and [sampling](AUDIT_SAMPLING.md).

Source: `node/auditor/worker.py:169`, `node/auditor/equality.py:14`,
`node/engine/adapters/transformers.py:208`, `node/engine/adapters/http.py:296`.
Adapter availability does not establish live per-request fallback frequency or
an audit cost reduction percentage.
