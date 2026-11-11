# Quickstart

Ember is experimental research software. Read the [disclaimer](DISCLAIMER.md).
A public network is forthcoming; this release includes no access endpoints.

## Verify and install

Verify the complete v0.1.0 release set using the root and commands in
[SIGNING.md](SIGNING.md). Linux amd64 and arm64 archives are available.
OpenSSL with Ed25519 support, tar and sha256sum are required.

```sh
tar -xzf ember-0.1.0-linux-arm64.tar.gz
cd ember-0.1.0-linux-arm64
./install-release.sh --role node --prefix "$HOME/ember" --python python3.12
```

Use the amd64 archive on amd64. Node installation needs Python 3.12 with venv
and ensurepip and downloads pinned dependencies. Installation needs no root.
Validator and gateway roles use the same installer with their matching role.

## Configure a network

Model weights are separate. Obtain a signed model manifest, genesis, and model
revision from the network you choose to operate. Verify model files against the
manifest before serving. Set the fields in the installed `config/node.env`:
seed address, certificate fingerprint, scheduler account, chain RPC, ledger
chain ID, model chain, artifact hashes, and local model path.
See [network operation](PUBLIC_NETWORK.md), [validators](VALIDATOR_GUIDE.md),
and [genesis ceremony](GENESIS_CEREMONY.md).

Private keys and configuration files must be mode 0600, and private directories
mode 0700. Keep node health endpoints on loopback. Once your network has funded
and authorized the account, run:

```sh
"$HOME/ember/bin/ember-node" id --state-dir "$HOME/ember/var/node"
"$HOME/ember/bin/ember-role" node "$HOME/ember/config/node.env"
curl -fsS http://127.0.0.1:19192/readyz
```

## Exit and recover stake

Stop serving and automatic restarts; keep the node identity. Use the RPC and
chain ID verified for your network:

```sh
"$HOME/ember/bin/ember-node" exit --state-dir "$HOME/ember/var/node" \
  --chain-rpc "$RPC" --chain-cli "$HOME/ember/bin/emberchain" \
  --ledger-chain-id "$CHAIN_ID" --action status
```

Wait for registration expiry and outstanding holds to finish or expire, then use
`--action unbond`. Wait for the finalized release height and use `--action claim`.
Registration and unbonding waits are measured in blocks. Check status again to
confirm the stake returned to the balance. Testnet Spark has no monetary value.
