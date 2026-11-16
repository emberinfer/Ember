# Pinned TLS hostname handling (v11-CERT)

## Mechanism and identity

`node/pinned_tls.py` first probes the endpoint using TLS without CA or hostname
verification, obtains the peer DER certificate, and compares its SHA-256 lowercase
hex digest to the supplied pin (lowercased). A mismatch raises `ValueError` before
any gRPC channel is created. This comparison remains unchanged.

After a match, cryptography parses the DER certificate. The helper copies all
caller options and, unless `grpc.ssl_target_name_override` is already supplied,
adds that option using the first DNS SAN, or otherwise the first IP SAN as a
string. gRPC trusts the probed certificate as its root and checks its own name.
The fingerprint is the identity anchor; the name check is redundant under this
pin. The subsequent connection still needs a certificate trusted by that pinned
root and proof of the corresponding private key; this is not an insecure channel.
A caller override is preserved, including its potential to cause a name failure.
No DNS/IP SAN means a clear, fail-closed `ValueError`, even with a caller override;
regenerate using `emberd cert --host` (default SAN is IP 127.0.0.1). A certificate
with only unsupported SAN types also fails closed. Certificate validity and other
gRPC TLS checks remain active. Unpinned clients receive no override or change.

## Coverage inventory

Verified with repository-wide `rg` searches for `pinned_channel`,
`secure_channel`, `insecure_channel`, `ssl_channel_credentials`, SSL contexts,
`server_hostname`, TLS constructors, HTTP clients, and RPC client constructors.
“Via helper” means the caller file is byte-identical but inherits the behavior.
Tests and generated stubs are not production client paths.

| Path | Endpoint role | Trust / transport | Changed? Why |
| --- | --- | --- | --- |
| `node/pinned_tls.py` | Shared pinned gRPC client | Fingerprint probe + pinned certificate root | Yes; SAN-derived target override removes dial-host constraint under pin |
| `node/agent/join.py` `channel_factory` | Node to coordinator/seed, including relay2 | Pinned; explicit insecure-localhost mode is plaintext loopback | Via helper for pinned branch only; insecure branch unchanged |
| `node/shard_rpc.py` `StageClient` | Pipeline client to stage | Pinned | Via helper; message-size options preserved |
| `node/shard_rpc.py` `register_stage` channel factory | Stage agent to coordinator | Pinned | Via helper |
| `node/moe_rpc.py` `_BaseClient` | MoE attention/expert clients, `address@fingerprint` | Pinned | Via helper; message-size options preserved |
| `node/pipeline_client.py` | Constructs MoE targets from on-chain `shard_endpoint` + `shard_tls_fingerprint` | Pinned through MoE clients | Via helper; profile binding unchanged |
| `node/agent/__init__.py` `serve` | Agent tunnel to coordinator | Caller factory, otherwise insecure gRPC (local use) | No direct change; supplied pinned factories inherit fix; default unchanged |
| `deploy/public-relay.py` `seed_client` | Relay to seed upstream | CA-verified TLS bridge, or plaintext loopback carrying TLS passthrough | No; `create_default_context(cafile=--seed-upstream-ca)` with `server_hostname='127.0.0.1'` retains its pre-existing loopback-SAN constraint |
| `deploy/public-relay.py` HTTP proxy | Gateway, faucet, RPC upstreams | Plaintext loopback HTTP | No; independent of pinning |
| `node/engine/adapters/http.py` | External HTTP engine | Standard CA/hostname verification for HTTPS; configured HTTP also supported | No; urllib defaults unchanged |
| `node/fetcher/__init__.py` | Model/source downloads | Standard CA/hostname verification for HTTPS; local files supported | No; urllib defaults unchanged |
| `node/agent/lifecycle.py` | Local node health probe | Enforced plaintext loopback HTTP | No; no TLS |
| `services/gateway/privacy.go` | Privacy executor HTTP endpoint | Default Go CA/hostname verification for HTTPS, or configured HTTP | No; independent HTTP client |
| `services/ledger/chain.go` | Ledger to chain RPC | CometBFT HTTP client; default HTTPS verification if configured, HTTP otherwise | No; no certificate pin |
| `cmd/emberchain/main.go` | CLI chain queries/transactions (also used by node tools) | CometBFT HTTP client; default HTTPS verification if configured, HTTP otherwise | No; no certificate pin |
| `spi-kit/examples/toy_adapter.py` | Example engine HTTP client | urllib default HTTPS verification or configured HTTP (conformance uses loopback) | No; example independent of pinning |
| `ops/validator_dynamics_drill.py`, `ops/f5-gpu-stage.py` | Operator drill HTTP calls | urllib default HTTPS verification or configured HTTP | No; operational tooling independent of pinning |
| `cmd/emberd/main.go` | gRPC/HTTP servers; `cert` generation | Server TLS only (`credentials.NewTLS`, HTTP TLS listeners); SAN from `--host` | No; no Go client TLS implementation here; cert fingerprint is SHA-256 of certificate DER |

All direct production `pinned_channel` calls are the join factory, StageClient,
register_stage factory, and MoE base client above. No proto, Go, consensus, or
AppVersion changes are needed.

## relay2 self-signed workaround retirement runbook

Steps only: this task does not execute any production action.

1. **Precondition — roll every client first.** Deploy builds containing this fix
   to all node/join, stage, and MoE client processes. Identify the executable,
   Python environment, installed package, and service process actually running
   (**operator to confirm**). Run `ember-node version` (the `python -m agent
   version` entry point in the same installed environment) to record its release
   version. Version alone is insufficient to distinguish builds sharing a release:
   check the deployed build/revision provenance includes this fix, or compare the
   installed `pinned_tls.py` with the fixed release artifact. Confirm processes
   have restarted onto that artifact, including stage and MoE processes; the
   deployment mechanism and fleet inventory are **operator to confirm**.
2. Issue a regular certificate for `relay2.emberinfer.org` whose DNS SAN includes
   `relay2.emberinfer.org`, using the operator's ACME/CA of choice. CA account,
   challenge routing, issuance command, renewal setup, and destination paths are
   **operator to confirm**. Preserve the old cert/key and current fingerprint
   configuration for rollback; verify new key/certificate correspondence.
3. Compute the leaf certificate fingerprint as lowercase SHA-256 of DER, not PEM
   text and not SPKI. For example, with the actual leaf PEM path substituted:
   `openssl x509 -in LEAF.pem -outform DER | openssl dgst -sha256`.
   The 64 hex digest is what `emberd cert` prints after `tls-fingerprint=` and
   what `pinned_tls` compares. Confirm the leaf presented by the relay will be
   this certificate, with any chain files ordered appropriately (**operator to
   confirm**).
4. If the pin changes, update the client-facing network artifact's
   `access.tls_fingerprint`, the source mapped by `node/agent/network.py` to
   `EMBER_TLS_FINGERPRINT`, **before swapping the served certificate**. Update
   actual client configurations too: configure deliberately refuses to overwrite
   existing files. Artifact location, distribution, and all overridden CLI/env
   sources are **operator to confirm**. A single-pin client cannot accept both
   old and new certificates: schedule a maintenance window, stage/distribute the
   new configuration before the swap, and coordinate its activation and client
   restart/reconnect with the relay change. Clients attempting new connections
   with the new pin while the old certificate is served will fail closed; old-pin
   clients after the swap will also fail. Do not claim a zero-downtime rotation.
5. In that window, swap the certificate/key files selected by
   `deploy/public-relay.py --seed-cert/--seed-key` and restart **only the relay
   process**. Production paths, process manager, and restart command are
   **operator to confirm**. Keep `--seed-upstream-ca` and the upstream's
   127.0.0.1 SAN arrangement intact: that separate CA-verified leg is unchanged.
6. Verify the served leaf fingerprint matches the distributed pin. Confirm a
   fresh node join via `relay2.emberinfer.org` reaches JOIN READY, then force a
   controlled reconnect of an already-joined node and confirm it rejoins with
   the new configuration. Exact production commands and node selection are
   **operator to confirm**. Check stage and MoE connectivity as applicable.
7. If verification fails, restore previous certificate/key files and previous
   network fingerprint configuration (including client overrides), restart only
   the relay, and reactivate/reconnect clients using the previous configuration.
   Coordinate rollback in the same window because mismatched pins reject new
   connections. Recheck fresh join and existing-node reconnect. Production
   restore commands and configuration distribution are **operator to confirm**.

## Validation

Using `/root/data/ember-gpu-venv/bin/python`:

`python -m unittest -v node/test_pinned_tls.py node/test_node.py node/test_join_retry.py node/test_network.py node/test_reconnect.py`

Result: **29 tests passed**: `test_pinned_tls.py` 6, `test_node.py` 13,
`test_join_retry.py` 7, `test_network.py` 2, `test_reconnect.py` 1.
The new tests use real local TLS gRPC for hostname mismatch success, wrong-pin
rejection before channel creation, IP SAN success, unpinned hostname rejection,
and missing-SAN rejection; option preservation and DNS preference are unit-tested.
No Go files changed; Go build/tests are intentionally skipped.
