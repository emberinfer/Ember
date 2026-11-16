# Ember

Ember is an open, decentralized inference network. Independent operators
contribute inference capacity; work is proven with signed execution evidence
and sampled replay audits, and usage is settled in Spark on a shared ledger.

The network is open to independent operators contributing inference capacity.
Current validator and custody limitations are described in
[Status and limitations](#status-and-limitations).

## Disclaimer

Ember is experimental, open-source research software provided **as-is, without
warranty**, subject to the [Apache-2.0 license](LICENSE).

**Testnet Spark is valueless test credit.** It is not money or a financial
instrument and is not offered for sale. The Ember project operates no exchange
or fiat on/off-ramp of any kind.

Nothing in this documentation is an offer, solicitation, or investment advice.
Participation as an operator or consumer is at your own risk and subject to your
own local rules. Read the [full disclaimer](docs/DISCLAIMER.md) before participating.

## TL;DR

- Connect inference contributors through outbound tunnels, serve chat completions,
  audit signed execution evidence, and record Spark accounting on a shared ledger.
- The completed **v0.20.0 baseline** includes real Qwen2.5-1.5B inference,
  CometBFT settlement, NAT serving, and signed Linux release/install tooling.
- **Public testnet LIVE:** [v0.22.0](https://github.com/emberinfer/Ember/releases/tag/v0.22.0)
  supports public operator onboarding, settlement and full stake recovery. See the
  [live site](https://emberinfer.org/) and signed
  [network parameters](https://emberinfer.org/network.json).
- Start with [Use it](#use-it) or the [operator quickstart](docs/QUICKSTART.md).

## What it is

- A **gateway** accepts OpenAI-compatible chat-completion requests and streams
  output; a scheduler selects eligible nodes, and seeds relay signed work.
- **Nodes** run a whole model or a shard stage, connecting outbound to a seed so
  they need no inbound public serving port.
- **Auditors** replay committed token prefixes and sign evidence-based verdicts.
- The **CometBFT ledger** (`emberchain`) orders accounts, stake, registrations,
  holds, evidence, audits, and settlement. Validators do not run model inference
  in consensus.
- The **Engine SPI** is the adapter contract between inference engines and the
  network's scheduling, signing, transport, and accounting.

## Why Ember

- **Concentrated supply:** inference is commonly accessed through a few clouds.
  Ember provides an open protocol and implementation for contributors to serve
  through gateways. Independent operators at public-network scale remain a target.
- **Idle, heterogeneous hardware:** devices cannot safely join merely by claiming
  capacity. Ember checks signed capabilities, model/Profile compatibility, stake,
  and registration before scheduling; eligible nodes can enroll through the public faucet.
- **Unverifiable remote execution:** a response alone says little about how it
  was produced. Signed commitments and sampled numerical replay make detected
  deviations attributable, with probabilistic rather than exhaustive guarantees.
- **Shared accounting:** open contributors need more than one gateway's private
  balance sheet. CometBFT records bounded holds, audits, and whole-model settlement;
  production issuance economics remain outside the delivered baseline.

## Use it

### Call a gateway

Use the gateway URL in the verified [network parameters](https://emberinfer.org/network.json)
and obtain a bearer token and funded consumer account from the gateway operator.
Set `EMBER_GATEWAY_URL` to its base URL without a trailing slash and `EMBER_TOKEN`
to the token. Public gateway HTTP carries valueless test credit; prompts are
visible to the gateway and serving/auditing nodes. The current model chain is
`ember-qwen2.5-1.5b-s22-cpu-v1`.

```sh
curl -fsS "$EMBER_GATEWAY_URL/v1/chat/completions" \
  -H "Authorization: Bearer $EMBER_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"ember-qwen2.5-1.5b-s22-cpu-v1","messages":[{"role":"user","content":"Say OK"}],"max_tokens":2}'
```

Add `"stream":true` to the JSON and `-N` to curl for server-sent events (SSE).

### Run a node

Follow the [quickstart](docs/QUICKSTART.md) using the
[v0.22.0 signed release set](https://github.com/emberinfer/Ember/releases/tag/v0.22.0):
your architecture archive, `SHA256SUMS`, `SHA256SUMS.sig`, `release-signing.pub`
and `network.json` from the same release directory.
Establish trust in the public key separately; a bundled key is not a trust anchor.

Use Linux amd64 or arm64, **Python 3.12 with venv/ensurepip support**
(usually `python3.12` and `python3.12-venv` packages), OpenSSL
with Ed25519 support, `tar`, `sha256sum`, and `curl`. The measured reference Python
is 3.12; runtime versions must match the selected immutable execution Profile.
Model weights are not bundled. From the release-set directory (replace `amd64`
with `arm64` on ARM64):

```sh
EMBER_VERSION=0.22.0
EMBER_RELEASE="https://github.com/emberinfer/Ember/releases/download/v$EMBER_VERSION"
for file in "ember-$EMBER_VERSION-linux-amd64.tar.gz" SHA256SUMS SHA256SUMS.sig release-signing.pub network.json; do
  curl -fLO "$EMBER_RELEASE/$file"
done
openssl pkey -pubin -in release-signing.pub -outform DER | sha256sum
sha256sum --ignore-missing -c SHA256SUMS
openssl pkeyutl -verify -pubin -inkey release-signing.pub \
  -rawin -in SHA256SUMS -sigfile SHA256SUMS.sig
tar -xzf "ember-$EMBER_VERSION-linux-amd64.tar.gz"
cd "ember-$EMBER_VERSION-linux-amd64"
./install-release.sh --role node --prefix "$HOME/ember" --python python3.12
```

The signing-key fingerprint (SHA-256 over DER SubjectPublicKeyInfo) remains
`35091a3aadf80815b47494439db6a0ee13f5a78eecc5bc516772dddefdd6a092`;
compare it through your trusted release channel before installing.
No root access is needed. The installer creates the pinned CPU Python venv.
Download the hash-pinned model revision using the [quickstart](docs/QUICKSTART.md),
then configure from the bundled public parameters and self-fund:

```sh
"$HOME/ember/bin/ember-node" configure \
  --network-parameters "$HOME/ember/network.json" \
  --model-path "$HOME/models/Qwen2.5-1.5B-Instruct" \
  --config "$HOME/ember/config/join.env"
ACCOUNT=$("$HOME/ember/bin/ember-node" id --state-dir "$HOME/ember/var/node")
FAUCET_URL=$(python3.12 -c 'import json,sys; print(json.load(open(sys.argv[1]))["access"]["faucet"])' "$HOME/ember/network.json")
curl -fsS "$FAUCET_URL" -H 'Content-Type: application/json' \
  -d "{\"account\":\"$ACCOUNT\"}"
```

After configuration and funding, join in the foreground; enrollment stakes the
chain's configured minimum and registers the node:

```sh
"$HOME/ember/bin/ember-role" node "$HOME/ember/config/join.env"
```

In another terminal, check readiness:

```sh
curl -fsS http://127.0.0.1:19192/readyz
```

Keep health endpoints and local transport bridges on loopback. To recover stake,
stop serving, wait for registration expiry, unbond, wait for the release height,
then claim using the [documented exit commands](docs/QUICKSTART.md#exit-and-recover-stake).
Keep the node identity throughout. See [public network and seed operation](docs/PUBLIC_NETWORK.md).

### Run a validator

On a separate validator host, verify and extract the release as above. From its
extracted directory, install into a fresh prefix and initialize a new home:

```sh
./install-release.sh --role validator --prefix "$HOME/ember"
"$HOME/ember/bin/emberchain" init-validator --home "$HOME/.ember/validator"
```

Follow the [validator guide](docs/VALIDATOR_GUIDE.md) and
[genesis ceremony](docs/GENESIS_CEREMONY.md) to export the public descriptor,
verify the reviewed genesis against the complete public descriptor set and local
key, and install it. Set `EMBER_VALIDATOR_HOME` in
`$HOME/ember/config/validator.env` to `$HOME/.ember/validator`, configure listeners
and peers, then start:

```sh
"$HOME/ember/bin/ember-role" validator "$HOME/ember/config/validator.env"
```

Consensus keys must stay separate from account keys. Never run two live machines
with the same validator signing state. Keep private keys/auth files mode 0600
and private state/config directories mode 0700.

Gateway operators can use `--role gateway` with the
[gateway template](config/gateway.env.example); provision compatible audit weights
and Python separately. Only node installation creates a venv automatically.

## How it works

For whole-model requests using the chain backend:

1. **Freeze:** authenticate the consumer and commit a bounded hold tied to the
   model/Profile, request, attempt, and expiry height; check executor eligibility.
2. **Execute:** relay signed work through the node's outbound tunnel; return
   generated text or SSE while collecting evidence.
3. **Commit evidence:** record the signed receipt and commitments before assigning
   an audit.
4. **Audit:** derive assignments from finalized CometBFT block entropy, verify
   signatures/openings, and compare forced-prefix replay under the signed policy.
   Slashing requires a rejection confirmed by a different assigned auditor;
   disagreement cannot slash.
5. **Settle:** finalize accepted work, release unused credit, or apply the policy's
   refund/dispute/penalty rule. Abandoned holds can expire by chain height.

```mermaid
flowchart TD
    C[Consumer] -->|HTTPS chat completions / SSE| G[Gateway]
    G --> S[Scheduler / seed discovery]
    N[Execution nodes / Engine SPI] -->|Outbound authenticated tunnel| S
    S -->|Signed work| N
    N -->|Tokens and signed evidence| G
    G -->|Freeze / commit evidence| L[CometBFT ledger / emberchain]
    L -->|Finalized registrations| S
    L -->|Post-commit assignment| A[Auditors / forced-prefix replay]
    N -->|Evidence openings| A
    A -->|Signed verdicts| L
    L -->|Settle / refund / policy penalties| B[Spark accounts]
```

These are logical components: the packaged gateway runs gateway, scheduler,
seed tunnel, and audit coordination together in `emberd`. Seeds can censor or
delay work but cannot forge node signatures or mutate ledger balances; a single
configured seed remains an availability dependency.

## Core concepts

- **Proof of Verified Inference:** attributable, probabilistic fraud detection
  using signed commitments and sampled replay. Signatures identify who committed
  evidence; they do not prove correct inference or every computation.
- **Evidence bundles:** request/attempt bindings, model/Profile digests, token
  commitments, chunk roots, receipts, and openings connect results to declared
  execution. Full-evidence profiles require raw top-k logits and specified hidden
  layers; commitments and signatures are checked before numerical comparison.
- **Capability-graded engines:** admission checks signed capabilities against the
  immutable Profile. The real Transformers adapter provides full evidence; mock
  evidence is test-only. The llama.cpp/vLLM HTTP adapters buffer completions and
  expose top-k logprobs, but lack the logits, layers, and replay required by current
  full-evidence profiles, so admission fails. See [adapters](docs/ENGINE_ADAPTERS.md)
  and the [standalone conformance kit](spi-kit/README.md); kit PASS proves interface
  and evidence-shape conformance, not model correctness or settlement eligibility.
- **Spark:** the accounting unit for balances, stake, holds, and settlement;
  1 Spark = 1,000,000 integer subunits. Testnet Spark is valueless test credit.
  The consensus ledger holds network-wide accounts across model chains/Profiles.

## Status and limitations

The **public testnet is live** with [v0.22.0](https://github.com/emberinfer/Ember/releases/tag/v0.22.0).
Fresh external CPU operators have self-funded, registered, served real requests,
settled through auditors on another host and recovered their full stake.
The [changelog](CHANGELOG.md) distinguishes delivered
work from preparation and [architecture targets](docs/PRODUCTION_ARCH.md).

- Validators are currently operated by a **single organization**. There is
  **no third-party security audit** and no production SLA or public mainnet.
  **Testnet Spark is valueless test credit: no price, no exchange, no returns.**
- External CPU serving is slow for long requests; launch acceptance covers short
  real requests and does not establish production throughput or independent governance.
- Numerical calibration is experimental, with no production error-rate estimate.
  New `union-interval-v1` policies compare bounded top-k gaps and required layer
  distances; this is a screening statistic, and tail deviations may be missed.
  Profiles pin runtime/device pairs, policy, tolerance, and calibration reference.
  Unknown pairs and cross-runtime observation bands are inconclusive for numerical
  enforcement and cannot justify slashing; legacy profiles retain their comparison.
- Validator membership is static and gateway account custody centralized. Compact
  light proofs, independent operators, and production economics remain gaps.
- Dynamic shards have separate identities and receipts, but stage credits and
  precise fault penalties use the SQLite development backend. CometBFT rejects
  pipeline mode. The Qwen2 reference stages require full-model audit; measured
  pipelines are private-LAN results. Non-streaming same-range failover works;
  failure after streaming starts terminates the attempt. WAN readiness is unproven.
- HTTP adapter tests use synthetic wire fixtures; real llama.cpp/vLLM integration
  and cross-engine numerical equivalence are not established. Remote aliases are
  not weight attestation. Runtime memory guards are sampled, not kernel-enforced.
- CometBFT assumes less than one-third Byzantine voting power. Collusion, key
  custody, denial of service, and model supply chains remain risks. TLS protects
  transport; gateways, executors, and auditors see needed plaintext. There is no
  confidential-inference or hardware-attestation claim; finality is not correctness.

## Documentation

| Need | Read |
| --- | --- |
| Understand participation risks | [Disclaimer](docs/DISCLAIMER.md) |
| Install and operate | [Quickstart](docs/QUICKSTART.md), [public network/seeds](docs/PUBLIC_NETWORK.md), [validators](docs/VALIDATOR_GUIDE.md), [genesis ceremony](docs/GENESIS_CEREMONY.md), [hardware](docs/HARDWARE.md), [operations](docs/OPERATIONS.md), [upgrades](docs/UPGRADE.md), [release signing](docs/SIGNING.md) |
| Understand the design | [Architecture](docs/PRODUCTION_ARCH.md), [token state and KV transfer](docs/PRODUCTION_ARCH.md#token-state-and-kv-transfer), [protocol](proto/ember.proto), [ledger decision](docs/LEDGER_DECISION.md), [pricing and quotes](docs/PRICING.md) |
| Integrate engines or shards | [Engine SPI](docs/ENGINE_SPI.md), [adapters](docs/ENGINE_ADAPTERS.md), [standalone kit](spi-kit/README.md), [sharding](docs/SHARDING.md) |
| Assess trust and evidence | [Audit design](docs/AUDIT_DESIGN.md), [audit calibration](docs/AUDIT_CALIBRATION.md), [threat summary](docs/THREAT_MODEL_SUMMARY.md), [full threat model](docs/THREAT_MODEL.md), [changelog](CHANGELOG.md) |

## Contributing

Keep changes focused and include reproduction steps, commands run, and results.
Distinguish mocks, saved captures, and real inference; state runtime/hardware
limits. Protocol/signature changes need compatibility vectors; published model or
policy changes need a new immutable Profile. See [audit calibration](docs/AUDIT_CALIBRATION.md),
[Go requirements](go.mod), and [reference Python dependencies](node/requirements-spi.txt).
Report vulnerabilities privately using [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE); see [NOTICE](NOTICE) for attribution.
Model weights are separate artifacts with their own licenses.

Operator resource controls are documented in [Contribution limits](docs/CONTRIBUTION_CAPS.md).
