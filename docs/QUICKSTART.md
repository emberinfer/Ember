# Quickstart

Read the [disclaimer](https://github.com/emberinfer/Ember/blob/master/docs/DISCLAIMER.md). Testnet Spark is valueless test credit.

## Prerequisites

On a fresh Ubuntu host, install the Python 3.12 venv package before running the release installer:

```sh
sudo apt-get install -y python3.12-venv
```

Use Linux amd64 or arm64, Python 3.12 with venv/ensurepip, OpenSSL with Ed25519, `tar`, `sha256sum` and `curl` for this CPU installation path. Check runtime and hardware against the selected signed profile. Other engines need their own compatible dependencies. Installation does not supply model weights.

## Verify a release

The latest software release is on the [releases page](https://github.com/emberinfer/Ember/releases/latest). If you are installing a different release, set `EMBER_VERSION` to that version without the leading `v`. Check that the selected release includes `SHA256SUMS.sig`, `SHA256SUMS` and `release-signing.pub`. If any are missing, choose a signed release.

Measured SHA-256 of the release key's DER SubjectPublicKeyInfo:

`d3b07085781f2d4d75de6caefa6e1dc9cd4eadeec10326b2b0c20eb03254eabb`.

Authenticate the fingerprint through a trusted channel. The downloaded key alone is not a trust anchor. The repository trust statement names an older key; its detached signature fails verification against the checked-in root key and current statement bytes. See [signing](SIGNING.md).

```sh
EMBER_VERSION='0.10.2'
EMBER_RELEASE="https://github.com/emberinfer/Ember/releases/download/v$EMBER_VERSION"
# Use arm64 instead of amd64 for ARM64.
for file in "ember-$EMBER_VERSION-linux-amd64.tar.gz" SHA256SUMS SHA256SUMS.sig release-signing.pub; do
  curl -fLO "$EMBER_RELEASE/$file" || exit 1
done
openssl pkey -pubin -in release-signing.pub -outform DER | sha256sum
# Compare the fingerprint with your trusted value before continuing.
openssl pkeyutl -verify -pubin -inkey release-signing.pub -rawin \
  -in SHA256SUMS -sigfile SHA256SUMS.sig || exit 1
sha256sum --ignore-missing -c SHA256SUMS || exit 1
tar -xzf "ember-$EMBER_VERSION-linux-amd64.tar.gz"
cd "ember-$EMBER_VERSION-linux-amd64"
./install-release.sh --role node --prefix "$HOME/ember" --python python3.12
```

## Run a consumer request

Use the tools from the node installation above. You can place an order without starting a node or downloading model weights. Run the examples in the same Bash shell.

### 1. Create a consumer identity

```bash
umask 077
CHAIN="$HOME/ember/bin/emberchain"
PYTHON="$HOME/ember/venv/bin/python"
KEY="$HOME/ember-consumer.key"
"$CHAIN" keygen --out "$KEY"
ADDRESS=$("$CHAIN" account --key "$KEY")
printf '%s\n' "$ADDRESS"
```

The address is the lowercase hex SHA-256 of the raw 32-byte Ed25519 public key. Keep the private key on your machine.

### 2. Claim test Spark through either faucet route

The MCP endpoint accepts JSON-RPC 2.0 over HTTPS. Send `tools/call` with `params.name` set to `faucet_claim` and `params.arguments.address` set to your 64-character hex address. This tool requires no signature. The helper works for later MCP calls too.

```bash
mcp() {
  curl -fsS https://relay.emberinfer.org/mcp \
    -H 'Content-Type: application/json' --data-binary "$1"
}

mcp "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"faucet_claim\",\"arguments\":{\"address\":\"$ADDRESS\"}}}"
```

On success, `result.isError` is `false` and `result.content[0].text` is a string containing the faucet JSON described below. A tool failure sets `result.isError` to `true` and puts an error message in the text item. Both routes enforce faucet limits.

For a direct claim, send `POST` with `Content-Type: application/json` and a JSON object with the `account` field. The URL below is the public faucet listed at `access.faucet` in the public `network.json`.

```bash
curl -fsS -X POST http://relay.emberinfer.org:39095/testnet/faucet \
  -H 'Content-Type: application/json' --data-binary "{\"account\":\"$ADDRESS\"}"
```

The direct success response is a JSON object with `account` (the credited address), `amount` (an integer in subunits), `unit` (`Spark-subunit`), `network` (`testnet`) and `value` (`none`).

### 3. Deposit website credit

The relay's `inference` tool spends prepaid website credit. Fetch the public `network.json` to look up the current website account and get the RPC URL and chain ID. When `network.json` omits `site_account`, use `network_status.site_account` after matching its chain ID to the public network file. The website credits your address automatically after the chain transfer finalizes.

```bash
NETWORK=$(curl -fsS https://emberinfer.org/network.json) || exit 1
RPC=$(printf '%s' "$NETWORK" | "$PYTHON" -c 'import json,sys; print(json.load(sys.stdin)["access"]["rpc"])')
CHAIN_ID=$(printf '%s' "$NETWORK" | "$PYTHON" -c 'import json,sys; print(json.load(sys.stdin)["ledger"]["chain_id"])')

mcp_text() {
  "$PYTHON" -c '
import json, sys
reply = json.load(sys.stdin)
if "error" in reply:
    sys.exit(reply["error"]["message"])
result = reply["result"]
text = result["content"][0]["text"]
if result["isError"]:
    sys.exit(text)
print(text)
'
}

STATUS=$(mcp '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"network_status","arguments":{}}}' | mcp_text) || exit 1
SITE_ACCOUNT=$("$PYTHON" - "$NETWORK" "$STATUS" "$CHAIN_ID" <<'PY'
import json, re, sys

network, status = map(json.loads, sys.argv[1:3])
if status["chain_id"] != sys.argv[3]:
    sys.exit("MCP and network.json chain IDs differ")
account = network.get("site_account") or status["site_account"]
if not re.fullmatch(r"[0-9a-f]{64}", account):
    sys.exit("Invalid website account")
print(account)
PY
) || exit 1

ACCOUNT=$("$CHAIN" query --rpc "$RPC" --path "/account/$ADDRESS") || exit 1
printf '%s\n' "$ACCOUNT"
NONCE=$(printf '%s' "$ACCOUNT" | "$PYTHON" -c 'import json,sys; print(json.load(sys.stdin)["nonce"] + 1)') || exit 1
AMOUNT=1000
"$CHAIN" tx --rpc "$RPC" --chain-id "$CHAIN_ID" --key "$KEY" --nonce "$NONCE" \
  --action "{\"type\":\"transfer\",\"to\":\"$SITE_ACCOUNT\",\"amount\":$AMOUNT}"
```

Set `AMOUNT` in subunits within your finalized balance. A successful transfer prints its height and transaction hash. The CLI signs the transfer with your consumer key and broadcasts it to the public chain RPC.

### 4. Sign and submit an inference order

Choose an available `profiles[].id` from `network_status`. The example picks the first available profile. The tool takes `address`, `model`, `messages`, `max_tokens`, `pubkey` and `signature`. Encode the raw public key and signature in base64. Sign the UTF-8 string `ember-web/mcp-inference/v1\n<address>\n<model>\n<sha256hex(canonical messages JSON)>\n<max_tokens>` with Ed25519. Each `\n` is one newline. There is no trailing newline. Canonical messages JSON uses compact separators, sorted keys and unescaped non-ASCII text.

```bash
REQUEST=$("$PYTHON" - "$KEY" "$ADDRESS" "$STATUS" <<'PY'
import base64, hashlib, json, sys
from pathlib import Path
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

key_path, address, status = sys.argv[1:]
model = next((profile["id"] for profile in json.loads(status)["profiles"] if profile["available"]), None)
if model is None:
    sys.exit("No available profile. Query network_status again.")
messages = [{"role": "user", "content": "Say OK"}]
max_tokens = 16
canonical = json.dumps(messages, separators=(",", ":"), sort_keys=True, ensure_ascii=False)
digest = hashlib.sha256(canonical.encode("utf-8")).hexdigest()
message = f"ember-web/mcp-inference/v1\n{address}\n{model}\n{digest}\n{max_tokens}"
key = Ed25519PrivateKey.from_private_bytes(Path(key_path).read_bytes()[:32])
arguments = {
    "address": address, "model": model, "messages": messages, "max_tokens": max_tokens,
    "pubkey": base64.b64encode(key.public_key().public_bytes_raw()).decode("ascii"),
    "signature": base64.b64encode(key.sign(message.encode("utf-8"))).decode("ascii"),
}
print(json.dumps({"jsonrpc": "2.0", "id": 3, "method": "tools/call",
                  "params": {"name": "inference", "arguments": arguments}}))
PY
) || exit 1
RECEIPT=$(mcp "$REQUEST" | mcp_text) || exit 1
printf '%s\n' "$RECEIPT" | "$PYTHON" -m json.tool
```

### 5. Check settlement

The inference result contains `content`, `request_id` and a settlement receipt. A receipt with `settlement.state` equal to `settled` reports `settlement.total_fee` in subunits. If it is `pending`, query the same request ID again with `get_trace` until it reaches a terminal state. The settled trace shows the charge under `settlement.shares.fee`, along with the settlement height and payment shares.

```bash
REQUEST_ID=$(printf '%s' "$RECEIPT" | "$PYTHON" -c 'import json,sys; print(json.load(sys.stdin)["request_id"])')
mcp "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"get_trace\",\"arguments\":{\"request_id\":\"$REQUEST_ID\"}}}" \
  | mcp_text | "$PYTHON" -m json.tool
```

## Install a role

The command above installs the node role under the selected prefix. The installer also accepts `--role gateway` and `--role validator`; the latter is the CLI name for a Ledger node. See the [Ledger node guide](VALIDATOR_GUIDE.md).

Obtain current signed profile artifacts, their chain anchors, model origin and immutable revision through an authenticated deployment channel. Verify local weights against the manifest before serving. Prefer the release package's `network.json` operator directory for `configure`. The [website's public view](https://emberinfer.org/network.json) supplies ledger bootstrap anchors. Verify the network parameters and supply the matching signed profile artifacts to the installation.

The configuration helper consumes a complete parameter file and refuses existing config overwrite:

```sh
NETWORK_PARAMETERS="$HOME/ember/network.json"
"$HOME/ember/bin/ember-node" configure \
  --network-parameters "$NETWORK_PARAMETERS" \
  --model-path "$MODEL_PATH" \
  --config "$HOME/ember/config/join.env"
"$HOME/ember/bin/ember-node" id --state-dir "$HOME/ember/var/node"
```

By default, `configure` selects all eligible coordinators for the profile from
the gateway directory. To select a single coordinator, pass `--gateway <url>`
with its directory URL or `--gateway <id>` with its directory ID.

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

Fund that account using the deployment's verified funding procedure. For the public testnet, use the faucet examples above with your node's account address. Wait for finalized funding before enrollment.

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

The node installer requires Python 3.12 with working venv/pip support. Before
copying any files into the prefix, it creates a temporary pip-enabled venv with
the selected interpreter and runs pip. On Ubuntu, repair missing support with
`sudo apt-get install -y python3.12-venv` and retry. Probe failure leaves no
partially installed prefix.
