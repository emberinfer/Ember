# Changelog

## v0.2.0

Encrypted request envelopes, assignment-gated audit key release, privacy-aware
settlement and migration; bounded evidence chunk transport; future-block audit
assignments, misjudgment safeguards, auditor work limits and replay compensation.
Consensus AppVersion is 4; operators must review [upgrade guidance](docs/UPGRADE.md).

The public testnet is open for external testing. Testnet Spark is valueless
test credit: it is not money, is not sold, and carries over to no other network.
All validators currently run by a single operator. There is no third-party audit.
Privacy is Phase 0: relays see no plaintext; executing nodes and assigned auditors
can see plaintext. Do not send sensitive data.

No live endpoint snapshot is included in the source or release artifacts.

## v0.1.0

Initial public release: inference serving, signed execution evidence,
probabilistic auditing, Spark accounting, model profiles and release tooling.
