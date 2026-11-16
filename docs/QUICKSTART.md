# Quickstart

Before participating, read the [disclaimer](DISCLAIMER.md): Ember is experimental
research software, and testnet Spark is valueless test credit.

> **Current network:** the live public testnet is `ember-testnet-1`
> (application version 9) at [emberinfer.org](https://emberinfer.org/), with
> step-by-step operator guides at
> [emberinfer.org/start](https://emberinfer.org/start) that track its signed
> network parameters and the current code in this repository. The
> signed-release flow below is the v0.22.0 flow for the first public network,
> retained unchanged for that release's reproducibility.

Prerequisites: Linux amd64 or arm64, **Python 3.12 with venv/ensurepip support**
(often packaged as `python3.12` plus `python3.12-venv`), OpenSSL with Ed25519,
`tar`, `sha256sum` and `curl`. Ask your host administrator or use your package
manager to provide these before installation. The installer needs no root.
Python 3.13 and newer are not compatible with these pinned runtime profiles.

On a stock Colab VM, choose a CPU runtime. Python 3.12 may already be available
as `python3.12` while the notebook kernel uses another version. If its venv
support is missing, provision the prerequisite with
`apt-get update && apt-get install -y python3.12-venv` before installing Ember.
This is host preparation; the Ember installer itself needs no root.

## Verify a release

Download the archive for your architecture, `SHA256SUMS`, `SHA256SUMS.sig`,
`release-signing.pub` and `network.json` from the
[v0.22.0 release](https://github.com/emberinfer/Ember/releases/tag/v0.22.0).
The published network artifact records the signing public-key SHA-256 fingerprint
(over DER SubjectPublicKeyInfo). Compare it before verification:

```sh
openssl pkey -pubin -in release-signing.pub -outform DER | sha256sum
```

Use the archive, `SHA256SUMS`, `SHA256SUMS.sig` and `release-signing.pub` from the
same release directory. A build without the two signature files is not a
release set. Authenticate the signing-key fingerprint through your trusted
release channel before relying on the bundled public key:


```sh
EMBER_VERSION=0.22.0
# Check the downloaded architecture; absent archives for other architectures
# are optional. At least one listed file must be present and verify.
sha256sum --ignore-missing -c SHA256SUMS
openssl pkeyutl -verify -pubin -inkey release-signing.pub \
  -rawin -in SHA256SUMS -sigfile SHA256SUMS.sig
tar -xzf "ember-$EMBER_VERSION-linux-amd64.tar.gz"
cd "ember-$EMBER_VERSION-linux-amd64"
```

## Install a role

No root access is required. The installer writes only below `--prefix`.

```sh
./install-release.sh --role node --prefix "$HOME/ember" --python python3.12
```

The installer selects the release architecture's CPU runtime requirements.
On amd64 this is `torch==2.5.1+cpu` from the PyTorch CPU index; on arm64 it
is the upstream CPU wheel named `2.5.1` (upstream supplies no `+cpu` suffix
for that architecture). CUDA profiles require separate measured requirements;
changing the wheel or device changes the signed runtime fingerprint.

The public testnet is live. Download the model below, use the signed network
artifact and `configure` command in Network parameters to create `join.env`,
then generate your identity and claim public faucet credit before starting.
Registration stakes the on-chain minimum. Validator set changes run through
the chain's on-chain governance process and the current set is public
on-chain; there is no third-party security audit, and testnet Spark
is valueless test credit: no price, no exchange, no returns. External CPU
serving is slow for long requests; start with short requests.

```sh
"$HOME/ember/bin/ember-node" id --state-dir "$HOME/ember/var/node"
```

The node dials the seed outbound. Do not expose its health endpoint or local SOCKS
bridges beyond loopback. Model weights are not bundled in the release. After installation, fetch the
model revision published in the verified network snapshot:

```sh
"$HOME/ember/venv/bin/python" - <<'PY'
import json, os
from huggingface_hub import snapshot_download
root = os.path.expanduser("~/ember")
profile = json.load(open(root + "/network.json"))["current_profile"]
snapshot_download(profile["model"], revision=profile["model_revision"],
                  local_dir=os.path.expanduser("~/models/Qwen2.5-1.5B-Instruct"),
                  allow_patterns=["*.json", "*.txt", "*.safetensors"])
PY
```

Join verifies the downloaded files against the signed manifest before serving.

Validator and gateway roles use the same installer with `--role validator` or
`--role gateway`, then their matching templates. Private keys and auth files must
be mode 0600; state/config directories must be mode 0700.

The optional `/testnet/faucet` endpoint issues rate-limited, stake-free,
valueless test credit only. It is disabled unless the gateway operator configures
a dedicated faucet account key, amount, window, and persistent mode-0600 state.

## Network parameters

The public home is https://emberinfer.org/ and its machine-readable
[network.json](https://emberinfer.org/network.json). The identical
snapshot ships as `network.json` in the signed release set and archive. It
contains the seed, certificate fingerprint, scheduler account, RPC, ledger chain
ID and bundled genesis/manifest hashes. Null access fields mean the public
network has not launched; do not replace them with private operator coordinates.

Use the signed release snapshot to generate a new config:

```sh
"$HOME/ember/bin/ember-node" configure \
  --network-parameters "$HOME/ember/network.json" \
  --model-path "$HOME/models/Qwen2.5-1.5B-Instruct" \
  --config "$HOME/ember/config/join.env"
```

The helper verifies profile hashes against the bundle and refuses incomplete
parameters or overwriting an existing config. The operator supplies only the
local model path; no operator's private node config is needed. A newer website
snapshot requires a matching signed release and verification before use.

Registration TTL is measured in finalized blocks, with a default of 200000,
capped by `registration_ttl_blocks` in `/parameters`. It is a duration added to
the observed height at registration, not a wall-clock deadline. Reconnection
can renew it. Heartbeat TTL is separate. For a short disposable staging trial, set
`EMBER_REGISTRATION_TTL=300` in the generated config before its first join;
confirm the chain permits that duration. Set `EMBER_AUDIT_ROLE=true` before
first join to offer replay as well as execution. Removing that role waits
for registration expiry. Stopping a process does not cancel
its on-chain registration; budget for the remaining TTL when planning exit.

The signed public testnet artifact supplies a faucet URL.
Generate your identity and self-fund it before starting the node:

```sh
ACCOUNT=$("$HOME/ember/bin/ember-node" id --state-dir "$HOME/ember/var/node")
FAUCET_URL=$(python3.12 -c 'import json,sys; print(json.load(open(sys.argv[1]))["access"]["faucet"])' "$HOME/ember/network.json")
curl -fsS "$FAUCET_URL" -H 'Content-Type: application/json' \
  -d "{\"account\":\"$ACCOUNT\"}"
```

Wait for the funding transaction to finalize before joining. Claims are limited
per account and source; `429` includes `Retry-After`. No gateway bearer token
or operator transfer is required for this funding path.

After funding finalizes, start serving in the foreground. This stakes the
minimum and registers the node; check readiness from another terminal:

```sh
"$HOME/ember/bin/ember-role" node "$HOME/ember/config/join.env"
```

```sh
curl -fsS http://127.0.0.1:19192/readyz
```

## Exit and recover stake

Stop the node with Ctrl-C (or SIGTERM for your service) and disable automatic
restart so it stops serving and renewing registration. Keep `identity.key`;
only its owner can recover the stake. Use the RPC and chain ID from your
verified network artifact:

```sh
RPC=$(python3.12 -c 'import json,sys; print(json.load(open(sys.argv[1]))["access"]["rpc"])' "$HOME/ember/network.json")
CHAIN_ID=$(python3.12 -c 'import json,sys; print(json.load(open(sys.argv[1]))["ledger"]["chain_id"])' "$HOME/ember/network.json")
"$HOME/ember/bin/ember-node" exit --state-dir "$HOME/ember/var/node" \
  --chain-rpc "$RPC" --chain-cli "$HOME/ember/bin/emberchain" \
  --ledger-chain-id "$CHAIN_ID" --action status
```

Wait until `registration_blocks_remaining` reaches zero and outstanding holds
finish or expire. Run the same command with `--action unbond` to request the
full remaining stake. It checks that the local node health listener has stopped;
if you changed its port, supply the matching `--health-url`. The chain also
rejects stake that remains exposed to outstanding work. Query `--action status`
again for each unbonding's `release_height`. Once that height is finalized, run
with `--action claim`, then check status to confirm stake returned to balance.
The registration wait and `unbonding_blocks` are separate waits measured in
blocks. Serving earnings remain in the account; unbonding creates no new credit.
