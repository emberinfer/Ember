# ADR: CometBFT and a custom Go settlement application

Status: accepted. Date: 2026-11-08.

Maintainers and operators can use this record to understand the ledger choice,
alternatives and operational costs. Related contracts: [architecture](PRODUCTION_ARCH.md),
[threat model](THREAT_MODEL.md) and [Engine SPI](ENGINE_SPI.md).

## Context and alternatives

The ledger needs deterministic integer Spark balances, stake, bounded reservations,
receipt/audit references, transfers, explicit issuance and replay protection.
Inference, HTTP calls and nondeterministic floating-point comparisons stay outside
consensus. Signed auditor verdicts are inputs to versioned state-transition rules.

| Route | Consensus security / trust | Operations and integration cost | Go migration | License / dependencies |
|---|---|---|---|---|
| CometBFT application chain + custom ABCI app | Established BFT protocol; <1/3 Byzantine voting power assumption; application correctness remains the Ember project's responsibility | Run validator/full-node P2P, snapshots, upgrades, key management; own state/proof/fee modules | Go ABCI boundary; extract ledger transitions from SQL, implement deterministic authenticated state | CometBFT Apache-2.0; verify pinned release and dependency licenses before adoption |
| Cosmos SDK app on CometBFT | Same consensus assumption; SDK modules offer application patterns, not extra Byzantine tolerance | More module/config/upgrade surface, but reusable auth/bank/staking/governance | Native Go; map Spark rules into SDK modules, reconcile custom hold/audit economics | SDK Apache-2.0; transitive/version review still required |
| Self-built BFT | Safety/liveness, networking, membership changes and proofs all need new scrutiny | Highest security/review cost, longest delivery; little reuse | Reuse Go syntax/library only, no proven consensus shortcut | Project license selectable; dependencies separately reviewed |
| Ethereum L2 anchoring | Posting a state hash alone does not provide decentralized Ember execution/finality; full L2 route depends on chosen rollup's proofs, DA, sequencer and L1 assumptions | Contracts, bridges, external fees/finality, monitoring and upgrade/admin-key risks | Go remains off-chain; redesign proof/contract settlement and account mapping | L2 client/contracts vary; no blanket license assertion without selecting a stack |

These are not four independent consensus algorithms: Cosmos SDK is an application
framework commonly using CometBFT; anchoring is not automatically a rollup. A fair
comparison separates application engineering from consensus/DA trust.

## Decision

Use CometBFT with a minimal custom Go ABCI application and one network-wide Spark
ledger. Model registrations bind immutable manifests and execution Profiles;
model-chain namespaces do not replace the ledger chain ID. Retain receipts as
signed evidence objects rather than treating SQL rows as a consensus specification.
The implementation is in [services/chain](../services/chain/) and
[cmd/emberchain](../cmd/emberchain/). SQLite remains a development backend.

Transfers are network-only. Spark has no fiat conversion or project-operated
exchange. Public source is Apache-2.0; model and dependency licenses remain separate.
Validator membership is fixed at genesis; stake-backed node registration does not
make a node a consensus validator.

## Consequences

The project owns the deterministic transaction codec, integer accounting, bounded
work, authenticated state, query verification, height-based lifecycle and application
upgrade rules. CometBFT supplies consensus under its less-than-one-third Byzantine
voting-power assumption; it does not validate inference or ensure operator diversity.

Cosmos SDK remains an alternative if standard staking/governance/IBC modules justify
a larger dependency and migration surface. Building a new BFT protocol would add
substantial security and operational work. Posting a hash on another chain alone
would supply neither Ember execution validity nor data availability.

Dynamic validator updates, governance, compact state proofs and snapshots/state sync
remain open work. Initial stake distribution, key custody and coordinated upgrades
remain operator/governance responsibilities. No SQLite database becomes decentralized
by running several copies; consensus safety also does not replace independent audits.

## Primary references

- [CometBFT consensus](https://docs.cometbft.com/v1.0/explanation/core/consensus)
- [ABCI boundary](https://docs.cometbft.com/v1.0/spec/abci/)
- [CometBFT license](https://github.com/cometbft/cometbft/blob/main/LICENSE)
- [Cosmos SDK](https://github.com/cosmos/cosmos-sdk)
- [Ethereum scaling](https://ethereum.org/en/developers/docs/scaling/)
