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

The gateway lazily maintains two exclusive Python daemon slots per interpreter
and working directory. Admission defaults to eight concurrent requests, so two
jobs can run while further admitted requests wait within their request timeout.
Each daemon processes newline-delimited jobs using the one-shot execution path,
retaining tokenizers, private-key objects and authenticated stage channels
(up to 64 channels retained after each job, evicting least recently used).
A `job_end` marker terminates each job and is consumed by the manager, never SSE.
Failed Python jobs close all cached channels. A process crash errors the current
job; a timeout kills and reaps that process. The next job starts a fresh process.
No failed job is automatically replayed. Interpreter startup failure attempts the
legacy subprocess path. Set `EMBER_PIPELINE_DAEMON=0` on the gateway to force the
legacy one-process-per-request behavior. Stop the gateway to stop its residents.

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
