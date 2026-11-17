# Operator contribution limits

A node can restrict the model chain IDs it offers, the number of simultaneous
jobs for each ID, and the total number of simultaneous jobs across IDs. Limits
control admission; changing them never cancels work already assigned. A profile
ID in this interface is a model's `chain_id`, rather than a device class or an
execution format version. Each running engine advertises its own loaded profile.

## Register and update

Include `caps` in the signed `register_node` action, or send an owner-signed
`update_caps` action through the existing transaction CLI:

```sh
emberchain tx --rpc http://127.0.0.1:26657 --key node.key \
  --chain-id example-network --nonce 12 \
  --action '{"type":"update_caps","caps":{"profiles":{"example-model":1},"global":1}}'
```

Use the account's next nonce. A missing profile key refuses all new work for
that profile. An empty `profiles` object pauses new contributions. Zero means
unlimited for a listed profile or for `global`; omission of `global` means zero.
Legacy registrations without `caps` retain unlimited admission. Renewing a
registration without `caps` preserves its existing limits.

An update included at height H has `caps_effective_height: H+1`. Query
`/node/<node_id>` to inspect `caps`, `pending_caps`, and this height. Assignments
at H use the old limits, including transactions later in the same block. All
assignments at H+1 use the new limits. A second pending update is rejected until
activation. New registrations carrying limits follow the same boundary.

The chain atomically counts reservations from accepted freezes, pipeline stage
authorizations, and pending auditor work. Execution slots remain occupied until
a receipt is submitted, the hold is cancelled, or its timeout is reached. Pending
audit slots remain occupied until the verdict or finalization. Each pipeline stage
occupies a slot, even if a node executes multiple stages of the same request.
Global slots include execution and audit work. The profile identity is pinned
with the assignment. Lowering a limit below current occupancy prevents new work
until enough existing reservations finish. A failed admission changes neither
balances nor offense records. Fault handling for accepted work remains unchanged.

Schedulers filter registered caps against current reservations and intersect
that result with the live signed qualification signal. Consensus checks again
when accepting work, so simultaneous schedulers cannot overbook a slot. A race
at admission causes the gateway to try another eligible node. Privacy migration
also checks the target's limits before accepting a rewrap.

## Presets and custom settings

`ember-node join` accepts `--contribution low|balanced|full`,
`--contribution-config policy.json`, `--contribution-concurrency N`,
`--vram-budget-bytes N`, and `--contribution-context-tokens N` (default 512).
`full` is the default. Configuration is local convenience; the resolved numeric
caps are included in registration and its signed runtime profile.

The configuration's `profiles` array explicitly orders the available model
chain IDs from smallest to largest. The default catalogue contains only the
current engine's profile. `capacity` is the operator's full concurrency setting
(default 1); memory capacity comes from the CUDA device's measured total bytes.

| Preset | Offered profiles | Per-profile concurrency | Global concurrency | Memory budget |
| --- | --- | --- | --- | --- |
| `low` | First (smallest) catalogue entry | 1 | 1 | Unlimited |
| `balanced` | Entire catalogue | max(1, floor(capacity / 2)) | max(1, floor(capacity / 2)) | floor(device total bytes / 2) |
| `full` | Entire catalogue | Unlimited (0) | Unlimited (0) | Unlimited (0) |

Integer division rounds down; concurrency has a minimum of one. For example,
capacity 7 and device memory 101 bytes resolve to concurrency 3 and budget 50.
That example specifies rounding only, not a hardware measurement. A backend
without a measured device capacity requires an explicit budget for `balanced`.
Custom values replace the corresponding preset values, including an empty
profile set or zero for unlimited. CLI concurrency and budget override config.

```json
{
  "preset": "balanced",
  "profiles": ["small-model", "large-model"],
  "capacity": 8,
  "overrides": {
    "profiles": {"small-model": 2},
    "global": 2,
    "budget_bytes": 4294967296
  }
}
```

The listed budget is an operator setting, not a claim about model occupancy.

## Measured memory admission

For CUDA Transformers engines, startup probes a real forward pass at the chosen
maximum context, after loading the actual model. It measures loaded parameter
storage, returned KV tensor storage, and peak CUDA allocated or reserved bytes (whichever is larger), including
forward workspace and allocator blocks. `ResourceProfile.contribution` signs `weight_bytes`,
`kv_bytes`, `peak_bytes`, `context_tokens`, `budget_bytes`, concurrency and an
`unavailable` reason. No model-size formula supplies these numbers.

A finite budget admits a profile only when it covers both the observed peak and
shared weights plus one maximum-context KV reservation per allowed simultaneous
job. Finite budgets require a finite concurrency setting. Input plus maximum
output tokens must fit the measured context bound, including forced replay.
The node refuses work before generating a receipt when qualification or a local
slot is unavailable, using an `unavailable` tunnel frame rather than an offense.

Backends without a measurement implementation, including remote HTTP engines
and CPU engines under a VRAM budget, signal unavailable. Failed memory probes
also fail admission closed; they never substitute estimates. An unlimited
budget does not require a memory probe. Use a separate engine process and
registration for each loaded profile; catalogue order never claims an unloaded
model is present. Independent node identities have independent budgets, so an
operator sharing one GPU must divide its available memory between processes.

## Network upgrade

Contribution limits are implemented in the current versioned state machine. The live chain runs AppVersion 12; the source base constant is 11. LoadState accepts supported versions 4 through 12. Follow the coordinated [upgrade procedure](UPGRADE.md) rather than treating the historical introduction version as the live version.
