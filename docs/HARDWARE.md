# Hardware Sizing

Sizing is per immutable model/Profile and evidence grade. Measure the complete
process, tokenizer, KV cache, hidden-state buffers, and audit overhead; model file
size alone is not sufficient.

Ledger nodes need durable consensus/application storage and no model GPU for
consensus. Gateway sizing must include queues, evidence storage and any hosted
replay worker. Serving/audit sizing depends on the loaded weights, context and
concurrency. No universal core/RAM/disk minimum is established by the source.
Measure representative workloads and retain capacity headroom before admission.

## Small-device shard guidance

Choose a shard only after measuring its peak resident memory. A practical initial
budget is at most 60% of currently available memory for weights plus runtime
buffers, leaving at least 25% host headroom and the remainder for KV/evidence
growth. Reduce layer count when long-context KV growth or hidden-state transfer
breaks that bound. WAN shard performance has not been established by the LAN pipeline. One
hidden-state round trip per generated token remains a material constraint.

For a candidate shard, run representative short and long prompts, record peak RSS,
first-token latency, steady token latency, transfer bytes, and failure recovery.
Admission must use the measured Profile, not a device marketing capacity figure.
