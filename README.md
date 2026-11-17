# Ember

Ember is an open inference network with signed execution evidence, sampled replay
audits and Spark accounting on a CometBFT ledger. Joining is permissionless;
Ledger membership and serving registrations are queryable on-chain.

## Disclaimer

Ember is experimental, open-source research software provided **as-is, without
warranty**, subject to the [Apache-2.0 license](LICENSE).
**Testnet Spark is valueless test credit.** It is not money or a financial
instrument and is not offered for sale. The Ember project operates no exchange
or fiat on/off-ramp. Nothing here is an offer, solicitation or investment advice.
Participation is at your own risk and subject to local rules. Read the
[full disclaimer](docs/DISCLAIMER.md).

## Network status

The public testnet is live with v0.6.0.

The live chain is `ember-testnet-1`, with genesis dated 2026-11-14 and five Ledger nodes. Live AppVersion 12 activated at height 116582. The source base constant is 11; the coordinated upgrade ladder advances the running state to 12.

The latest software release is [v0.6.0](https://github.com/emberinfer/Ember/releases/tag/v0.6.0), whose tag points to snapshot `b8b3128`. Assets are the Linux amd64 and arm64 archives, `ember-engine-spi-kit-0.6.0.tar.gz`, `SHA256SUMS` and `release-signing.pub`.

**v0.6.0 has no published `SHA256SUMS.sig`.** The signing key is offline. Checksum comparison detects mismatched downloaded bytes but does not authenticate unsigned checksums.

Measured SHA-256 of the release key's DER SubjectPublicKeyInfo:
`d3b07085781f2d4d75de6caefa6e1dc9cd4eadeec10326b2b0c20eb03254eabb`.


## Use / Serve / Ledger

- **Use:** send authenticated OpenAI-compatible chat-completion requests to a gateway.
- **Serve:** run a compatible model or shard, submit signed evidence and receive test credit after settlement. Eligible serving registrations also offer audit work by default.
- **Ledger:** run a consensus node that orders deterministic account, registration, hold and settlement transitions. Ledger nodes do not run model inference inside consensus.

## Models and profiles

A model is one set of weights; a profile is a deployment form. Forms are **Whole model** and **Sharded** (by layer, by expert, or layer + expert). A unified profile covers whole-model and sharded forms under one card. CPU/GPU describes a node, not a form. The 1.5B `-segmented` listing is the unified profile's sharded entry point, not another profile.

| Profile | Model | Context tokens | Availability |
| --- | --- | ---: | --- |
| `ember-qwen2.5-1.5b-unified-v1` | Qwen2.5-1.5B-Instruct | 32768 | Serving; unified Whole model and Sharded forms |
| `ember-qwen3.6-35b-a3b-fp8-unified-v1` | Qwen3.6-35B-A3B FP8 | 262144 | Serving via vLLM on GPU nodes |
| `ember-olmoe-1b-7b-moe-v1` | OLMoE-1B-7B | 4096 | Serving; Sharded by layer + expert; sampled segment-replay audits pass |
| `ember-kimi-k3.1-pro-v1` | Kimi K3.1 Pro | Not specified | Published on-chain; not deployed; no serving nodes |


## Use it

Obtain a current authenticated gateway address, bearer token and funded account.
Endpoint addresses and certificate pins must be verified with the deployment
operator; the repository website copy is not a current endpoint directory.

```sh
curl -N -fsS "$EMBER_GATEWAY_URL/v1/chat/completions" \
  -H "Authorization: Bearer $EMBER_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"ember-qwen2.5-1.5b-unified-v1","messages":[{"role":"user","content":"Say OK"}],"max_tokens":2,"stream":true}'
```

Whole-model and MoE paths support true SSE streaming. The reference live block
cadence is about 1.5–1.9 seconds, segmented execution about 1.02 seconds/token,
and a short 35B request about 10–12 seconds end-to-end. These reference numbers
are not throughput or latency guarantees.

## Run a node

Follow the [quickstart](docs/QUICKSTART.md) for release checks, installation,
verified profile artifacts, registration and exit. Model weights are separate.
Since AppVersion 10, registration offers both execution and on-chain audit eligibility by default. `--no-exec` and `--no-audit` select registration-time opt-outs. Eligibility still requires the appropriate evidence and replay capabilities. Registration binds the identity, exact signed profile, layer range and capacity. There is no deregistration or mid-term removal of an enabled role. Registrations lapse after the live 200,000-block TTL; stopping a process does not erase its registration.

For consensus participation, follow the [Ledger node guide](docs/VALIDATOR_GUIDE.md).

## How it works

Freeze a bounded hold, execute, commit signed evidence, resolve sampled audit, then settle using actual token counts. Accepted fees allocate 80% to execution, 15% to audit and 5% back to the caller. Unused reservation is refunded. The live hold timeout is 5,000 blocks; expiry refunds unresolved holds through `expire_hold` and gateway recovery, rather than a wall-clock timer.

Audit assignment requires committed evidence and a later finalized transaction;
the code derives its entropy from blocks immediately after **freeze**, not
necessarily after evidence commit. Gateways cannot name the assigned auditor.
Teacher-forcing replay is the default equality-audit path. The first divergent
token is evaluated with epsilon 1.0; a known alternative within the margin can
be accepted. Conviction requires corroboration, and whole-model adjudication
allows at most one reviewer beyond the second. See [audit design](docs/AUDIT_DESIGN.md).

Gateway dispatch combines reputation, session/cache affinity and locally measured
speed. Whole-model and sharded candidates stay separate. Speed does not alter
trust reputation or consensus audit sampling. The source currently records speed
after successful receipt submission, before final settlement. MoE generation
uses a bounded FIFO per gateway; full or expired waits return retryable 503.
See [scheduling](docs/SCHEDULING.md) and [MoE queue operation](docs/moe-serialization.md).

## Status and limitations

Signatures attribute statements; sampled audits do not prove every computation.
Distinct identities do not prove independent ownership. Ordinary gateway requests
expose needed plaintext to gateways, execution nodes and auditors. TLS does not
make execution confidential. Contract conformance is not model correctness or
a security audit. The source includes a separate [encrypted API](docs/PRIVACY.md)
with its own trust assumptions; no live deployment of that API is asserted here.

The live website is built from a separate repository. This repository's `site/`
is a documentation baseline, not evidence of live endpoint availability.

## Documentation

- [Quickstart](docs/QUICKSTART.md), [operations](docs/OPERATIONS.md), [public network](docs/PUBLIC_NETWORK.md), [Ledger membership](docs/VALIDATORS.md)
- [Pricing](docs/PRICING.md), [publishing](docs/PUBLISHING.md), [profiles](docs/PROFILE_RELEASES.md)
- [Engine SPI](docs/ENGINE_SPI.md), [adapters](docs/ENGINE_ADAPTERS.md), [SPI kit](spi-kit/README.md), [sharding](docs/SHARDING.md)
- [Audit design](docs/AUDIT_DESIGN.md), [slashing](docs/SLASHING.md), [threat model](docs/THREAT_MODEL.md), [release signing](docs/SIGNING.md), [upgrade](docs/UPGRADE.md)

## Contributing

Include reproduction steps and verification results. Distinguish synthetic
fixtures from measured inference. Published signed profile bytes are immutable.
Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE); see [NOTICE](NOTICE). Model weights have separate licenses.
