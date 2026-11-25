# Ledger node membership

Ledger nodes use Ed25519 consensus keys, distinct from signed operator accounts.
Every Ledger node has power **10**, matching the genesis tool: stake is admission
collateral and never changes voting weight. `/validators` exposes the registry,
organization IDs, readiness heights, current change, and applied/rejected history.
`/state` includes the same data in the application hash. Organization IDs are
operator declarations, not independently verified organizational credentials.

Observer synchronization is independent of this admission process: see
[Ledger guide](VALIDATOR_GUIDE.md). A non-voting node requires no inference
registration, GPU profile, collateral or validator proposal to synchronize.

## Admission and authority

Before registering, fund and stake the operator using the existing account
transactions. Register a signed `ResourceProfile` with `register_node`, keeping
its registration current. `validator_register` requires the operator's signed
transaction, a `validator` object (`pubkey`, `operator`, `org_id`, `power: 10`,
`status: "pending"`), and a `ready` proof from the consensus key.
Keys in JSON are base64; registry IDs and removal IDs are lower-case public-key
hex. An operator may have only one non-exited consensus identity.

The ready object contains `pubkey`, `height`, and `signature`. The consensus key
signs `chain.ReadySignBytes(chainID, operator, pubkey, height, blockHash)`, using
a recent finalized block. This proves key possession and recent observation of
the chain. Keep proofs fresh with operator-signed `validator_ready` transactions.
Do not use the consensus vote-signing endpoint for application proofs; use a
separate domain-aware signing operation, never arbitrary consensus messages.

An operator of an active Ledger node may propose a join. A voluntary exit must be
proposed by the leaving Ledger node's operator. This interim authority does not
implement governor multisignatures or emergency removals. Admission stake alone
never grants proposal authority.

## Announce, check, and activate

Submit `validator_set_change` with a `set_change` object containing `add`,
`remove`, and `effective_height`. Exactly one addition or removal is permitted
per proposal; batching is deliberately disabled. Added records must exactly
match their signed registry admission. Removal requires no new financial rule.

The effective height must be at least `proposal_height + change_delay_blocks`.
Choose the height with enough margin for transaction inclusion. The chain
accepts only one pending change and one proposal per sliding
`change_window_blocks` interval, including proposals later rejected at execution.
The window must be at least `change_delay_blocks + 2`; delay must be at least 3.
Once a set reaches three members, it can never drop below three. A legacy set of
two may expand. `max_validators` caps active membership, with a supported range
of 3 through 1000. Registry history has a separate lifetime cap of 10000 keys.

Reachability is derived from commit votes in `DecidedLastCommit`, excluding nil
votes and absent votes. A new candidate also needs a fresh consensus-key proof.
Both sets must satisfy `3 * reachable > 2 * set_size`. For example, two needs
two, three needs three, and four needs three. A set of three cannot keep producing
blocks after one member stops. The minimum set with single-stop tolerance is
four. The registry floor of three is not an availability guarantee.

Checks run on proposal and again at `effective_height - 2`, before emitting ABCI
updates. If the second check fails, the change is recorded as rejected, no update
is emitted, and leaving members return to active status. Candidate stake and
profile registration must remain valid; refresh registration and readiness
during a long announcement period, including through the effective height.
A profile need not already cover the entire announcement period at proposal.
At update emission it must cover the effective height.

CometBFT applies an update returned at height H at H+2. Registry membership
switches at that exact height. There is no second update and no silent retry.
Observation cannot guarantee future uptime: a node can fail after the last
check, and a candidate's proof is not a network connectivity oracle. Operators
must verify peering, synchronization and monitoring before proposing a join.

While pending, active, or leaving, membership locks the operator's stake.
After exit, use the existing `unbond` and `claim_unbonded` transactions. Existing
registration, exposure, and unbonding-period rules still apply. A pending
candidate can withdraw an unproposed or rejected admission using
`validator_cancel`; it cannot withdraw during its pending/scheduled join.
Exited keys remain in history; use a fresh key for a later admission.

## Activation and live status

Read the live chain's `/validators`, `/parameters` and RPC `/status` for its
current membership and app version. Historical activation examples do not
assert current membership or bootstrap anchors.

Membership changes require initialized on-chain membership and the admission and
continuity rules above. The optional v10→v11 initialization binds the existing
consensus set; it is not automatic at every release. Follow [Upgrade](UPGRADE.md)
for the current version ladder. Historical version-4 activation examples are not
current deployment instructions. Ordinary `/validators` queries do not constitute
an independently authenticated dynamic membership proof; use a suitable CometBFT
light client tracking set transitions.

## Operator checks and key rotation

Confirm hardware capacity, synchronization, stable public peering, protected
keys, monitoring, alert delivery, and an operator on call before admission.
Observe vote participation and the scheduled effective height on all peers.
For routine consensus-key rotation, admit a temporary operator/key first, then
have the old operator exit, respecting the same delay, floor and change window.
The same operator cannot hold two active keys. If the old key is compromised or
quorum is lost, this interface supplies no emergency override.

The local process drill in [ops/validator_dynamics_drill.py](../ops/validator_dynamics_drill.py)
creates only new homes and loopback listeners. It tests joins, exits, pending
change interruption, the size floor, the three-member boundary and four-member
single-stop availability. Its `--legacy-binary` mode also tests coordinated
activation, historical replay from an empty observer, and the first expansion
from two to three. Output contains local test keys; protect its directory.
