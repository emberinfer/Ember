# Release signing and trust anchors

The exact source snapshot for each software release is recorded in its build metadata.
Releases starting with v0.10.0 publish `SHA256SUMS.sig`, `SHA256SUMS` and
`release-signing.pub` alongside the archives. Follow the verification procedure in
the [quickstart](QUICKSTART.md).

**v0.7.0 and v0.6.0 have no published `SHA256SUMS.sig`.** The signing key is offline. Checksum comparison detects mismatched downloaded bytes but does not authenticate unsigned checksums.

Measured SHA-256 of the release key's DER SubjectPublicKeyInfo:
`d3b07085781f2d4d75de6caefa6e1dc9cd4eadeec10326b2b0c20eb03254eabb`.


## Measured keys

The repository root key at `release/ember-root.pub` measures:
`8f93347f50a04c2d3b420ebde2d7cd73fc792386b18c20e5a0f60f6d47706e09` (SHA-256 of DER SubjectPublicKeyInfo).

The v0.6.0 asset `release-signing.pub` measures:
`d3b07085781f2d4d75de6caefa6e1dc9cd4eadeec10326b2b0c20eb03254eabb` using the same encoding.

```sh
openssl pkey -pubin -in release-signing.pub -outform DER | sha256sum
openssl pkey -pubin -in release/ember-root.pub -outform DER | sha256sum
```

The checked-in `release/trust-statement.txt` names a different, older release
key. Verification of its detached `release/trust-statement.sig` with the
repository root public key fails against the current checked-in statement bytes.
Neither this failed check nor the statement text authenticates the v0.6.0 key.
Authenticate the current release fingerprint through a trusted channel; a bundled
public key is not proof of authenticity. A root-signed rotation statement covering
the measured current key needs maintainer publication.

## v0.7.0 download checks

Download the archive and `SHA256SUMS` from the same release directory and run
`sha256sum --ignore-missing -c SHA256SUMS`. At least one intended archive must be
present and pass. This checks consistency with downloaded sums, not a detached
signature. Do not run a command that expects `SHA256SUMS.sig` for v0.7.0 or v0.6.0.

## Release tooling and custody

`release/build.sh` and `release/sign.sh` provide release signing tooling; tooling
support does not mean a particular release published a signature. Keep private
signing material outside the repository, protect it with restrictive permissions
and offline backup, and compare reproducible builds before publication. A key
rotation should name old and new fingerprints in a statement signed by the
established root. Preserve the statement and verification records with the
release. Do not use a compromised key as the sole authority for its replacement.
