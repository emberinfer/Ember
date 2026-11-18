# Ember

Ember is an open inference network: independent hardware operators run models,
submit signed execution evidence and settle inference fees in Spark on a
CometBFT ledger. Participation is permissionless, subject to protocol admission.

The public testnet is live with v0.7.0. The chain is `ember-testnet-1`.
**Testnet Spark is valueless test credit.**

## How the network works

Inference and replay run outside consensus. Ledger nodes order deterministic
accounting transitions; they do not run models or floating-point comparisons
inside consensus.

```text
Authenticated request → Gateway → Model or shard execution
                                      ↓
                               Signed evidence
                                      ↓
                              Sampled replay audit
                                      ↓
                            Ledger settlement / refund
```

Gateways authorize requests and reserve bounded fees. Discovery and scheduling
use finalized registrations and availability information to select compatible
capacity. Nodes connect through outbound authenticated tunnels. The Engine SPI
separates model execution and evidence extraction from network services.

A request follows five steps:

1. Freeze a bounded hold that pins the request's profile, price and maximum fee.
2. Execute on a compatible whole model or sharded path.
3. Commit signed evidence binding the request, profile and output.
4. Resolve sampled audit using eligible replay capacity. The gateway cannot name
   the assigned reviewer. Confirmed penalties require corroborated rejection evidence.
5. Settle using actual token counts and refund unused reservation.

Accepted fees allocate 80% to execution, 15% to audit and 5% back to the caller.
The audit allocation and an individual reviewer's actual payment are distinct.
Unresolved live holds have a 5,000-block expiry; gateway recovery submits the
expiry transaction for refunds. This is height-based, not a wall-clock timer.
HTTP output does not establish final settlement.

Audits sample work rather than verify every request. Unsampled settlement is
explicitly unaudited. Registered equality-audit workers default to teacher-forcing
replay; supported alternative tokens within the declared margin can be accepted.
Signatures and consensus finality do not prove inference correctness. See
[audit design](docs/AUDIT_DESIGN.md), [sampling](docs/AUDIT_SAMPLING.md) and
[architecture](docs/PRODUCTION_ARCH.md).

## Participate

### Use

Send authenticated OpenAI-compatible chat-completion requests to a gateway.
Use [site/network.json](site/network.json) for current gateway addresses, faucet,
RPC, seeds and certificate pins. Obtain your bearer token through the deployment
operator and fund your account using the listed faucet.

For a first request, set `EMBER_GATEWAY_URL` to a listed gateway and `EMBER_TOKEN`
to your operator-issued bearer token and run:

```sh
curl -N -fsS "$EMBER_GATEWAY_URL/v1/chat/completions" \
  -H "Authorization: Bearer $EMBER_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"ember-qwen2.5-1.5b-unified-v1","messages":[{"role":"user","content":"Say OK"}],"max_tokens":2,"stream":true}'
```

Whole-model and MoE paths support SSE streaming. For a quote before inference,
use authenticated `POST /v1/quote` with the intended request body; quoting creates
no hold. Effective prices and frozen multipliers come from chain state. See
[pricing](docs/PRICING.md).

### Serve

Run a compatible model or shard, submit signed evidence and receive test credit
after accepted settlement. Eligible serving registrations offer execution and
audit work by default; each requires the appropriate evidence and replay capabilities.

Start with the [quickstart](docs/QUICKSTART.md): check the release, install,
authenticate network parameters and signed profile artifacts, verify local model
weights, fund the account, then configure and start the node. Linux amd64 and
arm64 release archives are available from the [v0.7.0 release](https://github.com/emberinfer/Ember/releases/tag/v0.7.0).
Installation does not provide model weights.

**v0.7.0 has no published `SHA256SUMS.sig`.** Checksum comparison detects mismatched
bytes but does not authenticate unsigned checksums. Authenticate the release-key
fingerprint through a trusted channel; read [release signing](docs/SIGNING.md)
for the measured keys and trust-statement verification gap.

Registrations bind identity, exact signed profile, layer range and capacity.
`--no-exec` and `--no-audit` select registration-time opt-outs. There is no
mid-term removal of an enabled role or deregistration. The live registration TTL
is 200,000 blocks; stopping a process does not erase the commitment. Follow the
[exit procedure](docs/QUICKSTART.md#exit-and-recover-stake) and
[operations guide](docs/OPERATIONS.md) before withdrawing capacity.

### Ledger

Run a consensus node for accounts, stake, registrations, holds and settlement.
The documented live network has five Ledger nodes. Membership and serving
registrations are queryable on-chain; serving registration alone does not grant
consensus membership.

Follow the [Ledger node guide](docs/VALIDATOR_GUIDE.md) for existing-network
participation, the [membership rules](docs/VALIDATORS.md) for admission and
readiness, and [upgrades](docs/UPGRADE.md) for coordinated changes. Do not create
a new genesis to join an existing chain. Keep consensus signing keys separate
from account keys and never run two live copies of the same consensus identity.

## Models and deployment forms

A model is one set of weights. A profile fixes the signed deployment and evidence
contract. Whole model and Sharded are forms; shards may divide layers, experts,
or both. CPU/GPU describes the node, not a form. Unified profiles cover
whole-model and sharded forms under one card.

Qwen2.5-1.5B-Instruct serves in Whole model and Sharded forms.
Qwen3.6-35B-A3B FP8 serves through vLLM on GPU nodes. OLMoE-1B-7B serves
sharded by layer + expert, with sampled segment-replay audits. Kimi K3.1 Pro is
published on-chain but is not deployed and has no serving nodes.

Publication and endorsement do not activate execution. Consult the
[profile records](docs/PROFILE_RELEASES.md) for exact identifiers and context
limits, [sharding](docs/SHARDING.md) for supported paths and
[publishing](docs/PUBLISHING.md) for immutable profile releases.

## Documentation

| Task | Guides |
| --- | --- |
| Join and operate | [Quickstart](docs/QUICKSTART.md), [public network](docs/PUBLIC_NETWORK.md), [operations](docs/OPERATIONS.md) |
| Participate in consensus | [Ledger guide](docs/VALIDATOR_GUIDE.md), [membership](docs/VALIDATORS.md), [upgrades](docs/UPGRADE.md) |
| Understand network rules | [Architecture](docs/PRODUCTION_ARCH.md), [pricing](docs/PRICING.md), [audit design](docs/AUDIT_DESIGN.md), [slashing](docs/SLASHING.md) |
| Supply model capacity | [Profiles](docs/PROFILE_RELEASES.md), [hardware](docs/HARDWARE.md), [contribution limits](docs/CONTRIBUTION_CAPS.md), [sharding](docs/SHARDING.md) |
| Build integrations | [Engine SPI](docs/ENGINE_SPI.md), [adapters](docs/ENGINE_ADAPTERS.md), [publishing](docs/PUBLISHING.md) |

## Status and limitations

Ember is experimental software, provided as-is without warranty. **Testnet Spark
is valueless test credit**, not money or a financial instrument, and is not
for sale. The project operates no exchange or fiat on/off-ramp. Participation is
at your own risk and subject to local rules. Read the [full disclaimer](docs/DISCLAIMER.md).

Signed evidence attributes statements; sampled audits do not prove every
computation. Unsampled settlement is explicitly unaudited. Distinct identities
do not establish independent ownership. Ordinary requests expose needed plaintext
to gateways, execution nodes and auditors; TLS does not make execution confidential.
The separate [encrypted API](docs/PRIVACY.md) is documented in source without a
claim of live deployment. There is no public mainnet, production SLA or
third-party security audit. See the [threat model](docs/THREAT_MODEL.md).

## Contributing

Include reproduction steps and verification results. Distinguish synthetic
fixtures from measured inference. Published signed profile bytes are immutable.
Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE); see [NOTICE](NOTICE). Model weights have separate licenses.

## ACT-2e MoE launcher reference

[`ops/act2e/moe_service.py`](ops/act2e/moe_service.py) mirrors the ODSI
ACT-2e launcher. The installed fleet launcher at
`/root/work/act2e-nodes/moe_service.py` differs at one line: its gRPC send
and receive limits are 320 MiB, while this reference retains ODSI's 64 MiB.
This source reference is not a production installation or rollout.
