# Changelog

## Unreleased

- The gateway supervisor passes pre-bound HTTP and tunnel listeners to workers, eliminating the port allocation race during startup.
- Node readiness stays healthy while any configured gateway tunnel has completed JOIN and remains connected.

## 0.10.2 — 2026-11-25

- The gateway supervisor can start serving newly published profiles whose signed sample layers match their verified model manifests.
- Sampled audits of profiles with shapes other than the bundled 1.5B model can replay, reach a verdict, and settle. The execution and audit engines use the sample layers from the signed policy.
- The supervisor catalog shows the latest worker startup or exit failure.
- Generated validator configurations use unquoted state-sync chunk fetcher counts that the parser accepts. Existing quoted counts remain supported.
- `ember-node configure` can use the website's `network.json` directly and skips public gateway entries without membership metadata.
- `ember-node profile verify --conformance` works again for bundled legacy profiles using their exact-ID replay comparator and signed tolerance.
- The publishing guide includes a copyable CPU policy example, weight uniqueness rules, and the chain CLI requirement. The release also bundles the bootstrap and validator membership guides.

## 0.10.1 — 2026-11-25

- Allow snapshot production to exclude historical archive data with `[snapshots] include_archive`, enabling state-sync bootstrap on the live chain when its archive exceeds the snapshot package limit.
- Restored nodes accumulate archive data starting from their sync height.

## 0.10.0 — 2026-11-25

- Distribute signed profile artifacts from the chain and bind JOIN assertions to the selected profile.
- Add the gateway supervisor, dynamic gateway directories, and a same-origin website directory.
- Bootstrap the ledger with ABCI snapshots and CometBFT state sync.
- Publish profiles with one `ember-node profile publish` command.
- Document and package the CUDA installation path and preflight checks for virtual environments.
- Separate ledger synchronization and governance in the operator guides, and synchronize the website guidance.
- Fix page placeholders in public installation and onboarding instructions.

## 0.7.0 — current published release

Latest GitHub release: v0.7.0. Snapshot source `1e6ecfc` (the release commit
adds version metadata to this snapshot; its exact SHA is recorded in the build).
AppVersion 13 activated on `ember-testnet-1` at height **129067**.

- Consensus parameter mechanism: Version >= 13 attaches Block parameters to each
  block; max_bytes increases from 4194304 to 8388608, while max_gas stays 10000000.
- Protocol execution-role exit: `request_exec_exit` takes effect after 1000 blocks,
  gateways filter the on-chain exit set, and clients provide `request-exec-exit`.
- MoE receipt chunking and incremental submission: manifest + 1MiB chunks +
  finalize, spool excluded from consensus state, block-history reconstruction,
  stage gRPC limit 320MiB, and Tier-1 compact transport v2 with signatures still
  over the v1 logical receipt.
- Context-window admission: gateways jointly check input + max_tokens against
  profile ContextLimit, errors include numeric limits, and pricing/quote expose
  context_limit_tokens. Includes the LONG35 HTTP 400 adapter production patch;
  the live 35B service window is 46080 tokens.

Assets: Linux amd64/arm64 archives, Engine SPI kit, SHA256SUMS and
release-signing.pub. SHA256SUMS.sig will be added for v0.7.0 and v0.6.0 when the
offline signing key is available.

## 0.6.0 — previous published release

Latest GitHub release: v0.6.0, snapshot `b8b3128`. The release sequence below
retains older version labels as historical records; do not infer current status
from their numerically larger version strings.

Available assets: Linux amd64 and arm64 archives, Engine SPI kit, `SHA256SUMS`
and `release-signing.pub`. `SHA256SUMS.sig` is not yet published for this release.

## Current network snapshot — 2026-11-18

The live chain is `ember-testnet-1`, with genesis dated 2026-11-14 and five Ledger nodes. Live AppVersion 13 activated at height 129067. The source base constant is 11; the coordinated upgrade ladder advances the running state to 13.

The live catalog includes unified 1.5B and 35B profiles and serving OLMoE layer +
expert sharding. Kimi is published but not deployed. AppVersion 12 corrected its
compute derivation to 208,736,282,112 FLOPs/token (correction count 0 to 1).
AppVersion 10 supplies default execution/audit registration availability and
bounded whole-model adjudication; AppVersion 11 supplies token-price overrides.
This snapshot describes chain state, not a claim that every mechanism first
shipped in v0.7.0. See [pricing](https://github.com/emberinfer/Ember/blob/master/docs/PRICING.md) and [upgrades](docs/UPGRADE.md).

## Historical 0.22.0 - 2026-11-11

- Launch the public testnet with the externally proven rc.3 code, final version
  labels, signed reproducible Linux archives and refreshed network parameters.
- Publish the live public onboarding path: verify, install, self-fund, stake,
  register, serve, then exit and recover stake.
- Retain the established release signing key and document experimental security, valueless test credit and CPU serving limits.

## Historical 0.22.0-rc.3 - 2026-11-11

- Support public-address TLS termination with HTTP/2 ALPN and verified backend
  TLS for the seed relay. Preserve standard gRPC hostname verification when
  the backend certificate covers private addresses.
- Supersede the rc.2 seed fingerprint with the public relay certificate pin.

## Historical 0.22.0-rc.2 - 2026-11-11

- Publish public relay access and signed network parameters for the joinability
  candidate, with isolated seed/RPC/gateway/faucet ports.
- Preserve faucet account and client-source limits behind an explicitly trusted
  proxy; external callers cannot override the relay-observed source address.
- Document cold operator model download, Python prerequisites and public releases.

## Historical 0.21.0-rc.1 - 2026-11-09

- Added a public-material-only multi-operator genesis ceremony and independent
  operator verification commands.
- Added a persistent-rate-limit faucet for valueless test Spark and
  expanded validator, hardware, signing, audit, and hard-fork guidance.

## Historical 0.20.0 - 2026-11-09

- Added CometBFT-backed Spark settlement, public seed discovery, outbound NAT
  serving, signed scheduler channels, unpredictable post-commit audit assignment,
  precise stage attribution and abuse controls.
- Added reproducible Linux amd64/arm64 release archives, signatures, role
  installers, health/metrics endpoints, operations drills and alert rules.
- Added the standalone Engine SPI conformance kit and public-repository operator
  documentation. This note describes the historical release preparation state.

These historical entries do not describe the current chain or release.

## Historical 0.22.0-rc.1

- Resolve replay routes from finalized stake-backed registrations and authenticated
  node tunnels; independent auditors retain custody of their signing keys.
- Distributable builds include the checksum signature and signing public key.
  Single-architecture verification tolerates archives that were not downloaded.
