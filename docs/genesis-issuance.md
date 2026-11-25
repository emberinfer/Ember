# Genesis consumption vouchers

A chain may configure `parameters.genesis_issuance` at genesis. This creates the
reserved `ember:genesis-subsidy-pool` account and disables legacy work-evidence
subsidy claims. Ordinary transfers cannot replenish that account. Chains without
this configuration retain their existing accounting rules.

Subsidies are consumption vouchers, usable only for inference fees; they cannot
be transferred, staked, or used as deposits. The restriction is permanent. Every
recipient must confirm this disclosure through a signed API transaction before a
grant is issued. Applications must display the disclosure before requesting that
confirmation. `marked_subunits` is separate from an account's clean `balance`.

## Genesis configuration

`standard_request_subunits` fixes the published reference profile's 512-input /
256-output request price; one Spark is 1,000,000 subunits. `price_table_ref`
identifies the published price table. `organic_forecast_equivalents` and
`forecast_ref` supply the published conservative first-year organic settlement
forecast and its reference. Initialization rejects a forecast below twice the
pool; it never raises that forecast or silently resizes the pool.

`genesis_time_unix` must match the CometBFT genesis timestamp. `governor_signers`
contains five distinct account IDs, also listed in the genesis `governors` set;
`governor_threshold` must be 3. `founding_entities` names the founding
organizations. `registered_entities` maps account IDs to registered entity IDs,
including retrospective operators. Additional identities require a governor
quorum via `register_entity`; existing identity mappings cannot be replaced.

The fixed rules are returned under `parameters.genesis_issuance.fixed_rules`.
They may be omitted from the input; explicit values must exactly match:

| Rule | Value |
| --- | --- |
| Total pool, standard-request equivalents | 1,714,286 |
| A / B / C equivalents | 1,200,000 / 342,857 / 171,429 |
| A seed accounts / single grant / loss factor | 8,000 / 100 / 1.5 |
| Daily total / A / B / C limits | 1% / 0.6% / 0.25% / 0.15% of the initial pool |
| B supply / audit / consume weights | 6 / 3 / 1 |
| B account cap | 1,000 equivalents |
| A activation window / minimum input | 7 days / 128 tokens |
| C approval / grant cap / public notice | 4 of 5 / 0.25% of the initial pool / at least 7 days |

Amounts are converted to integer subunits once at genesis. Percentage limits
round down to whole subunits. Each channel has an independent budget and daily
counter; unused capacity cannot move between channels. UTC midnight resets daily
counters. Initial `misjudge.slash_mode` is `refund_only`. Mode upgrades fail closed
until their calibration, minimum sample, runtime, and appeal evidence workflow is
available. Objective offenses continue to follow the existing slash rules.

## Signed subsidy actions

Use the usual signed transaction envelope with an action of this form:

```json
{
  "type": "subsidy",
  "subsidy": {
    "op": "claim",
    "confirm_consume_only": true,
    "id": "unique-grant-id",
    "batch_id": "public-batch-id"
  }
}
```

A new public key can submit `claim` or `confirm` with nonce 1 without an existing
funded account. A failed request does not consume its nonce or confirmation.
`confirm` requires `confirm_consume_only: true` and records that account's signed
disclosure acknowledgement. Another signer cannot confirm on a recipient's behalf.

`claim` selects the higher of the fixed A grant and the account's published B
award. A tie selects A. A/B claims are mutually exclusive and lifetime limited,
including an A claim that later expires. Client-specified amounts cannot change
the computed entitlement. An A grant remains pending until a successful audited
inference settlement with at least 128 input tokens and a positive fee. The first
qualifying inference needs clean funding. Pending vouchers cannot fund their own
activation. Empty blocks recycle unactivated grants at the seven-day deadline
back into their original channel. Activated vouchers have no unlock date.

Inference fees reserve marked funds first. Cancellation, timeout, rejected work,
and unused fee reservations return the original balance classes. Executor and
auditor payments credit clean Spark. A payer's consumer rebate retains the mark
up to the subsidy-funded portion, preventing conversion through refunds or rebates.
The same accounting applies to pipeline and encrypted inference requests.

## Recompute B awards

Publish `testnet_records` with unique `request_id`, `payer`, `executor`, optional
`auditor`, and `settled` / `audited` flags. Unsettled records receive no points.
Records whose payer and executor map to the same registered entity are excluded
entirely, including audit points. A settled non-self request receives 6 supply
points, 3 points for a completed audit, and 1 consume point.

One weighted point is valued at one reference-request equivalent. Awards are
capped at 1,000 equivalents per account, then proportionally reduced if their
sum exceeds B's budget. Integer rounding leaves the remainder in B. Scores and
awards are stored at genesis; later claims cannot alter them.

```sh
emberchain subsidy-retrospective --app-state app-state.json
```

This command recomputes the list from the published input without contacting a
chain. Its JSON output is keyed by account ID and includes each component,
weighted score, and final subunit award.

## Developer inventory

A governor submits `propose_c` with `id`, recipient `to`, `amount`, and
`public_notice` describing the product and expected users. The notice and its
consensus timestamp are immutable. Distinct governors sign `approve_c` for that
ID. `issue_c`, with a `batch_id`, requires four approvals, seven elapsed days,
recipient disclosure confirmation, available C inventory, the per-grant cap,
and both daily limits. An optional `amount` on `issue_c` issues a tranche; omit it
to request the entire unfunded remainder. A proposal above the C daily limit
requires tranches on separate UTC days. Every tranche has its own grant ID and
batch, and cumulative funding cannot exceed the approved proposal. Proposal approval does not reserve capacity or bypass
issuance limits.

A compliant grant creates developer distribution inventory. It cannot be
transferred or spent by the developer. The developer signs `distribute_c` with
the proposal `id`, `to`, `amount`, and `batch_id`. The end user must independently
confirm the disclosure first. Each distribution creates a child grant with its
own seven-day inference activation requirement; it does not issue money twice
or charge the daily counters a second time. Undistributed inventory recycles
after three calendar months. Unactivated child grants recycle separately.

Governors can permanently disqualify a developer using `revoke_c` with an ID and
a public violation finding in `public_notice`. This requires the normal 3-of-5
governor quorum and recycles undistributed inventory. Revocation details remain
queryable on the proposal. The chain does not attempt to infer off-chain sales.

## Governor approvals and public accounting

Governor-controlled policy changes require three distinct signed
`approve_governance` actions containing the exact proposed `governance_action`.
The governor then submits that action through its usual handler. Approvals bind
to its complete JSON digest and cannot execute twice. C grant approvals use their
separate 4-of-5 rule. The privacy epoch governance operations also use this quorum.

Available queries:

| Path | Contents |
| --- | --- |
| `/parameters` | Fixed issuance rules, reference price, entity registry and governor configuration |
| `/account/ember:genesis-subsidy-pool` | Reserved pool balance |
| `/subsidy` | Channel budgets, grants, batches, activation references, recycle records, C proposals, daily counters, B awards and reminders |
| `/subsidy-monthly` | Opening balance, issuance by channel, recycling and closing balance for months with ledger activity and the current month |
| `/governor-metrics` | Trailing 30-day settled executor amounts, non-founding share inputs, consecutive qualifying days, cumulative issuance, calendar age and reminder deadlines |

Pool reconciliation is `initial - issued + recycled`. Child distributions are
linked to their parent and excluded from cumulative issuance; their recycled
amounts count normally. Every recycle record includes its grant, channel, amount,
and consensus timestamp.

Non-founding share uses settled executor subunits and registered organizations;
unknown organizations count conservatively toward the total only. Pipeline work
is attributed to each executor's actual credited share. Thirty complete
consecutive UTC days at or above 20% trigger a reminder; zero-traffic days do not
qualify. Cumulative issuance at 50% or twelve calendar months also triggers one.
Each trigger records a 90-day deadline and the required expansion to at least
seven signers, including two signers from outside the founding organization, with a 4-of-7 threshold.
These records expose the social restructuring obligation; they do not automate
membership changes.

Genesis issuance is an optional configured mechanism. Follow the
[upgrade procedure](UPGRADE.md) to check live AppVersion, build compatibility
and coordinated activation. Issuance configured at genesis applies to the new chain.
