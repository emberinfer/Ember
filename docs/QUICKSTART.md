# Quickstart

Read the [disclaimer](https://github.com/emberinfer/Ember/blob/master/docs/DISCLAIMER.md). Testnet Spark is valueless test credit.

## Verify a release

The latest software release is on the [releases page](https://github.com/emberinfer/Ember/releases/latest). Assets are the Linux amd64 and arm64 archives, the engine SPI kit archive, `SHA256SUMS` and `release-signing.pub`. Set `EMBER_VERSION` below to the version shown there, without the leading `v`.

**Recent releases have no published `SHA256SUMS.sig`.** Check the asset list of the release you downloaded: if it has no detached signature, checksum comparison detects mismatched downloaded bytes but does not authenticate unsigned checksums.

Measured SHA-256 of the release key's DER SubjectPublicKeyInfo:

`d3b07085781f2d4d75de6caefa6e1dc9cd4eadeec10326b2b0c20eb03254eabb`.

Authenticate the fingerprint through a trusted channel. The downloaded key alone is not a trust anchor. The repository trust statement names an older key; its detached signature fails verification against the checked-in root key and current statement bytes. See [signing](SIGNING.md).

```sh
# Take <version> from the releases page above, e.g. EMBER_VERSION=0.9.2
EMBER_VERSION=<version>
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

Use Linux amd64 or arm64, Python 3.12 with venv/ensurepip, OpenSSL with Ed25519, `tar`, `sha256sum` and `curl` for this CPU installation path. Check runtime and hardware against the selected signed profile; other engines need their own compatible dependencies. Installation does not supply model weights.

## Install a role

The command above installs the node role under the selected prefix. The installer also accepts `--role gateway` and `--role validator`; the latter is the CLI name for a Ledger node. See the [Ledger node guide](VALIDATOR_GUIDE.md).

Obtain current signed profile artifacts, their chain anchors, model origin and immutable revision through an authenticated deployment channel. Verify local weights against the manifest before serving. The repository's [network parameter file](https://github.com/emberinfer/Ember/blob/master/site/network.json) contains synchronized public seed, RPC, scheduler and profile anchors for configure. Verify these values against the current public network before use, and supply the matching signed profile artifacts to the installation.

The configuration helper consumes a complete parameter file and refuses existing config overwrite:

```sh
"$HOME/ember/bin/ember-node" configure \
  --network-parameters "$NETWORK_PARAMETERS" \
  --model-path "$MODEL_PATH" \
  --config "$HOME/ember/config/join.env"
"$HOME/ember/bin/ember-node" id --state-dir "$HOME/ember/var/node"
```

## Profile anchors: genesis hash, manifest hash and scheme hash

A profile is pinned by three hashes, and a node must present all of them
correctly for its registration to be accepted and routed:

- **Genesis hash** and **manifest hash** — the SHA-256 of the profile's
  signed `genesis.pb` and `manifest.pb` files. They are published in
  `network.json` as `current_profile.genesis_sha256` and
  `current_profile.manifest_sha256`, and the configure helper above
  checks your bundled artifacts against them.
- **Scheme hash** — one per layout scheme declared in the manifest
  (the manifest's `schemes` list). If the manifest declares any scheme,
  joining requires pinning exactly one of them: pass
  `ember-node join --scheme-hash <64 hex characters>` (or set
  `EMBER_SCHEME_HASH` in the node config the role launcher reads).
  Without a pinned scheme hash — or with one that matches no declared
  scheme — the client stops with `missing or unknown scheme hash`,
  because the node's signed resource profile and every execution
  receipt it produces carry the scheme hash, and gateways check
  receipts against it. Profiles whose manifest declares no schemes do
  not need one.

For the current public testnet profile
`ember-qwen2.5-1.5b-unified-v1` the manifest declares exactly one
scheme, and its scheme hash is:

`400120b65600b51587238ea514a711bb57c68d267194e6ed400a1e2b64a7a142`

Where to get and check this value:

- The live network parameter file, https://emberinfer.org/network.json,
  carries it as `current_profile.scheme_hash`, next to the genesis and
  manifest hashes above. Treat the three values as one set: never mix
  a scheme hash from one profile (or one profile revision) with the
  genesis or manifest of another.
- You can recompute it from the manifest itself. The scheme hash is
  `sha256("ember-layout-v1\0" || deterministic protobuf serialization
  of the LayoutScheme message)`, exactly as implemented in
  `layout_identity.py` in the release (`lib/ember/node/`). First verify
  the manifest file's SHA-256 equals the published manifest hash, so
  the scheme you hash is the anchored one.

## Local trust policy

A node does not blindly trust the profile material a gateway serves
during the join handshake. Before it registers, it reads the profile's
publication record from the chain and checks it against a local trust
policy: a JSON file pinning which accounts may publish (or endorse)
each profile. If no rule in the file accepts the profile's actual
publisher, the node refuses to become ready -- the log shows
`profile does not satisfy local publisher/endorsement policy` and the
health endpoint stays at 503. Gateways distribute the bytes, but only
publishers you pinned can make your node serve them.

The format (implemented in `lib/ember/node/agent/trust.py` in the
release) is:

```json
{"profiles": {"<profile-id>": {"publishers": ["<publisher account id>"]}}}
```

A rule may instead, or additionally, accept endorsements through
`trusted_endorsers` and `min_endorsers`. The release bundles a
ready-made example as `config/trust-policy.json` in the installation,
and the configure helper above writes its path into the generated node
config as `EMBER_PROFILE_TRUST_POLICY`. For the current public testnet
profile it contains:

```json
{"profiles": {"ember-qwen2.5-1.5b-unified-v1": {"publishers": ["c0058cba3b9014581b4f88679d24f3fc1c524e33668c4974fadfebf3bb29307d"]}}}
```

Verify the publisher yourself before relying on the example: query the
public chain RPC (the `rpc` address in `network.json`) for the profile
record and read `publication.publisher`:

```sh
"$HOME/ember/bin/emberchain" query --rpc http://relay.emberinfer.org:39657 \
  --path /model/ember-qwen2.5-1.5b-unified-v1
```

The full state at path `/state` carries the same record under
`models["ember-qwen2.5-1.5b-unified-v1"].publication.publisher`. The
value must equal the pinned publisher above; the record's
`registered_height` (71510 for this publication) identifies which
publication you are looking at.

If you run `ember-node join` directly instead of through the launcher,
pass `--trust-policy <file>`. The `ember-role node` launcher forwards
`EMBER_PROFILE_TRUST_POLICY` from the node config as `--trust-policy`
automatically, just as it forwards `EMBER_SCHEME_HASH` as
`--scheme-hash`.

Fund that account using the deployment's verified funding procedure. An enabled faucet supplies rate-limited valueless credit; this document does not assert a current public faucet address. Wait for finalized funding before enrollment.

Then start and check the loopback health listener:

```sh
"$HOME/ember/bin/ember-role" node "$HOME/ember/config/join.env"
# In another terminal, if using the default node health address:
curl -fsS http://127.0.0.1:19192/readyz
```

Since AppVersion 10, registration offers both execution and on-chain audit eligibility by default. `--no-exec` and `--no-audit` select registration-time opt-outs. Eligibility still requires the appropriate evidence and replay capabilities. Registration binds the identity, exact signed profile, layer range and capacity. There is no deregistration or mid-term removal of an enabled role. Registrations lapse after the live 200,000-block TTL; stopping a process does not erase its registration.

Keep identity keys mode 0600, private directories mode 0700, and health listeners and local transport bridges on loopback. See [contribution limits](https://github.com/emberinfer/Ember/blob/master/docs/CONTRIBUTION_CAPS.md).

## Exit and recover stake

Stop the node and disable automatic restart/renewal. Keep its identity key.

Set `RPC` to your verified chain RPC and use the live ledger chain ID:

```sh
"$HOME/ember/bin/ember-node" exit --state-dir "$HOME/ember/var/node" \
  --chain-rpc "$RPC" --chain-cli "$HOME/ember/bin/emberchain" \
  --ledger-chain-id ember-testnet-1 --action status
```

Wait for `registration_blocks_remaining` to reach zero and for outstanding holds to finish or expire. Repeat with `--action unbond`, then wait for the returned `release_height` and repeat with `--action claim`. The health-listener check must use the node's actual port (`--health-url` if changed). Registration expiry, pending work exposure and unbonding delay are separate constraints. Recoverable stake is the amount remaining after any finalized penalties. Serving payments remain in the account; unbonding creates no new credit.
