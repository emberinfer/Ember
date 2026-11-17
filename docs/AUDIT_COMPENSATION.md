# Audit compensation and work limits

AppVersion 4 supports per-model `audit_comp` governance policy. A governor sets
it with a signed `set_audit_comp` action (`model_id`, `audit_comp`), or includes it
in model registration/genesis. Existing models without this policy retain their
historical settlement path. A request freezes the policy when its initial hold is accepted;
a later policy update affects new requests. The model policy carries a SHA-256
`benchmark_ref` identifying its published replay-cost fixture.

The [GB10 Qwen replay fixture](../services/chain/testdata/audit_replay_gb10.json)
records 36 forced-prefix replay observations on Qwen2.5-1.5B BF16: three repetitions
of input lengths 9, 33 and 129 and prefixes 0, 1, 8 and 32. It records runtime,
model config digest, wall times and the derivation. The accounting assumption is
1,000 subunits per replay second, **not a measured monetary exchange rate**.
An ordinary least-squares fit on `[1, N_in/1000, N_out/1000]` clips negative token
coefficients to zero, rounds token rates up, then uses the largest positive
residual as fixed overhead. The resident model is loaded before timing; the
fixed residual is dominated by the first positive-prefix execution, not a
measurement of model loading or a directly isolated per-request fixed cost.
The original benchmark-only output rate was 38526. The historical saved-load envelope raised only that rate. Its fixture parameters are
`o_fixed=704`, `r_in_per_ktok=0`, `r_out_per_ktok=43000`, `formula_ver=2`,
`margin_bps=12000`, `audit_share_bps=1500`. The zero input coefficient reflects
this small noisy corpus; it does not establish that input processing is free.
These fixture parameters describe one recorded profile; there is no benchmark claim for
other models, longer inputs, other devices or independent production validation.

For that policy the deterministic formula, in subunits, is:

```
floor((1000*o_fixed + r_in_per_ktok*N_in + r_out_per_ktok*N_out_billed)
      * margin_bps / 10000000)
```

Intermediate arithmetic uses arbitrary-precision integers and rejects int64
overflow. This formula uses decimal ktok (1,000). It does not set current model token prices; effective governance tables use their own per-1,000-token rounding, while the optional context interaction retains divisor 1,024. Input counts come from
the signed execution receipt and pricing checks against the frozen tokenizer
commitment. Independent replay classifies same-version input-count mismatches
as objective offenses in either direction. Missing artifacts or a different
tokenizer version cause a service-failure refund without a penalty; see
[token pricing](PRICING.md).

The signed audit verdict cites `n_out_billed`; it must equal one plus the largest
position in its signed challenge, or zero for an empty position list, and cannot
exceed the execution receipt's output count. Node tooling meters the returned replay prefix and cites only that prefix.
A deterministic check that rejects before replay cites zero output tokens. Extra local replay outside the cited prefix receives nothing.
Zero prefix bills fixed overhead plus the input term only. As with other audit
evidence, the signed assertion does not prove physical computation occurred.

A `settle` action for a request with compensation policy includes `audit_receipts`
in assignment order, one for each submitted verdict. For formula v2, each protobuf
`SettlementReceipt` has `version=2`, canonical chain request ID, `AuditComp`
(auditor, formula version, input count, billed prefix, compensation), and an
`audit_shortfall_subunits` field. The chain recomputes all fields and rejects any
mismatch. This applies to both whole-model and pipeline settlement. The
`ExpectedAuditReceipts` Go helper constructs the required statements.

Funding comes exclusively from the request fee's audit allocation: no issuance
or external funding is added. The policy keeps a 15% budget in this version.
Payments consume that request budget in assignment order; each payment is
clamped to the remainder and the difference is recorded as a shortfall. Whole-model requests
use the fixed 80/15/5 gross allocation. Supply and demand affect only the
consumer gross fee through its frozen multiplier; role weights are not multiplied.
A payment exceeding the actual role allocation fails closed. The unused audit
allocation stays in that hold's `audit_escrow_subunits`; no withdrawal or reuse
operation is implemented. New compensated models without role pricing use the
80/15/5 gross allocation. Compensated pipelines allocate 80% to stage work, 15%
to audit escrow and 5% to the consumer, with deterministic integer remainder
handling. Legacy pipelines without compensation policy retain their existing
stage-work allocation. Gross allocation and the amount actually received by an
auditor are separate quantities.

A rejection/service failure preserves the existing full payer refund. Its fee
and audit budget are therefore zero, and every replay's compensation becomes a
recorded shortfall. This is an economic limitation: the implementation cannot
promise nonnegative auditor P&L for rejected requests or measured replay-cost
outliers. Zero-fee/full-refund audit funding policy remains deferred to mainnet
pricing/funding policy. Whole-model adjudication and consumer appeals are implemented separately; their deposits and penalty-cost handling do not guarantee reimbursement of every replay cost.

The signed governor action `set_audit_work` sets `epoch_blocks` (default 1,000)
and `max_per_executor` (default 64). Both are frozen per request. At assignment,
the actual transaction block height selects the epoch. Every executor in a
pipeline consumes a count; confirmation assignment consumes another count.
Reaching the cap rejects assignment before recording work or changing the pair
streak. A new epoch has a separate count. Existing first-data and future-block
entropy rules apply unchanged. Old direct draw records without frozen work
policy remain readable.

For compensated requests, admission also requires the fee's 15% share to cover
the formula before margin for the full possible replay prefix. This check stops
known underfunded small-request work before assignment; it does not establish
availability, actual future wall cost, or a guarantee of profitable replay. Historical saved-load admission reports do not establish current live profitability or model availability.

Operators can collect an epoch report from a saved chain state and measured
costs:

```sh
PYTHONPATH=node:core-py:proto python -m auditor.pnl \
  --state state.json --costs costs.json --output auditor-pnl.json
```

The costs file is a JSON array of `{hold_id, auditor, cost_subunits}`. The artifact
reports each auditor's assigned/replayed counts, compensation received, share
shortfalls, cost and P&L by epoch. Missing measurements produce `null` P&L;
unsettled work has zero payments so far. Registered idle auditors appear with
zero counts. Duplicate/negative costs fail closed. Use the report context to
distinguish chain payments from an explicitly labeled counterfactual replay
experiment.

## Reproduction and limitations

The repository tools `ops/benchmark-audit-replay.py`, `ops/measure-audit-load.py`,
`ops/calibrate-audit-load.py`, `ops/derive-audit-comp.py` and
`ops/derive-audit-envelope.py` accept local measurement inputs and produce
accounting fixtures. Their saved-data scenarios are not current effective model
prices or live profitability measurements. Do not replace the on-chain per-model
price table with a historical equal-price fit. See [current pricing](PRICING.md).

Teacher-forcing defaults, sampled audits and current slashing rules are described
in [audit design](AUDIT_DESIGN.md), [sampling](AUDIT_SAMPLING.md) and
[slashing](SLASHING.md). The compensation policy's introduction did not implement
all of those features, but the current repository does implement them.

## Segment compensation

The current source also supports formula version 3: add
`r_seg_per_krow * replayed_segment_rows` to the numerator before the same
margin and final division. Version 2 forbids nonzero segment rows. Signed
segment citations and policy validation remain required; this is not a
self-reported elapsed-time payment. See `services/chain/audit_comp.go:47` and
`services/chain/moe.go:568`.
