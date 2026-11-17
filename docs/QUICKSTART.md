# Quickstart

Read the [disclaimer](DISCLAIMER.md). Testnet Spark is valueless test credit.

## Verify a release

The latest software release is [v0.7.0](https://github.com/emberinfer/Ember/releases/tag/v0.7.0), whose tag points to snapshot `b8b3128`. Assets are the Linux amd64 and arm64 archives, `ember-engine-spi-kit-0.7.0.tar.gz`, `SHA256SUMS` and `release-signing.pub`.

**v0.7.0 has no published `SHA256SUMS.sig`.** The signing key is offline. Checksum comparison detects mismatched downloaded bytes but does not authenticate unsigned checksums.

Measured SHA-256 of the release key's DER SubjectPublicKeyInfo:
`d3b07085781f2d4d75de6caefa6e1dc9cd4eadeec10326b2b0c20eb03254eabb`.


Authenticate the fingerprint through a trusted channel. The downloaded key alone
is not a trust anchor. The repository trust statement names an older key; its detached signature fails
verification against the checked-in root key and current statement bytes. See [signing](SIGNING.md).

```sh
EMBER_VERSION=0.7.0
EMBER_RELEASE="https://github.com/emberinfer/Ember/releases/download/v$EMBER_VERSION"
# Use arm64 instead of amd64 for ARM64.
for file in "ember-$EMBER_VERSION-linux-amd64.tar.gz" SHA256SUMS release-signing.pub; do
  curl -fLO "$EMBER_RELEASE/$file"
done
openssl pkey -pubin -in release-signing.pub -outform DER | sha256sum
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf "ember-$EMBER_VERSION-linux-amd64.tar.gz"
cd "ember-$EMBER_VERSION-linux-amd64"
./install-release.sh --role node --prefix "$HOME/ember" --python python3.12
```

Use Linux amd64 or arm64, Python 3.12 with venv/ensurepip, OpenSSL with Ed25519,
`tar`, `sha256sum` and `curl` for this CPU installation path. Check runtime and
hardware against the selected signed profile; other engines need their own
compatible dependencies. Installation does not supply model weights.

## Install a role

The command above installs the node role under the selected prefix. The installer
also accepts `--role gateway` and `--role validator`; the latter is the CLI name
for a Ledger node. See the [Ledger node guide](VALIDATOR_GUIDE.md).

Obtain current signed profile artifacts, their chain anchors, model origin and
immutable revision through an authenticated deployment channel. Verify local
weights against the manifest before serving. The repository's [network parameter file](../site/network.json) contains synchronized
public seed, RPC, scheduler and profile anchors for configure. Verify these values
against the current public network before use, and supply the matching signed
profile artifacts to the installation.

The configuration helper consumes a complete parameter file and refuses existing
config overwrite:

```sh
"$HOME/ember/bin/ember-node" configure \
  --network-parameters "$NETWORK_PARAMETERS" \
  --model-path "$MODEL_PATH" \
  --config "$HOME/ember/config/join.env"
"$HOME/ember/bin/ember-node" id --state-dir "$HOME/ember/var/node"
```

Fund that account using the deployment's verified funding procedure. An enabled
faucet supplies rate-limited valueless credit; this document does not assert a
current public faucet address. Wait for finalized funding before enrollment.
Then start and check the loopback health listener:

```sh
"$HOME/ember/bin/ember-role" node "$HOME/ember/config/join.env"
# In another terminal, if using the default node health address:
curl -fsS http://127.0.0.1:19192/readyz
```

Since AppVersion 10, registration offers both execution and on-chain audit eligibility by default. `--no-exec` and `--no-audit` select registration-time opt-outs. Eligibility still requires the appropriate evidence and replay capabilities. Registration binds the identity, exact signed profile, layer range and capacity. There is no deregistration or mid-term removal of an enabled role. Registrations lapse after the live 200,000-block TTL; stopping a process does not erase its registration.

Keep identity keys mode 0600, private directories mode 0700, and health listeners
and local transport bridges on loopback. See [contribution limits](CONTRIBUTION_CAPS.md).

## Exit and recover stake

Stop the node and disable automatic restart/renewal. Keep its identity key.
Set `RPC` to your verified chain RPC and use the live ledger chain ID:

```sh
"$HOME/ember/bin/ember-node" exit --state-dir "$HOME/ember/var/node" \
  --chain-rpc "$RPC" --chain-cli "$HOME/ember/bin/emberchain" \
  --ledger-chain-id ember-testnet-1 --action status
```

Wait for `registration_blocks_remaining` to reach zero and for outstanding holds
to finish or expire. Repeat with `--action unbond`, then wait for the returned
`release_height` and repeat with `--action claim`. The health-listener check must
use the node's actual port (`--health-url` if changed). Registration expiry,
pending work exposure and unbonding delay are separate constraints. Recoverable
stake is the amount remaining after any finalized penalties. Serving payments
remain in the account; unbonding creates no new credit.
