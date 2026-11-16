# Hardware Sizing

Sizing is per immutable model/Profile and evidence grade. Measure the complete
process, tokenizer, KV cache, hidden-state buffers, and audit overhead; model file
size alone is not sufficient.

| Role | Initial sizing estimate | Notes |
|---|---|---|
| Validator | 2 CPU cores, 4 GiB RAM, 20 GiB durable disk | Add disk for block/state retention and backups; no GPU is required. |
| Gateway/seed | 4 CPU cores, 8 GiB RAM, 20 GiB disk | Audit replay can require model-class memory unless delegated to a separate auditor. |
| Full-model node | model working set plus 30% headroom | Runtime admission guards vary by device/Profile; long contexts can exceed these estimates. |
| Monitor | 1 CPU core, 1 GiB RAM | Must reach each validator RPC or an authenticated collector. |

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
