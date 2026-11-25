# Operations

## Health, keys and backups

Gateway and node expose `/healthz`, `/readyz` and `/metrics` on configured
listeners; keep node health access on loopback. Ledger nodes expose CometBFT
`/status` and optional metrics. Protect account, consensus, TLS and wrapping keys
separately. Private files should be mode 0600 and directories mode 0700.

Back up Ledger homes and signing state, application state, node identity,
gateway accounts/auth mappings, audit spool and evidence chunks. Version-11+
state can depend on the adjacent `.archive` directory; copying only state JSON
is insufficient. Preserve activation manifests for replay. Never run two live
copies of a consensus signing identity or roll a signing high-water mark backward.
See [upgrades](UPGRADE.md) and [Ledger guide](VALIDATOR_GUIDE.md).

## Registration and exit

Since AppVersion 10, registration offers both execution and on-chain audit eligibility by default. `--no-exec` and `--no-audit` select registration-time opt-outs. Eligibility still requires the appropriate evidence and replay capabilities. Registration binds the identity, exact signed profile, layer range and capacity. There is no deregistration or mid-term removal of an enabled role. Registrations lapse after the live 200,000-block TTL; stopping a process does not erase its registration.

Stop renewal and the service before exit. Wait for registration lapse, clear
outstanding liabilities, unbond, wait for release height, then claim with the
same identity. Query status between steps. See [Quickstart](QUICKSTART.md#exit-and-recover-stake).
Do not confuse serving-node exit with consensus membership removal.

## Audits and settlement

Assigned auditors receive work through authenticated routing, sign their own
verdicts and require compatible evidence and replay capabilities. Default
execution/audit availability does not waive those requirements. The gateway
cannot choose the assigned reviewer. Keep the resident liveness sender enabled
for hosted identities; see [liveness](https://github.com/emberinfer/Ember/blob/master/docs/AUDITOR-LIVENESS.md).

Freeze a bounded hold, execute, commit signed evidence, resolve sampled audit, then settle using actual token counts. Accepted fees allocate 80% to execution, 15% to audit and 5% back to the caller. Unused reservation is refunded. The live hold timeout is 5,000 blocks; expiry refunds unresolved holds through `expire_hold` and gateway recovery, rather than a wall-clock timer.

HTTP success is not final settlement. Query `/hold/<id>` for actual chain status;
archived holds remain queryable. Missing auditors and transport failures require
recovery/refund, not an invented fraud verdict. Capacity limits and deadlines
must budget tokenization, generation, replay and evidence transfer independently.

## Gateway operation

Dispatch combines reputation, cache affinity and gateway speed observations.
The source records observations after successful submission, before final
settlement. See [scheduling](https://github.com/emberinfer/Ember/blob/master/docs/SCHEDULING.md). Local execution exclusion is an
operator policy, not a consensus ban. Whole-model and sharded candidate pools
remain separate.

MoE generation uses one FIFO per gateway fleet, with eight waiting requests and
10-minute maximum wait by default. Queueing precedes freeze. Full/expired waits
return retryable 503. Separate gateway instances do not coordinate. See
[MoE serialization](https://github.com/emberinfer/Ember/blob/master/docs/moe-serialization.md).

The pipeline daemon is opt-in with `EMBER_PIPELINE_DAEMON=1`; retain the supported
per-request mode (`EMBER_PIPELINE_DAEMON=0`) unless independently validated.
Protect failure stderr artifacts and evidence storage; they may expose sensitive
operational details. See [executor operation](https://github.com/emberinfer/Ember/blob/master/docs/gateway-executor-robustness.md).

## Query current policy

```sh
emberchain query --rpc "$RPC" --path /parameters
emberchain query --rpc "$RPC" --path /validators
emberchain query --rpc "$RPC" --path "/effective-price/$MODEL"
emberchain query --rpc "$RPC" --path "/price/$MODEL"
emberchain query --rpc "$RPC" --path "/hold/$HOLD"
```

The live multiplier corridor is [0.8, 2.0], with at most 10% relative change per
1,000-block epoch. Only settled accepted demand contributes; role shares remain
80/15/5. Coverage is registration coverage, not measured uptime. See [pricing](https://github.com/emberinfer/Ember/blob/master/docs/PRICING.md).
Library defaults and historical example chains are not live parameter snapshots.

## Public ingress and faucet

Authenticate actual endpoint addresses, scheduler keys and TLS pins through the
deployment operator. The repository website is not a live endpoint inventory.
An enabled faucet sends valueless test credit from its funded account with
persistent account/source limits. A trusted proxy must overwrite forwarded client
headers; untrusted headers must not bypass quotas. Keep metrics and administration
private. Verify source preservation and connectivity before advertising a relay.

Pinned gRPC clients use the shared pin/SAN helper; the HTTP relay and upstream
CA-verified TLS leg have separate checks. Certificate rotation changes pins and
requires coordinated configuration updates. See [TLS handling](https://github.com/emberinfer/Ember/blob/master/docs/v11-cert-hostname.md).

## Content policy

Only the signed profile can define supported refusal behavior. A local external
backend moderation setting is not an audit exemption. Check receipt category,
assigned verdict and settled hold. See [profile policies](https://github.com/emberinfer/Ember/blob/master/docs/PROFILE_RELEASES.md#profile-content-policies).
