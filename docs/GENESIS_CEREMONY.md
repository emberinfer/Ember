# Genesis Ceremony

Validator operators and genesis assemblers can use this procedure to create and
independently verify a network genesis. Only public validator material is exchanged.

## Roles and inputs

- Each validator operator controls one isolated validator home and its consensus
  signing key. Validator private keys and signing state never leave that host.
- The assembler receives reviewed `validator-public` JSON descriptors and the
  reviewed Ember app-state JSON. The assembler has no validator private keys.
- Every operator receives the complete public descriptor set, app state, and
  assembled genesis, then verifies all three before launch.

## Operator procedure

On each operator host, from the verified signed release:

```sh
install -d -m 0700 "$HOME/ember-operator/validator"
emberchain init-validator --home "$HOME/ember-operator/validator"
emberchain validator-public \
  --home "$HOME/ember-operator/validator" \
  --name operator-a-v1 \
  --out "$HOME/ember-operator/operator-a-v1.public.json"
sha256sum "$HOME/ember-operator/operator-a-v1.public.json"
```

Exchange only the `.public.json` file and its SHA-256 digest. Do not exchange
`priv_validator_key.json`, `priv_validator_state.json`, `node_key.json`, account
keys, SSH keys, bearer tokens, or release-signing private material.

The assembler records the ordered public set and creates the genesis:

```sh
emberchain genesis --out genesis.json --chain-id ember-<network> \
  --app-state app-state.json \
  --validator-public operator-a-v1.public.json,operator-b-v1.public.json,operator-c-v1.public.json
chmod 0600 genesis.json app-state.json
sha256sum genesis.json app-state.json operator-*.public.json
```

Each operator independently verifies the exact public set and its own local key:

```sh
emberchain verify-genesis --genesis genesis.json --app-state app-state.json \
  --validator-public operator-a-v1.public.json,operator-b-v1.public.json,operator-c-v1.public.json \
  --local-home "$HOME/ember-operator/validator"
install -m 0600 genesis.json "$HOME/ember-operator/validator/config/genesis.json"
```

Launch is blocked if any operator reports a different genesis hash, descriptor
order, app state, validator name, voting key, or local-key match. Retain each
operator's verification output and the ceremony manifest in access-controlled
operator records; publish the agreed public genesis hash for participants.

## Hard-fork ceremony

A model/Profile hard fork uses a new chain ID and a new ceremony. The old chain
must be halted and backed up first. Current Ember genesis cannot safely carry
active holds, audit assignments, node-registration TTLs, or pending evidence;
follow `UPGRADE.md` and require a drained state before assembly.
