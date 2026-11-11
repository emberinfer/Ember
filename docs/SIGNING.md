# Release signing

Ember v0.1.0 uses an Ed25519 root to certify a separate release signing key.
The root public key is `release/ember-root.pub`. Its fingerprint is SHA-256
of the DER SubjectPublicKeyInfo:

`cc3522f6bbfa1ca057cd5beddfefb740a0b6ef86d81d4c30307f613bc21bd918`

The certified release key fingerprint is:

`d3b07085781f2d4d75de6caefa6e1dc9cd4eadeec10326b2b0c20eb03254eabb`

Authenticate this root fingerprint through a trusted channel before installing.
A key downloaded beside an archive alone does not establish trust.

## Verify from a clean checkout

Keep the archive, `SHA256SUMS`, `SHA256SUMS.sig`, and `release-signing.pub`
from the same release set. From a checkout of v0.1.0 run:

```sh
release/verify.sh /path/to/release-set
```

The verifier checks the root signature on the trust statement, root fingerprint,
certified release-key fingerprint, signature on checksums, and every listed
archive. For one architecture, the same verifier accepts absent archives but
requires at least one verified archive:

```sh
EMBER_VERIFY_PARTIAL=1 release/verify.sh /path/to/release-set
```

## Build and sign

Use dedicated private keys held outside the checkout in a mode-0700 directory;
private key files must be mode 0600. Never include them in source or archives.

```sh
release/reproducible-check.sh v0.1.0 /tmp/ember-release-check
release/sign.sh /tmp/ember-release-check /secure/ember/release.pem
release/verify.sh /tmp/ember-release-check
```

The reproducibility command builds both Linux architectures twice from the tag
and compares archive hashes. Release archives include no public network snapshot.
`EMBER_RELEASE_UNSIGNED=1` is for build checks only. Distribute signed sets.

## Rotation

A release-key change requires a new root-signed trust statement identifying the
new fingerprint and its scope. If the root is compromised, stop releases and
establish a replacement through an independently authenticated channel.
Keep separately protected backups and test restoration without publishing keys.
