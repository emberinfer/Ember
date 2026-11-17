# Public network and discovery

The live chain is `ember-testnet-1`, with genesis dated 2026-11-14 and five Ledger nodes. Live AppVersion 12 activated at height 116582. The source base constant is 11; the coordinated upgrade ladder advances the running state to 12.

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
