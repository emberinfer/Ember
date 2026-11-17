# Segmented pipeline runtime

Stages retain up to eight LRU sessions, expiring after 600 idle seconds. A
session owns KV tensors and (except at the head) full boundary output history;
its sequence length is bounded by the model context limit. Budget memory for
eight context-length sessions per process, rather than eight single tokens.
Expired entries are reclaimed on the next forward. Stage execution stays
serialized. Session identity includes scheduler, request, executed specification
and scheme. Restarts, skipped/backward steps and changed prefixes recompute from
the full request. Head continuations whose top-two margin is within two BF16
relative quantization units (at least 0.25 logits) also recompute the head stage
from its full received input; this avoids observed greedy divergence at a
near-tie on the acceptance prompt. Switching candidates therefore requires no state transfer.

`Qwen2ShardEngine.forward` remains stateless unless `use_cache=True`; that mode
returns `(output, past_key_values)` and accepts an absolute `position_offset`.
The head returns only `[batch,1,vocabulary]`. Hidden boundary tensors, signed
requests, receipts, activation hashes and protobuf definitions are unchanged.

The gateway uses the legacy one-process-per-request path by default.
`EMBER_PIPELINE_DAEMON=1` opts into the retained experimental resident path;
`EMBER_PIPELINE_DAEMON=0` explicitly keeps the supported per-request mode.
Do not infer production enablement from daemon lifecycle tests. The optional
manager has exclusive slots and consumes `job_end` internally. Failed jobs,
read/protocol errors and cancellation discard the process. No failed execution
is automatically replayed. See [executor operation](gateway-executor-robustness.md).

Stages and gateway can roll independently: new stages accept old clients, and
new clients/daemons work with old stages. Cache misses and audit replay always
receive the full input. No protobuf or chain migration is needed. BF16 cached
execution can differ in low bits; validate each deployed runtime's calibrated
audit band and greedy output before enabling it broadly.

For an isolated CPU benchmark, with repository dependencies on `PYTHONPATH`, run
`python node/bench_segengine.py --output .runtime/segengine/after.json` from the
repository root. It uses only ports 49711/49712 (checked before binding), ephemeral
TLS/scheduler/stage identities, two CPU stage processes with two threads each,
and the local Qwen2.5-1.5B model. It also exercises sequential resident jobs and
recovery after a rejected job. It never registers stages or contacts a gateway.
