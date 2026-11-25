# Publish authenticated bootstrap anchors and snapshot suppliers

This runbook provisions a dedicated non-voting snapshot supplier. It does not
admit a validator, change chain rules, alter state serialization or app hashes,
or authorize copying production signers. Production changes require a separately
authorized operator session. Prefer a synchronized dedicated non-signing donor;
if only a validator has the initial state, plan its I/O and quorum budget before
enabling snapshot production on one machine. Never restart quorum together.

## Snapshot format and capacity

`emberchain start` reads `[statesync]` consumer settings and `[snapshots]` producer
settings from home/config/config.toml. Production is disabled by default
(`interval=0`, `keep_recent=2`); consumption is independently disabled. There is
no snapshot-interval CLI flag and no general offline tar importer.

Format 1 transfers canonical State.Bytes, all state.archive files and all
state.chunk-spool files. It excludes CometBFT databases, node keys, validator
keys, signing high-water state and other home files. Metadata binds chain ID,
height, state format version, app hash, total size and SHA256 of every 1 MiB
chunk. Snapshot.Hash is the complete package SHA256; it is neither app hash nor
trusted header hash. Package checksums protect transport integrity, not the
independent chain trust anchor.

Current limits are 256 MiB/package, 256 chunks, 32 KiB metadata and retention
1–20 snapshots. Packages are uncompressed. Measure the full archive/spool volume
on a same-version authorized replica first. If the complete state exceeds the
limit, stop rollout and arrange a capacity implementation; never omit historical
dependencies to fit. Commit fixes file inodes; background packaging/hashing does
not hold the application lock. A busy producer skips a cycle. Only complete
immutable packages appear through ABCI ListSnapshots.

Offer checks format, chain, height, limits and the expected app hash supplied by
CometBFT's light client. Apply requires ordered, complete, checksum-valid chunks,
loads all state/archive/spool dependencies and recomputes AppHash. Dependencies
are installed before the state file's atomic rename exposes the restored height.
Errors, duplicate/corrupt chunks and interrupted restores fail closed and clean
temporary files. Nonzero application height cannot be overwritten.

## Authenticate and publish real anchors

Obtain original CometBFT chain genesis bytes from independently authenticated
ceremony custody. Compare chain_id, genesis_time, initial_height and the initial
validator addresses/public keys/power. Model genesis.pb and staging
ledger-genesis.json are different artifacts and cannot substitute for this file.
Publish an immutable HTTPS download; calculate SHA256 over the **original bytes**:

```sh
sha256sum "$CEREMONY_GENESIS"
```

RPC `/genesis` is a JSON wrapper; reserialization changes the byte hash. Publish
the independently checked original hash and URL in network.json's
`bootstrap.genesis_sha256` and `bootstrap.genesis_url`.

Choose a recent trusted height from a trusted local node/light client. Verify
chain ID, >2/3 voting-power commit and validator-set continuity against an
independently trusted checkpoint. Obtain witnesses from at least two independent
RPC operators, but do not mistake matching JSON for the initial trust anchor.
Publish these fields using network-bootstrap.schema.json:

- `bootstrap.status=ready` only after all real fields are verified.
- `bootstrap.statesync.trust_height` and `trust_hash` (the trusted **header hash**).
- `trust_period`, `trusted_header_time`, `expires_at` and `rpc_servers` under
  `bootstrap.statesync`.
- `bootstrap.snapshot_peers`: publicly reachable node-id@host:port suppliers.

Pending publication uses null anchors and empty lists and must remain explicitly
unavailable. Do not replace placeholders with guessed values. Set the trust
period within the chain's actual safety/unbonding window, refresh before half
that period elapses, and revoke anchors when suppliers fail or witnesses
withdraw. Retain immutable public witness receipts. The consumer checks time,
chain ID and header hash before sync, then uses CometBFT verification.

Snapshot state at H is committed by **H+1 header.AppHash**. H+1/H+2 headers,
validators and consensus parameters must remain available. Header hash, app hash
and snapshot checksum are three distinct values.

## Enable a donor and synchronize a new supplier

On the authorized donor, enable only snapshot production:

```toml
[snapshots]
interval = 1000
keep_recent = 2
```

These are examples, not universal production sizing. Retention time
`interval × keep_recent × block_time` must comfortably exceed discovery, light
verification and full download time. An extremely short window removes snapshots
before consumers fetch them. Check completed snapshot.json metadata, chain ID,
height and package checksum; independently verify its app hash using H+1.

On a new machine, install a verified matching release and follow
[Ledger guide](VALIDATOR_GUIDE.md) to initialize a new home with independent keys,
install the exact verified chain genesis and write the explicit `[statesync]`
settings from the published bootstrap object. Distinct RPC URLs are necessary;
operator independence still needs human authentication. Public P2P peers must
include a reachable donor, since RPC alone cannot transport snapshots.

Enable consumption with `enable=true`, at least two `rpc_servers`, verified
`trust_height`, `trust_hash` and `trust_period`. Optional supported settings are
`discovery_time`, `chunk_request_timeout`, `chunk_fetchers` and `temp_dir`.
Do not copy a donor home, consensus key or signing state. `init-validator`
generates local independent keys; its name does not grant voting membership.
Start with explicit public peers, for example:

```sh
emberchain start --home "$NEW_HOME" --p2p tcp://0.0.0.0:26656 \
  --rpc tcp://127.0.0.1:26657 --peers "$VERIFIED_SNAPSHOT_PEERS"
```

Observe successful snapshot installation, rising status height and eventually
catching_up=false. Compare same-height header.AppHash against an independent
verified RPC for at least 20 subsequent blocks. Exercise historical archive
reads and committed spool chunk reads, not just RPC status. Only then enable
production on this dedicated observer and verify its own snapshots against H+1
before advertising it as a public supplier. Consumption may be disabled at its
next maintenance; production is a separate switch. No validator admission
transaction is involved.

## Public methods, validation and rollback

The relay's read-only allowlist covers status, commit, block, header, validators,
consensus_params and genesis/genesis_chunked. Do not expose signer, administration
or debug interfaces to provide state sync. Production RPC checks do not require
SSH or changes to production balances/registrations.

Local tests in services/chain/snapshots_test.go and
cmd/emberchain/statesync_test.go cover real loopback P2P state sync, comparison
against an application with production disabled, corrupted/wrong-hash rejection,
follow-on blocks and historical dependency access. Run full package tests and
the relay tests before rollout. Local acceptance does not authenticate production
anchors or prove that production's full dependency set fits current limits.

On failure, stop the new observer, retain diagnostics and recreate its disposable
home with new independent keys after resolving the cause. Never roll back a
voting validator's signing high-water state. Disabling snapshot production only
changes local transport availability; it does not rewrite application state.
Revoke unavailable public checkpoints and publish new authenticated ones.
