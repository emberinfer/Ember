# Ember independently authenticated pipeline sharding

The general unit and architecture contract is [SHARDING_SPEC.md](SHARDING_SPEC.md).

Ember dynamically assembles contiguous model stages. Each stage has its own
Ed25519 identity, stake, signed `ResourceProfile`, invocation receipts and work
share. The first stage owns embeddings; the final stage owns normalization and
the output projection. Signed topology fixes stage boundaries, artifacts, dtype,
runtime policy and audit tolerance.

## Build shard artifacts

```bash
PYTHONPATH=core-py:proto node/.venv/bin/python \
  chain-genesis/build_shards.py \
  --model ~/models/Qwen2.5-1.5B-Instruct \
  --output /secure/ember-qwen-pipeline3 \
  --shards 3
```

Publish `genesis.pb` and `manifest.pb` immutably. Deploy only the assigned
`shard-NNN.safetensors`, config, and signed anchors to each stage. Keep a complete
matching model on an independent auditor until partial-weight audit is available.

## Authenticated stages

Generate a mode-0600 Ed25519 scheduler key and use its account ID from the chain
coordinator set. Stages receive the public account ID, never the private key.
Their TLS certificate fingerprint and stage signer are included in the signed
profile. Every request is signed and carries a persistent replay nonce.

```bash
ember-node shard serve \
  --listen 0.0.0.0:19601 \
  --advertise stage-0.example.org:19601 \
  --state-dir /var/lib/ember/stage-0 \
  --cert /etc/ember/stage.crt --key /etc/ember/stage.key \
  --authorized-scheduler ON_CHAIN_SCHEDULER_ACCOUNT \
  --model-path /opt/ember/stage-0 \
  --genesis /opt/ember/stage-0/genesis.pb \
  --manifest /opt/ember/stage-0/manifest.pb \
  --shard-index 0 --device auto \
  --coordinator seed.example.org:19091 \
  --coordinator-tls-fingerprint SEED_CERT_SHA256
```

The dynamic gateway uses `--pipeline-scheduler-key`; the subprocess receives the
key path, not key bytes. Reversed stages, altered hidden-state handoffs, request
replays, wrong scheduler identities and mismatched parameter hashes fail closed.

The compatibility pipeline adapter uses the same scheduler key:

```bash
ember-node join \
  --state-dir /var/lib/ember/pipeline-agent \
  --coordinator seed.example.org:19091 \
  --tls-fingerprint SEED_CERT_SHA256 \
  --chain ember-qwen2.5-1.5b-pipeline3 \
  --engine pipeline --model-path /opt/ember/pipeline-common \
  --scheduler-key-file /etc/ember/scheduler.key \
  --scheduler-id ON_CHAIN_SCHEDULER_ACCOUNT \
  --stage stage-0.example.org:19601@STAGE_0_CERT_SHA256 \
  --stage stage-1.example.org:19602@STAGE_1_CERT_SHA256 \
  --stage stage-2.example.org:19603@STAGE_2_CERT_SHA256
```

## Settlement and limits

- SQLite development mode independently credits stages and, on reject, slashes
  only signed `fault_node_ids`; an innocent stage loses zero stake.
- Same-range non-streaming failover is supported. Output already streamed to a
  consumer cannot migrate to another stage.
- Chain mode supports independently signed pipeline holds, frozen stage registrations,
  activation continuity, authorized full-model audits and per-unit credits. This
  pipeline audit path uses genesis-authorized reference identities; it is separate
  from the registered distributed whole-model replay-worker route.
- Full-model audit remains required. Cross-runtime comparisons remain observe-only.
- The reference stage executor implements contiguous Qwen2 layers. Engine SPI
  profiles may define other model-specific shard units without changing gateway,
  ledger or signing semantics.

Public regression coverage is in [shard receipt tests](../node/test_shard_receipts.py),
[pipeline audit tests](../node/test_pipeline_audit.py) and
[consensus pipeline tests](../services/chain/pipeline_test.go). These contract
tests do not establish WAN performance.

Build explicit endpoint stages with `chain-genesis/build_shards.py
--separate-boundaries --shards 2`: the resulting four stages are embedding,
[0,14), [14,28), and lm_head for the 28-layer reference. Each stage uses the same
`shard serve` command with its own signed index. For consensus registration also
supply `--chain-rpc`, `--chain-cli`, `--ledger-chain-id` and `--registration-ttl`.
The model's signed genesis/manifest must be anchored in the ledger genesis;
this builder does not claim a permissionless derived-artifact publication flow.
