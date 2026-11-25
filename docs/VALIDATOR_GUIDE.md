# Run a ledger node: synchronization and validator admission

There are two independent paths. Anyone may synchronize a non-voting ledger
node without registering inference, owning a GPU, or obtaining anyone's approval.
Joining the validator voting set requires the separate governance process below.
Do not create a new chain genesis to join the public network.

## Self-service synchronization

Use the published `network.json` as the bootstrap metadata source. The chain's
CometBFT `genesis.json` is distinct from model `genesis.pb` and descriptive model
`genesis.json` files. Staging ledger genesis files cannot bootstrap this chain.

The `bootstrap` schema is published as `network-bootstrap.schema.json`:

- `status`: `pending-publication` or `ready`.
- `genesis_url` and `genesis_sha256`: immutable original chain genesis bytes and
  their SHA256, independently authenticated against the ceremony record.
- `statesync.trust_height` and `statesync.trust_hash`: recent trusted height and
  **header hash**, not app hash or snapshot-package hash.
- `statesync.trust_period`, `trusted_header_time`, `expires_at`, `rpc_servers`:
  valid trust window and at least two independent RPC operators.
- `snapshot_peers`: reachable public snapshot suppliers in node-id@host:port form.

The live `network.json` published at https://emberinfer.org/network.json is the
bootstrap authority and currently carries `status=ready` with independently
verified anchors and a public snapshot supplier. Source-tree copies may carry
null anchors and empty peer lists with `pending-publication` placeholders and
must not be relied on for bootstrap. Do not substitute a staging genesis,
fabricate a checkpoint or replay from block 1 with the current binary to work
around missing anchors. See the [bootstrap runbook](BOOTSTRAP-RUNBOOK.md) for the state-sync procedure.

1. Download the Linux release and its checksum/signature assets from the
   `release.base_url` in network.json. Authenticate the signing key independently
   and verify the release according to SIGNING.md. If that release lacks a
   signature, a checksum alone does not authenticate it. Extract the matching
   architecture archive and install the ledger binaries:

```sh
set -euo pipefail
PREFIX="$HOME/ember-ledger"
./install-release.sh --role validator --prefix "$PREFIX"
export PATH="$PREFIX/bin:$PATH"
curl -fsS https://emberinfer.org/network.json -o network.json
```

2. Check `bootstrap.status`, the independently authenticated genesis pin and the
   recent checkpoint before creating the node home. This guard explicitly stops
   on the unpublished placeholders. Checkpoint independence requires a trusted
   local chain/light client and validator-set continuity; two matching RPC JSON
   responses alone are not a trust proof. The launcher rejects an expired or
   wrong-chain/header anchor; verify the independent witness records as well.

```sh
python3 - <<'PY'
import datetime, json
n = json.load(open('network.json'))
b = n['bootstrap']
assert b['status'] == 'ready', 'Bootstrap anchors/snapshot suppliers are not published yet'
s = b['statesync']
assert b['genesis_url'] and b['genesis_sha256'] and s['trust_height'] and s['trust_hash']
assert len(set(s['rpc_servers'])) >= 2 and b['snapshot_peers']
assert datetime.datetime.now(datetime.timezone.utc) < datetime.datetime.fromisoformat(s['expires_at'].replace('Z', '+00:00')), 'Checkpoint expired; obtain a fresh authenticated anchor'
print('Chain:', n['ledger']['chain_id'], 'trusted header height:', s['trust_height'])
PY
GENESIS_URL=$(python3 -c 'import json; print(json.load(open("network.json"))["bootstrap"]["genesis_url"])')
GENESIS_SHA256=$(python3 -c 'import json; print(json.load(open("network.json"))["bootstrap"]["genesis_sha256"])')
curl -fsSL "$GENESIS_URL" -o ledger-genesis.json
printf '%s  %s\n' "$GENESIS_SHA256" ledger-genesis.json | sha256sum -c -
```

The SHA256 is over original downloaded bytes; reserializing an RPC `/genesis`
JSON wrapper does not reproduce that byte pin. Compare chain ID, genesis time,
initial height and initial validator set to the authenticated ceremony record.

3. Create a **new** home with independent node and consensus keys. The CLI name
   `init-validator` creates local keys only; it grants no voting membership.
   Never copy donor node keys, validator keys or signing high-water marks.

```sh
export LEDGER_HOME="$HOME/.ember/ledger-observer"
test ! -e "$LEDGER_HOME"
emberchain init-validator --home "$LEDGER_HOME"
install -m 0600 ledger-genesis.json "$LEDGER_HOME/config/genesis.json"
python3 - <<'PY'
import json, os
from pathlib import Path
n = json.load(open('network.json')); b = n['bootstrap']; s = b['statesync']
assert b['status'] == 'ready'
assert json.load(open('ledger-genesis.json'))['chain_id'] == n['ledger']['chain_id']
# This is a fresh home: write the supported state-sync consumer settings explicitly.
config = ('[statesync]\nenable = true\n'
          + 'rpc_servers = ' + json.dumps(','.join(s['rpc_servers'])) + '\n'
          + 'trust_height = ' + str(s['trust_height']) + '\n'
          + 'trust_hash = ' + json.dumps(s['trust_hash']) + '\n'
          + 'trust_period = ' + json.dumps(s['trust_period']) + '\n'
          + '[snapshots]\ninterval = 0\nkeep_recent = 2\n')
p = Path(os.environ['LEDGER_HOME']) / 'config/config.toml'
p.write_text(config); p.chmod(0o600)
PY
PEERS=$(python3 -c 'import json; print(",".join(json.load(open("network.json"))["bootstrap"]["snapshot_peers"]))')
emberchain start --home "$LEDGER_HOME" --p2p tcp://0.0.0.0:26656 \
  --rpc tcp://127.0.0.1:26657 --peers "$PEERS"
```

Public P2P suppliers transfer snapshots; RPC endpoints alone cannot do so.
Snapshot production is separate from consumption: an observer need not supply
snapshots. The runbook describes verified supplier provisioning and capacity
limits; no general offline tar/state.json importer is provided.

4. In another shell, verify `/status`: network chain ID, a rising height and
   eventually `catching_up=false`. Use an independent authenticated RPC from
   `bootstrap.statesync.rpc_servers`. Select a common finalized block height and
   compare `/block?height=H` header `app_hash` on both endpoints. The header at
   H+1 commits application state at H. Header hash/trust_hash and app hash are
   different values. Confirm at least 20 subsequent blocks and inspect historical
   archive/chunk dependencies as in the runbook; RPC reachability alone is not
   synchronization acceptance.

```sh
LOCAL_RPC=http://127.0.0.1:26657
INDEPENDENT_RPC=$(python3 -c 'import json; print(json.load(open("network.json"))["bootstrap"]["statesync"]["rpc_servers"][1])')
curl -fsS "$LOCAL_RPC/status"
HEIGHT=$(curl -fsS "$LOCAL_RPC/status" | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["sync_info"]["latest_block_height"])')
export LOCAL_RPC INDEPENDENT_RPC HEIGHT
python3 - <<'PY'
import json, os, urllib.request
height = os.environ['HEIGHT']
def header(endpoint):
    with urllib.request.urlopen(endpoint + '/block?height=' + height) as response:
        return json.load(response)['result']['block']['header']
a, b = header(os.environ['LOCAL_RPC']), header(os.environ['INDEPENDENT_RPC'])
assert (a['chain_id'], a['height'], a['app_hash']) == (b['chain_id'], b['height'], b['app_hash'])
print('Same-height app hash matches:', a['height'], a['app_hash'])
PY
```

Report the release/source revision, genesis SHA256, checkpoint height/header hash,
snapshot supplier, local node ID, observed status/catching_up and same-height
independent app-hash comparison. Running this observer never requires inference
registration or permission from a current validator.

## Governance admission to the validator set

Synchronization does not grant a vote. Read the [validator membership guide](VALIDATORS.md) and the live
`/validators` and `/parameters` application queries. Query current values instead
of relying on an old validator count, app version or activation height.

1. Protect a distinct Ed25519 validator key and operator identity. Publish only
   the public descriptor (`emberchain validator-public`). Never copy a running
   validator's private key or roll its signing state backward.
2. Meet the current admission collateral and signed ResourceProfile registration
   requirements for the **operator** in the [validator membership guide](VALIDATORS.md). This is a
   governance prerequisite, not an observer-sync prerequisite. No particular
   historical model profile is the required entry path.
3. Submit `validator_register` and a fresh consensus-key readiness proof bound
   to chain ID, operator, public key, recent height and block hash. Maintain
   registration/readiness through the announced effective height.
4. A **current active validator's operator** must propose the join using
   `validator_set_change`. Possessing keys, syncing or staking gives no proposal
   authority and does not guarantee approval.
5. The live `change_delay_blocks` timelock, `change_window_blocks`, one-pending-
   change limit, set size and continuity rules apply. The chain rechecks at
   `effective_height - 2`; CometBFT activates the emitted update at the announced
   effective height. A failed recheck rejects the change; there is no silent
   automatic retry.

Only report validator admission after the activated on-chain set and observed
votes confirm it. Protect private files with mode 0600 and directories with
mode 0700, monitor peering, height, vote participation and alerts, and never run
two machines with the same consensus signer/high-water state. A new-network
ceremony is a separate procedure in GENESIS_CEREMONY.md, never a shortcut for
joining this chain.

Snapshot producers may set `[snapshots] include_archive = false` (default `true`)
when historical archives would exceed snapshot transport limits; restored nodes
start with an empty archive, retain current state, and accumulate new audit history,
while omitted older entries are unavailable (see [snapshot policy](BOOTSTRAP-RUNBOOK.md#snapshot-archive-inclusion-policy)).
