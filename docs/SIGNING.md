# Release Signing

Release maintainers use the release key to sign approved testnet releases,
including v0.22.0. A root trust anchor was established on 2026-11-11 (see Trust anchor below).


## Trust anchor

Root key fingerprint (SHA-256 of the DER-encoded Ed25519 public key):
`8f93347f50a04c2d3b420ebde2d7cd73fc792386b18c20e5a0f60f6d47706e09`

The root public key is `release/ember-root.pub`. It endorses, in
`release/trust-statement.txt` (detached signature
`release/trust-statement.sig`), the current release signing key:

Release key fingerprint:
`35091a3aadf80815b47494439db6a0ee13f5a78eecc5bc516772dddefdd6a092`

Verify the full chain before trusting a release:

```sh
# 1. the root endorses this release key
openssl pkeyutl -verify -pubin -inkey release/ember-root.pub -rawin \
  -in release/trust-statement.txt -sigfile release/trust-statement.sig
# 2. the release public key from the downloaded release set matches
openssl pkey -pubin -in release-signing.pub -outform DER | sha256sum
# 3. the release key signed the checksums
openssl pkeyutl -verify -pubin -inkey release-signing.pub -rawin \
  -in SHA256SUMS -sigfile SHA256SUMS.sig
```

A release-key rotation is valid only when a new trust statement signed
by the root key lists both the old and the new release key fingerprints.

## Custody

- Use a dedicated Ed25519 private key only for Ember release checksums.
- Keep the live key in a mode-0700 operator directory on the designated signing
  host; the private key file is mode 0600 and is never committed or copied into a
  release archive.
- Keep a separately protected encrypted backup and record its SHA-256 digest,
  creation date, custodian, and restore test. Do not store its passphrase beside
  the backup.
- Record the public-key SHA-256 fingerprint with the release verification records
  and publish it through the project's established trust anchor.

## Signing procedure

`release/build.sh` requires `EMBER_RELEASE_SIGNING_KEY` and writes the public
key and detached signature beside the archives and `SHA256SUMS`. An explicit
`EMBER_RELEASE_UNSIGNED=1` permits build regression only; distribute no unsigned
set. A single-architecture download uses `sha256sum --ignore-missing -c
SHA256SUMS` while retaining and verifying the signature over the complete sums.

Build twice from the reviewed local tag, compare archive hashes, then sign only
the final `SHA256SUMS`:

```sh
release/reproducible-check.sh vX.Y.Z-rc.N /tmp/ember-release-check
release/sign.sh /tmp/ember-release-check /secure/ember/release-signing.pem
openssl pkey -pubin -in /tmp/ember-release-check/release-signing.pub \
  -outform DER | sha256sum
```

Verify on at least one separate operator host before installation. The signed
set is indivisible: archive, checksums, signature, and public key must come from
the same retained directory.

## Rotation and compromise

Rotate on suspected disclosure, custodian change, cryptoperiod expiry, or failed
backup verification. Stop releases, preserve evidence, create a new dedicated
key, and produce a transition statement containing both old and new public-key
fingerprints. Maintainers must verify the transition against the established root trust
anchor before distributing the new key. Never use a compromised old key to assert that its replacement is safe.
