# Scheduling v2

The layer/unit contract and activation commitments are specified in
[SHARDING_SPEC.md](SHARDING_SPEC.md). Deploy these features only on a network with a compatible
configuration. A public network is forthcoming.

## Dynamic layer capabilities

A signed ModelManifest can select `dynamic_ranges`, a positive
`min_stage_layers`, and an optional sorted `allowed_layer_cuts` including 0 and
L. Otherwise legal cuts are multiples of the minimum, plus L. Executed stages
must meet the minimum length. Only complete dense Qwen2 decoder layers are
implemented; MoE and intra-block cuts remain specification only. The cut catalog
is bounded to 4096 entries. The executor checks the profile layer count against
the local model configuration, and the auditor checks it against the reference.

For dynamic profiles, nodes register a signed `ResourceProfile` capability
[a,b), `shard_dynamic_range=true` and unit kind `range`. Endpoint capabilities
include embeddings when a=0 and normalization/head when b=L. The parameter
binding is SHA-256 over `ember/capability-range/v1\0`, manifest root and the
big-endian uint32 range bounds. It commits the held capability, while each
receipt separately commits the range actually executed. A capability is a
stake-backed declaration, not a cryptographic storage proof. Execution and
independent replay supply the work evidence.

```bash
ember-node shard serve --shard-index 0 --range-start 0 --range-end 20 \
  --genesis PROFILE/genesis.pb --manifest PROFILE/manifest.pb \
  --model-path ORIGIN_WEIGHTS --state-dir PRIVATE_STATE \
  --listen LISTEN_ADDRESS --advertise REACHABLE_ADDRESS \
  --cert CERTIFICATE --key TLS_KEY --authorized-scheduler SCHEDULER_ACCOUNT \
  --coordinator SEED --coordinator-tls-fingerprint SEED_PIN \
  --chain-rpc RPC --chain-cli emberchain --ledger-chain-id LEDGER_CHAIN_ID \
  --registration-ttl BLOCKS --device cuda
```

The profile's original artifacts must verify before loading selective layer
weights. Capability registrations use the existing signed, stake-backed,
height-expiring chain registration. One identity has one active profile/range
at a time, as in the original registration model. Range geometry/parameter
binding cannot change during an active lease; renewing the same range can
extend expiry. A different identity can advertise another range concurrently.
Pending holds retain their frozen authorizations even after registration expiry;
stake exposure protects them until settlement/refund.

At request time the scheduler finds minimum-stage covers, rotates among legal
partitions using the request selection counter, and ranks all eligible
capabilities that contain each executed range. A scheduler-signed RPC declares
its executed start/end. The node refuses execution outside its capability or
through a forbidden cut. Receipts name the executed range, capability parameter
binding, profile, token step and activation commitments at the actual boundaries.
Consensus credits executed layer-token work, including only the successful
alternative. It never pays all advertised layers. Pinned-layout profiles keep
their original plan, endpoint ownership and attribution.

The auditor reconstructs the exact executed chain. It replays the forced token
prefix and captures reference activations at every executed cut, in addition
to the signed profile's sampled layers/logits. Fault ownership follows executed
ranges. Non-streaming failover reuses the same frozen range authorization with
another covering capability. Connections are opened lazily inside this retry
path, so an offline alternative does not abort construction of the whole plan.
Already streamed output is not transparently replayed on another node.

## Countersigned latency and policy

Resource profiles expose no accepted speed field for this policy. Executor
`server_elapsed_us`, unsigned aggregate timings, RTT/reputation claims and
`stage_ms` diagnostics do not create latency samples or affect v2 scoring.

The observing auditor/client measures each successful RPC with its own
monotonic start/end clock. It signs a StageLatencyWitness over those timestamps
and the domain-separated digest of the executor-signed ShardStepReceipt. Its
key stays with the observing client, never on an executor. This gives the
executor receipt plus the auditor's countersignature and independently observed
interval. The final assigned auditor's ACCEPT verdict binds the aggregate,
including witnesses, after numerical replay.

Consensus accepts witnesses only from configured auditor accounts distinct from
all authorized stages, checks exact receipt references/signatures and bounded
positive intervals, and creates statistics only at accepted settlement. Only
witnesses signed by the final assigned auditor become samples. A witness by
another valid auditor can remain receipt data without producing a statistic.
The auditor remains a clock authority. The optional consumer cross-check below
detects disagreement with an independent payer observation.

The `/parameters` query exposes immutable `scheduling` configuration:

- `scoring`: `auditor-roundtrip-us-per-unit-v1`.
- `witness_authority`: `assigned-auditor`.
- `percentile_ppm`, `window_samples` and `min_samples` define nearest-rank
  quantiles over the latest verified samples and when a node becomes proven.
- `max_observation_ns` bounds an accepted observation interval.
- `exploration_ppm_per_node` and `exploration_period_requests` define evenly
  interleaved per-node exploration slots; the period must represent the share
  exactly.

Round-trip duration is normalized to ceiling microseconds per executed work
unit (decoder layer; or one standalone embedding/head unit). Data is keyed by
node and immutable model/profile ID. All timings include observed connection,
transport and execution cost; they are not pure FLOP benchmarks. Different
prefix lengths/ranges can affect the distribution. `/latency` exposes receipt
references, raw observed intervals, units, normalized samples and the exact
configured percentile. Original signed pairs remain in each settled pipeline
receipt, so independent code can recompute the statistics.

For proven nodes, lower measured percentiles rank first. Exploration gets its
configured slots even when a slow node is proven; unproven nodes also receive
these slots. With no proven candidates the policy uses round robin. If the
requested per-node shares exceed available slots, it uses round robin for the
whole pool. Node IDs define deterministic exploration order. A gateway request
counter starts at zero on process start; full configured periods provide the
stated share while the candidate set remains stable. Failures/changes in that
set are availability events, not invented timing samples. Legacy scoring is
used when no chain scheduling policy is configured.

The first implementation gathers countersigned timing for pipeline execution.
Whole-model paths without these witnesses remain unproven under this policy;
no self-reported whole-model speed is substituted. A reference evaluation
configuration uses two replicas of the same profile/range, four warm-up requests,
then a
separate 30-request window with P90 and a 20% per-node exploration share.

## Consumer latency cross-check

Gateways record their own monotonic request interval, from admission/planning
through inference completion, excluding asynchronous audit and settlement time.
For pipelines they also time each successful stage RPC between flushed start/end
events from the local pipeline client. Failed attempts do not become samples.
The gateway signs an `observe_latency` transaction with the payer's key after
receipt submission and before audit settlement. The aggregate receipt reference
and every ordered stage receipt digest must match exactly; positive stage
intervals must fit inside the request interval and the scheduling bound. A
whole-model observation has no invented stage intervals. Observations cannot be
overwritten. Holds retain observer identity and the complete signed transaction
for independent verification. `/state` and `/hold` expose this provenance.

`scheduling.consumer_cross_check` enables the consensus rule:

- `max_ratio_ppm`: greater than 1,000,000, at most 100,000,000. An observation
  disagrees when the larger raw interval exceeds the smaller times this ratio.
- `min_samples`: positive and at most `window_samples`, per executor/model/auditor.
- `divergence_ppm`: the minimum disagreement fraction, in 1..1,000,000.

Accepted pipeline settlement requires an independent payer observation when
enabled, and auditor selection excludes the payer as well as all authorized
executors. Receipt submission and observation alone have no scheduling effect.
At accepted settlement, the existing bounded sample window is grouped by final
assigned auditor. A pair diverges when both the sample count and disagreement
fraction thresholds are met. Only that pair's samples use consumer-observed
microseconds per work unit in the effective percentile. Other pairs retain their
auditor intervals. The scheduler reads this effective percentile using the
existing exploration rule. Rolling-window agreement restores auditor primacy.
`/latency` exposes both the raw auditor percentile and effective percentile,
signed receipt references, both intervals, and per-auditor counts and flags.

A reference GPU policy uses a 4x ratio, four observations, 75% disagreements,
an eight-sample window, P90, and 20% per-node exploration. An executor/auditor
pair meeting the divergence thresholds falls back to
consumer-based ranking.

Omitting the optional cross-check preserves the legacy serialization and rule.
Activation is immutable genesis policy and requires coordinated chain rollout.
Whole-model observations are retained but remain unranked without stage witnesses.
These are authenticated observations, not proofs of elapsed time: the submitting
gateway/payer is trusted to measure honestly. A payer colluding with both other
roles, malicious local pipeline code, network asymmetry, and identity changes
remain outside this defense. Ratio calibration must allow transport overhead.

## GPU admission and inherited bands

GPU pipeline profiles use the signed `rank-probability-v1` bands from the
[GPU profile family](GPU_EXECUTION.md). Every stage must request CUDA explicitly
and report a recognized accelerator class plus exact Transformers, torch and
CUDA versions. The reference measures its own CUDA runtime. Every executed
stage/reference pair must have an enforced signed band, and all selected bands
must have the same threshold. A mixed pipeline needing different thresholds
fails closed; a single aggregate threshold cannot widen a stricter stage.
The existing full-model calibration does not prove every composition or cut:
each proposed layout/cut needs its own reproducible execution and replay checks.
CPU remains supported with its separately signed policies.
