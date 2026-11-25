# Coordinated application upgrades and recovery

The live chain is `ember-testnet-1`, with genesis dated 2026-11-14 and five Ledger nodes. Live AppVersion 12 activated at height 116582. The source base constant is 11; the coordinated upgrade ladder advances the running state to 12.

Software release version, source base AppVersion, persisted state version and
live consensus AppVersion are different values. `services/chain/state.go:22`
sets base 11; `services/chain/validators.go:436` advances 11 to 12 at the configured
height. Existing activation manifests are the mechanism; changing documentation
or replacing a binary alone does not activate consensus rules.

## Plan and activate

1. Authenticate the release and exact build. For v0.7.0, checksum signatures are
   not published; see [signing](SIGNING.md) for the available checks and trust gap.
2. Query chain ID, current state version, height and parameters. Preserve a
   finalized state snapshot, consensus signing state and all dependent archives.
3. Validate the matching supported upgrade rung on an isolated copy. Distribute
   the same `--membership-upgrade` manifest to participants, with matching chain
   ID and a future activation height. Preserve its hash and full contents.
4. Coordinate replacement and monitor the exact activation block. Missing an
   activation height fails closed. Confirm AppHash agreement and reported consensus
   application version before reopening admission.
5. Retain the manifest ladder for historical replay and recovery. Never restore
   one signing identity backward or run two copies concurrently.

LoadState accepts supported persisted versions 4 through 12. The ladder contains
separate version gates; do not infer that every old state can skip directly to
12. An incompatible migration still needs an explicitly reviewed procedure.
Active holds, nonces, registrations, assignments, signed artifact bytes and
accounting rights must not be silently dropped by a new genesis.

## Membership and model changes

Dynamic Ledger membership is implemented, when initialized. An optional v10→v11
`membership_init` maps the existing live consensus set to operator accounts and
initial parameters; it does not replace that set with an arbitrary new one.
See [Ledger membership](https://github.com/emberinfer/Ember/blob/master/docs/VALIDATORS.md). Publication and price governance do not
automatically require a new ledger chain ID. Signed profile bytes remain immutable;
new profiles and authorized compute corrections use their respective transactions.

AppVersion 10 introduces registration availability and whole-model adjudication.
AppVersion 11 adds effective token-price overrides and persistence changes.
AppVersion 12 gates architecture-first expert TopK parsing. The live Kimi correction
sets F to 208,736,282,112 FLOPs/token without replacing profile bytes or identity.
See [compute](https://github.com/emberinfer/Ember/blob/master/docs/compute-v11.md) and [governance pricing](https://github.com/emberinfer/Ember/blob/master/docs/v11-pricing.md).

## Backup and recovery

Version-11+ state can reference `<state-file>.archive/` deltas. Commit persists
archive data before the canonical state file. Copy the archive with the state or
use a complete exported snapshot; missing archive history fails load. Retain
account keys, node identities, gateway evidence store and spool, and activation manifests
with appropriate access controls. Restore first on an isolated host.

If activation produces divergent state or cannot commit, halt affected admission
and coordinate recovery from a reviewed consistent state and compatible binaries.
A post-activation downgrade is not a local restart decision. Never sacrifice
consensus signing high-water marks to restore an older application snapshot.

Source: `services/chain/validators.go:361`, `services/chain/state.go:1569`,
`services/chain/history_archive.go:78`, `services/chain/verified_compute.go:110`.
