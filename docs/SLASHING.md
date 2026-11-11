# Confirmed-cheating slashes

State version 3 applies a stake floor and a rolling repeat-offense multiplier to
confirmed numerical cheating. New genesis defaults are `slash_floor_bps=100`
(1%) and `slash_repeat_window_blocks=1000`. The basis-point parameter accepts
1..10000 and the window accepts a positive block count; omitted/zero genesis
values select the defaults. `slash_multiplier` remains fixed at 20.

For an offender with bonded stake S immediately before settlement and attributed
request revenue R, consensus computes:

```
floor = ceil(S * slash_floor_bps / 10000)
multiplier = 2 ^ min(prior_confirmed_offenses_in_window, 3)
slash = min(S, max(20 * R, floor) * multiplier)
```

The ladder is 1x, 2x, 4x, 8x, then 8x for subsequent offenses. Arithmetic uses
unbounded integers before capping to current bonded stake. Even one positive
stake unit has a positive floor. A penalty cannot create negative stake or an
overflowed credit. Transaction application remains atomic if any credit fails.

The window is measured at the actual settlement transaction's block height H.
A prior confirmed settlement P counts exactly when `0 <= H-P < window`;
an offense exactly one window old has expired. Multiple separate confirmed
requests in one block count separately. The account identity shares its history
across models. Only the last three confirmed heights need retention: three
qualifying predecessors already select the capped multiplier. Each hold's
`slashes` retains the complete charged record (node, height, revenue, stake
before, rounded floor, multiplier and amount). `/state` exposes the bounded
`confirmed_offenses` history; `/hold/<id>` exposes the per-request records.
Both survive normal state save/reload.

A whole-model rejection uses the submitted actual charge as R, preserving the
existing 20R basis. A pipeline rejection uses the offender's aggregate executed
credit before fee splitting. Only the fault-node set agreed by two independently
assigned signed REJECT verdicts is charged. An account executing multiple stages
receives one floor and one offense per rejected request. Honest stages have no
offense, no stake change, and no execution credit for a rejected request.

All rejected request reservations are refunded in full; the slash is transferred
from each offender's bonded stake to the payer. No new supply is issued. First
accusations, an independent confirming ACCEPT, service failures, expired holds,
and accepted execution do not add offenses. Existing pending-exposure checks,
registration stake locks, unbonding delays and honest exit paths still apply.
Only successful confirmed-reject settlement records an offense, and a settled
hold cannot be charged twice.

With the 1% default, a 50,000-unit stake and 3-unit request revenue yield a
500-unit floor versus the 60-unit revenue-based minimum (8.33x). At 100,000 stake
with 2..8 revenue,
the floor is 1,000 versus 40..160. Revenue-dominant requests retain their 20R
minimum before escalation. These are credit distributions, not dollar-valued
loss or a measured global economic-security guarantee. A fresh identity still
starts with a new history and must supply stake.

This changes consensus and bumps `AppVersion` from 2 to 3. Version-2 persisted
states are explicitly refused; they are never silently reinterpreted. Existing
chains need a coordinated, separately reviewed state migration or a new genesis.
All validators must run the same version and parameters. Version-2 chains must
retain compatible binaries until migration or replacement genesis. CPU chain tests cover the new policy independently of GPU inference.
