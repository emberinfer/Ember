# Ledger node guide

The live chain is `ember-testnet-1`, with five Ledger nodes and AppVersion 12
activated at height 116582. Serving-node registration does not by itself grant
consensus membership. Follow [Ledger membership](VALIDATORS.md) for admission,
readiness proofs and coordinated set changes; follow [Upgrade](UPGRADE.md) for
the activation ladder. Do not create a new genesis to join an existing chain.

For a new network ceremony only, initialize an isolated home and export public
material with the existing CLI names:

```sh
emberchain init-validator --home "$HOME/.ember/ledger"
emberchain validator-public --home "$HOME/.ember/ledger" \
  --name operator-ledger --out ledger.public.json
```

Exchange only the public descriptor. The assembler and every participant verify
the complete descriptor set and app state as described in
[Genesis ceremony](GENESIS_CEREMONY.md). Install only the agreed genesis. The
`ember-role validator` CLI selects the Ledger role; keep the existing command
identifier when configuring scripts.

Keep consensus signing keys separate from account/governor keys. Protect private
files with mode 0600 and state directories with mode 0700. Back up the full home
and application archive consistently. Never run two live machines with the same
consensus signing state or restore its high-water mark backward. Monitor RPC
`/status`, peers, height, vote participation and metrics.
