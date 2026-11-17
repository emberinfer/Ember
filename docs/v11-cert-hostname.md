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

## Certificate rotation

Deploy compatible pinned clients first. Verify the new certificate/key and SANs,
compute the leaf certificate's SHA-256 DER fingerprint, distribute the intended
pin through authenticated configuration, and coordinate activation with the relay
change. `configure` does not overwrite existing configurations. A single-pin
client cannot accept both certificates: mismatched pins fail closed, so do not
promise a zero-downtime rotation.

The relay's CA-verified upstream TLS leg is separate and retains its own hostname
checks. Verify fresh join, reconnect, stage and MoE channels as applicable after
rotation. Retain old certificate and configuration for coordinated rollback.
This is a generic procedure; no current endpoint, process manager, certificate
path or successful fleet rollout is asserted here.

Source and local regression coverage: `node/pinned_tls.py:10`,
`node/test_pinned_tls.py`, `deploy/public-relay.py`.
