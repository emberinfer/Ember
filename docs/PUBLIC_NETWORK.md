# Public network and discovery

The public network chain ID is published in network.json under `ledger.chain_id`.
Query live status, app version and the current validator set instead of relying
on historical counts or activation-height summaries.

Participation is permissionless subject to protocol admission. Query the Ledger
set and registrations on-chain; neither count proves independent ownership.
Read the [disclaimer](DISCLAIMER.md). Testnet Spark is valueless test credit,
is not money, is not offered for sale and is not redeemable for value. There is
no project exchange or fiat on/off-ramp. Participation is at your own risk.

## Network operation

Nodes use outbound authenticated tunnels. Seeds relay signed work and expose
finalized registration-derived discovery; they do not create a node identity,
grant stake or choose audit outcomes. A single configured seed remains an
availability dependency. Authenticate model/profile artifacts, scheduler identity,
RPC chain ID and TLS pin before joining. No live endpoint coordinates are asserted
by this repository copy; obtain them through an authenticated deployment channel.

## Synchronize an observer, then consider governance

Anyone can synchronize a non-voting ledger node without inference registration,
a GPU or prior approval. Download and verify the release, authenticate the exact
chain genesis through `bootstrap.genesis_sha256`, create a new home with an
independent node key, and use application state sync with
`bootstrap.statesync.trust_height`, `trust_hash`, `trust_period`, the independent
`rpc_servers` and public `bootstrap.snapshot_peers`. Compare status height,
catching_up and same-height app hashes against an independent authenticated RPC.
See [Ledger guide](VALIDATOR_GUIDE.md) and [supplier runbook](BOOTSTRAP-RUNBOOK.md).

The network bootstrap object currently declares `pending-publication` with null
anchors and empty suppliers. Values must be authenticated and filled by the
release/online operators; this schema is not a claim of production availability.
Never substitute staging genesis or invented checkpoint values.

Becoming a voting validator separately requires validator/operator keys, current
admission registration and stake, readiness proof, a current validator operator's
proposal, the governance timelock and successful activation at the announced
height. Synchronization alone grants no vote. See [membership](VALIDATORS.md).

## Trust and authority

Ledger nodes order deterministic accounts, stake, registrations, holds, evidence,
audits and settlement under CometBFT's less-than-one-third Byzantine voting-power
assumption. Membership updates are implemented; admission is not simply static
genesis policy. See [Ledger membership](VALIDATORS.md).

Gateways can censor, delay and observe ordinary plaintext requests and retain
consumer account custody. Relays cannot forge node signatures. TLS protects
transport, not data from authorized execution/audit processes. Profile hashes
bind selected bytes, not model quality or organizational independence.

Audit resolution occurs after evidence submission. Source entropy uses finalized
blocks after freeze, not necessarily blocks after submission. Gateways cannot
name auditors. Rejection requires corroboration; a third whole-model review may
resolve an initially uncorroborated case. See [audit design](AUDIT_DESIGN.md).

## Enrollment and exit

Since AppVersion 10, registration offers both execution and on-chain audit eligibility by default. `--no-exec` and `--no-audit` select registration-time opt-outs. Eligibility still requires the appropriate evidence and replay capabilities. Registration binds the identity, exact signed profile, layer range and capacity. There is no deregistration or mid-term removal of an enabled role. Registrations lapse after the live 200,000-block TTL; stopping a process does not erase its registration.

Use [Quickstart](QUICKSTART.md) for release checks and enrollment. Keep private
state and identity keys protected. Expiry, pending liabilities and unbonding are
separate waits; see [operations](OPERATIONS.md). Stopping an endpoint does not
cancel on-chain availability commitments.

## Pricing and abuse controls

Effective chain prices and frozen multipliers determine the bounded hold and
actual-token settlement. See [pricing](PRICING.md). Rate limits, body/token and
concurrency bounds, nonce fencing and attempt deadlines constrain resource use;
they do not guarantee public endpoint uptime. Unresolved live holds have a
5,000-block expiry and gateway recovery submits refunds. Never infer fraud from
an offline connection alone.
