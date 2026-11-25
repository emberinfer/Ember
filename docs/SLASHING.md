# Confirmed-offense penalties

The chain requires `slash_multiplier=20`. Conviction requires corroborated signed
rejection evidence; availability failure alone cannot slash. Whole-model
adjudication may use one additional reviewer beyond the second.

For current stake S and attributed revenue R, the uncapped per-offense basis is:

```text
floor = ceil(S * slash_floor_bps / 10000)
ladder = 2 ^ min(prior distinct offense heights inside repeat window, 3)
basis = min(S, max(20*R, floor) * ladder)
```

The source also imposes a per-node, per-block budget. The first slash fixes the
block's ladder and limit using stake at block start:

```text
limit = max(floor(stake_start * cap_block_bps / 10000),
            ceil(stake_start * slash_floor_bps / 10000))
paid_this_offense = min(basis, limit - already_paid_this_block)
```

Multiple offenses in the same block share one history height and ladder step;
they do not independently advance the ladder. Later payments can be zero after
the block budget is exhausted. A prior height exactly one repeat window old is
excluded. Arithmetic uses arbitrary-precision intermediates before bounded
integer conversion. The source defaults are floor 100 BPS, repeat window 1,000
blocks and block cap 500 BPS. Query the live parameters rather than assuming
these defaults are the deployed settings.

`/hold/<id>` exposes charged records; offense history and block budgets persist.
The whole-model revenue basis is actual charge; pipeline attribution uses the
agreed fault-node work. Honest unrelated stages are not automatically punished.
Ordinary whole-model penalties credit the payer. With adjudication, collected
penalties first pay outstanding adjudication costs into the audit pool; only
the remainder reaches the payer. MoE Plan B applies its separate honest-work
payment waterfall and must not be described as always payer-only.

Safety modes and signed offense grading determine whether a rejection is
slash-eligible. Gray or pending-review handling can suppress penalties. A timeout,
unconfirmed accusation or missing reference is not confirmed cheating.

Use the [upgrade procedure](UPGRADE.md) to check the live AppVersion and
activation requirements. See also [audit design](AUDIT_DESIGN.md) and
[adjudication](../services/chain/adjudication.md).

Source: `services/chain/slashing.go:30`, `services/chain/slashing.go:112`,
`services/chain/moe.go:912`, `services/chain/state.go:365`,
`services/chain/misjudge.go:36`.
