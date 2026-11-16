# Validator Guide

Initialize an offline validator home, export only its public descriptor, and
assemble one reviewed genesis without collecting private validator homes:

```sh
emberchain init-validator --home "$HOME/.ember/validator"
emberchain validator-public --home "$HOME/.ember/validator" \
  --name <operator-validator-name> --out validator.public.json
emberchain genesis --out genesis.json --chain-id ember-<network> \
  --app-state app-state.json --validator-public <comma-separated-public-json>
emberchain verify-genesis --genesis genesis.json --app-state app-state.json \
  --validator-public <comma-separated-public-json> \
  --local-home "$HOME/.ember/validator"
install -m 0600 genesis.json "$HOME/.ember/validator/config/genesis.json"
ember-role validator config/validator.env
```

On the live testnet, joining or leaving the validator set after genesis uses
the on-chain membership change process (governance-signed set changes; see
[VALIDATORS.md](VALIDATORS.md)); the ceremony in `GENESIS_CEREMONY.md`
assembles a new chain's genesis.

Keep consensus keys separate from account/governor keys. Back up the complete
validator home only while stopped or from an atomic filesystem snapshot. Monitor
RPC `/status`, Prometheus `/metrics`, peer count, block height and missed rounds.
Never reuse a validator signing state on two live machines.
Follow `GENESIS_CEREMONY.md`; any mismatch blocks launch.
