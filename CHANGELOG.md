# Changelog

## Unreleased

- Add input and context-aware Spark fees committed in signed model profiles,
  serving-tokenizer commitments, exact worst-case itemized gateway quotes,
  and consensus verification at submission and settlement.
- Preserve output-only signed profile/receipt compatibility, exact legacy
  amounts, fee conservation, role allocation and executed-work pipeline splits.
- Document the pricing formula, integer rounding, reserve/refund flow and
  profile rollout in `docs/PRICING.md`.

## 0.22.0 - 2026-11-11

- Launch the public testnet with the externally proven rc.3 code, final version
  labels, signed reproducible Linux archives and refreshed network parameters.
- Publish the live public onboarding path: verify, install, self-fund, stake,
  register, serve, then exit and recover stake.
- Retain the established release signing key and document single-organization
  validators, unaudited security, valueless test credit and slow CPU serving.

## 0.22.0-rc.3 - 2026-11-11

- Support public-address TLS termination with HTTP/2 ALPN and verified backend
  TLS for the seed relay. Preserve standard gRPC hostname verification when
  the backend certificate covers private addresses.
- Supersede the rc.2 seed fingerprint with the public relay certificate pin.

## 0.22.0-rc.2 - 2026-11-11

- Publish public relay access and signed network parameters for the joinability
  candidate, with isolated seed/RPC/gateway/faucet ports.
- Preserve faucet account and client-source limits behind an explicitly trusted
  proxy; external callers cannot override the relay-observed source address.
- Document cold operator model download, Python prerequisites and public releases.

## 0.21.0-rc.1 - 2026-11-09

- Added a public-material-only multi-operator genesis ceremony and independent
  operator verification commands.
- Added a persistent-rate-limit faucet for valueless test Spark and
  expanded validator, hardware, signing, audit, and hard-fork guidance.

## 0.20.0 - 2026-11-09

- Added CometBFT-backed Spark settlement, public seed discovery, outbound NAT
  serving, signed scheduler channels, unpredictable post-commit audit assignment,
  precise stage attribution and abuse controls.
- Added reproducible Linux amd64/arm64 release archives, signatures, role
  installers, health/metrics endpoints, operations drills and alert rules.
- Added the standalone Engine SPI conformance kit and public-repository operator
  documentation. The repository is prepared but is not published.

This is a public-testnet readiness release. It is not a mainnet release.

## 0.22.0-rc.1

- Resolve replay routes from finalized stake-backed registrations and authenticated
  node tunnels; independent auditors retain custody of their signing keys.
- Distributable builds include the checksum signature and signing public key.
  Single-architecture verification tolerates archives that were not downloaded.
