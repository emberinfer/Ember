# Audit compensation and work limits

AppVersion 4 supports per-model `audit_comp` governance policy. A governor sets
it with a signed `set_audit_comp` action (`model_id`, `audit_comp`), or includes it
in model registration/genesis. Existing models without this policy retain their
historical settlement path. A request freezes the policy at first acceptance;
a later policy update affects new requests. The model policy carries a SHA-256
`benchmark_ref` identifying its published replay-cost fixture.

The [GB10 Qwen replay fixture](../services/chain/testdata/audit_replay_gb10.json)
contains 36 measured forced-prefix replays on Qwen2.5-1.5B BF16: three repetitions
of input lengths 9, 33 and 129 and prefixes 0, 1, 8 and 32. It records runtime,
model config digest, wall times and the derivation. The accounting assumption is
1,000 subunits per replay second, **not a measured monetary exchange rate**.
An ordinary least-squares fit on `[1, N_in/1000, N_out/1000]` clips negative token
coefficients to zero, rounds token rates up, then uses the largest positive
residual as fixed overhead. The resident model is loaded before timing; the
fixed residual is dominated by the first positive-prefix execution, not a
measurement of model loading or a directly isolated per-request fixed cost.
The original benchmark-only output rate was 38526. The saved-load tail-envelope
calibration below raises only that rate. The calibration-pending defaults are
`o_fixed=704`, `r_in_per_ktok=0`, `r_out_per_ktok=43000`, `formula_ver=2`,
`margin_bps=12000`, `audit_share_bps=1500`. The zero input coefficient reflects
this small noisy corpus; it does not establish that input processing is free.
These defaults describe one measured profile; there is no benchmark claim for
other models, longer inputs, other devices or independent production validation.

For that policy the deterministic formula, in subunits, is:

```
floor((1000*o_fixed + r_in_per_ktok*N_in + r_out_per_ktok*N_out_billed)
      * margin_bps / 10000000)
```

Intermediate arithmetic uses arbitrary-precision integers and rejects int64
overflow. This formula uses decimal ktok (1,000). It does not change the existing
freeze pricing formula or its context divisor of 1,024. Input counts come from
the signed execution receipt and existing pricing-v2 checks; no new tokenizer
registry or independent input-token verdict is provided.

The signed audit verdict cites `n_out_billed`; it must equal one plus the largest
position in its signed challenge, or zero for an empty position list, and cannot
exceed the execution receipt's output count. Node tooling meters the returned replay prefix and cites only that prefix.
A deterministic check that rejects before replay cites zero output tokens. Extra local replay outside the cited prefix earns nothing.
Zero prefix bills fixed overhead plus the input term only. As with other audit
evidence, the signed assertion does not prove physical computation occurred.

A `settle` action for a request with compensation policy includes `audit_receipts`
in assignment order, one for each submitted verdict. Each protobuf
`SettlementReceipt` has `version=2`, canonical chain request ID, `AuditComp`
(auditor, formula version, input count, billed prefix, compensation), and an
`audit_shortfall_subunits` field. The chain recomputes all fields and rejects any
mismatch. This applies to both whole-model and pipeline settlement. The
`ExpectedAuditReceipts` Go helper constructs the required statements.

Funding comes exclusively from the request fee's audit allocation: no issuance
or external funding is added. The policy keeps a 15% budget in this version.
Payments consume that request budget in assignment order; each payment is
clamped to the remainder and the difference is recorded as a shortfall. Existing
role pricing and scarcity allocation remain in use for whole-model requests;
a payment exceeding the actual role allocation fails closed. The unused audit
allocation stays in that hold's `audit_escrow_subunits`; no withdrawal or reuse
operation is implemented. New compensated models without role pricing use the
80/15/5 gross allocation. Compensated pipelines allocate 80% to stage work, 15%
to audit escrow and 5% to the consumer, with deterministic integer remainder
handling. Legacy pipelines without compensation policy retain their existing
stage-work allocation. Gross allocation and the amount actually earned by an
auditor are separate quantities.

A rejection/service failure preserves the existing full payer refund. Its fee
and audit budget are therefore zero, and every replay's compensation becomes a
recorded shortfall. This is an economic limitation: the implementation cannot
promise nonnegative auditor P&L for rejected requests or measured replay-cost
outliers. Zero-fee/full-refund audit funding policy remains deferred to mainnet
pricing/funding policy. Appeals are designed but are not implemented.

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
availability, actual future wall cost, or a guarantee of profitable replay. In
the measured small-fee load, admission rejected all requests. That is not a
successful production replay load or evidence that the 80% P&L target passes.

Operators can collect an epoch report from a saved chain state and measured
costs:

```sh
PYTHONPATH=node:core-py:proto python -m auditor.pnl \
  --state state.json --costs costs.json --output auditor-pnl.json
```

The costs file is a JSON array of `{hold_id, auditor, cost_subunits}`. The artifact
reports each auditor's assigned/replayed counts, compensation earned, share
shortfalls, cost and P&L by epoch. Missing measurements produce `null` P&L;
unsettled work has zero earnings so far. Registered idle auditors appear with
zero counts. Duplicate/negative costs fail closed. Use the report context to
distinguish chain earnings from an explicitly labeled counterfactual replay
experiment.

To reproduce the benchmark and collect an independent fixed two-epoch load,
use a CUDA-enabled interpreter with the existing model installed:

```sh
PYTHONPATH=node:core-py:proto python ops/benchmark-audit-replay.py \
  --output replay-benchmark.json
PYTHONPATH=node:core-py:proto python ops/measure-audit-load.py \
  --output audit-load.json
```

The load uses ten ordinary prompts and generated committed prefixes, plus a
one-token `Hi` flood shape, across two epochs. Its signed profile's existing
prices are recorded; neither fee parameters nor workload are adjusted to force
a P&L pass. Benchmark measurements are policy inputs, not a guarantee of
production economics. Audit sampling, rescaled slashing and replay caching are
not implemented by this compensation change.

To derive a separate accounting scenario from saved measurements without
changing replay times or compensation policy:

```sh
python ops/calibrate-audit-load.py --load audit-load.json \
  --output audit-load-calibrated.json --derivation audit-price-derivation.json
```

For each row, the tool computes `cost=ceil(elapsed_ns*rho/1e9)`,
`target=ceil(cost*margin_bps/10000)`,
`minimum_fee=ceil(target*10000/audit_share_bps)`, then
`price=ceil(minimum_fee/(N_in+N_out_billed))`. It selects the maximum row price
as equal input/output prices, keeping zero context pricing. The derivation
includes source hashes and every row's arithmetic. This is a local fee scenario,
not a signed genesis profile or a deployed price change. Original profile
metadata still identifies the measurement's source profile.

With the saved 40-row generated-prefix GB10 workload and the accounting rate
of 1,000 subunits/second, this minimum equal price is 513. Over two epochs,
production admission is 14/20, and 6/10 auditors have nonnegative counterfactual
replay P&L; the 80% profitability target still fails. Flood admission is 0/20;
its counterfactual replay P&L is nonnegative for 10/10, which does not establish
profitable admitted flood service. The original 704/0/38526 policy underestimates the
measured long-prefix replay cost even when fully funded. A higher fee cannot
raise compensation beyond that formula. Aggregate positive P&L and funding
coverage alone do not establish per-auditor profitability.

A subsequent, rejected saved-data calibration fits all 40 load observations together
with all 36 benchmark observations, equally weighted. The reproducible tool is:

```sh
python ops/derive-audit-comp.py --load audit-load-calibrated.json \
  --output audit-comp-derivation.json
```

It uses integer measured accounting costs `ceil(elapsed_ns*1000/1e9)` and exact
rational OLS on `[1,N_in,N_out_billed]`. Negative coefficients are fixed at zero
and the remaining coefficients refitted; fixed overhead and ktok rates are then
rounded up. The resulting scenario is `o_fixed=23`, `r_in_per_ktok=0`,
`r_out_per_ktok=55738`, with margin and audit share unchanged. R-squared is
0.849661; absolute residual p50/p95/max are 34.885/661.720/721.244 subunits.
This fit still does not support a positive input coefficient.

At the unchanged price 513, the single saved-load rerun admits 20/20 production
requests but only 3/10 production auditors have nonnegative P&L (aggregate
22,208 compensation minus 24,363 measured cost = -2,155). Flood admits 20/20;
10/10 auditors are nonnegative (1,880 minus 1,736 = 144). These are accounting
reconstructions using the same measurements, not live chain earnings or
independent validation. The production 80% gate fails. This scenario is not
adopted as the shipped default. Funding-policy changes require a governance
decision; no further calibration or economic guarantee is implied.


The adopted saved-data calibration uses an output-first tail envelope, starting
from the original `704/0/38526` parameters rather than the rejected mean fit:

```sh
python ops/derive-audit-envelope.py --load audit-load-calibrated.json \
  --output audit-comp-envelope.json
```

For each of the same 40 observations, let `C=ceil(elapsed_ns*1000/1e9)` and
`T=ceil(C*10000000/12000)`. The minimum output rate is
`max(38526, max ceil((T-704000)/N_out_billed))=43000` over positive prefixes.
The binding observation has input 8, prefix 32 and cost 2496: `T=2080000`,
so `(2080000-704000)/32=43000`. Rate 42999 pays 2495 and fails coverage.
Fixed overhead 704 and input rate 0 need no increase. All 40 load observations
and all 36 benchmark observations receive formula payment at least their
measured cost. The fixture retains the original starting policy separately;
its wall-time observations are unchanged. The tool emits source hashes and
row-level coverage arithmetic. This envelopes the calibration corpus and is
not an independent validation or a bound on future replay costs.

At the unchanged local price scenario 513, the single two-epoch rerun gives:

| policy scenario | production auditors P&L >= 0 | production admitted | flood auditors P&L >= 0 | flood admitted |
|---|---:|---:|---:|---:|
| Original policy, placeholder price | 0/10 | 0/20 | 0/10 | 0/20 |
| Original policy, price 513 | 6/10 | 14/20 | 10/10 | 0/20 |
| Rejected mean fit, price 513 | 3/10 | 20/20 | 10/10 | 20/20 |
| Adopted tail envelope, price 513 | 10/10 | 14/20 | 10/10 | 0/20 |

These P&L counts use all offered saved replays, including denied work;
they are counterfactual accounting, not live chain earnings. The tail-envelope
production total is 31,198 compensation minus 24,363 measured cost = 6,835;
flood is 3,060 minus 1,736 = 1,324. Admission accepts seven production requests
per epoch. Its production-only accounting is 27,200 minus 22,856 = 4,344;
flood admission remains zero, so profitable admitted flood service is not
established. Compensation clamps on 12/20 production and 20/20 flood requests,
recording respective shortfalls of 2,410 and 14,860. The specified 80% production
accounting target and nonempty production admission pass on these saved data.
The formula, margin 1.2, audit share 15%, allocation 80/15/5 and fee arithmetic
are unchanged; only the profile output compensation rate increases. Price 513
remains an accounting scenario, not a signed or deployed price profile.
Zero-fee/full-refund funding and future-cost outliers remain limitations.
