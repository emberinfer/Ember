# Gateway dispatch and sharded scheduling

## Current gateway selection

The scheduler filters compatible profile, range, stake, capacity and availability
before dispatch. Reputation and session/cache affinity contribute to selection;
speed-weighted selection is a separate gateway policy. An affinity match takes
priority in the weighted picker. With speed enabled, 10% of non-affinity choices
explore uniformly and the rest use clamped speed weights. An explicitly configured
chain latency policy takes its own selection path. See `services/scheduler/speed.go:37`.

Whole-model and sharded requests never share a candidate pool. Speed cohorts are
keyed by execution profile and form, with range bounds for shards. Gateway-local
execution exclusions filter dispatch only; they are not consensus exclusions or
audit-sampling penalties.

## Measured speed

The gateway measures generation duration and time to first token; shard timings
come from its pipeline subprocess around successful forward RPCs. Decode medians
are token-weighted. TTFT medians weight requests equally and compare node medians
within the cohort. Scores are clamped to [0.5, 2.0]; low-confidence data is neutral.
The normal window is 24 hours, with a 72-hour fallback when the cohort has fewer
than ten observations and hard expiry after 72 hours.

Decode confidence requires at least five requests and 256 tokens. TTFT confidence
requires five requests. Samples under four output tokens do not record; decode
also requires at least 500 ms measured duration. The two score components combine
and clamp. These are observations, not proofs of elapsed time or hardware speed.

The current source records whole-model and pipeline speed after successful receipt
submission and optional latency observation, **before final audit settlement**.
Do not describe it as settled-only data. It never changes trust reputation or
consensus audit sampling. Source: `services/gateway/gateway.go:426`,
`services/gateway/pipeline.go:390`, `services/gateway/speedstats.go:63`.

## Dynamic ranges

Signed capabilities identify held layer range, profile, endpoint ownership and
capacity. A capability is a stake-backed declaration, not a storage proof. The
scheduler can cover a request with legal contained executed ranges, subject to
manifest minimum granularity/cuts and local memory admission. Receipts bind the
executed range and activation boundaries. Credit follows verified executed work,
not advertised capacity. Frozen alternatives may support same-range failover
before streaming starts; already streamed output cannot be silently spliced.
See [sharding](SHARDING.md), [unit specification](SHARDING_SPEC.md) and
[range fit](../node/agent/RANGE_FIT.md).

## Consensus latency policy

The source also implements optional signed stage-latency witnesses and consumer
cross-checks. They are distinct from gateway-local speed scores. Witnesses bind
the executor receipt and observer interval; only accepted settlement produces
consensus latency statistics under the configured policy. Consumer observations
can detect configured divergence but still depend on honest clocks and identities.
Whole-model observations do not invent stage witnesses. See
`services/chain/latency.go` and `services/scheduler/latency.go`.

## MoE dispatch

OLMoE layer + expert sharding is live. MoE generation is serialized per gateway
fleet using a bounded FIFO, independent of audit replay. Independent gateways do
not coordinate queues. See [MoE serialization](moe-serialization.md).
