# MoE generation serialization

The gateway uses one global FIFO generation queue per Gateway instance (one MoE
fleet). In `node/moe_gateway.py`, the executor builds stage-a and stage-b;
`MoeAssembler.generate` in `node/moe_client.py` visits every stage on every
frame. The attention ranges cover layers 0–7 and 8–15, and
`moeAuthorizations` in `services/gateway/moe.go` pins exactly six fleet nodes.
Every generation therefore shares both stages: no disjoint stage subsets exist.
Deploy one generating Gateway instance per fleet; independent gateways do not
coordinate their queues.

The lifecycle is admission → authorization → tokenize → quote → queue → freeze
→ generate → submit → dispute/settlement polling. Queue waiting happens before
`FreezeMoe`, so it consumes no frozen hold lifetime (`HoldTimeout`), and before
`SubmitMoe`, which starts the dispute window (`DisputeBlocks`). The executor's
`RequestTimeout` context (default 15 minutes) is created after freeze. Tokenization
has its own 30-second timeout before queueing. The background finisher's 30-minute
polling deadline starts after submit. Queue waiting consumes none of these budgets;
the HTTP caller's own context can still cancel it.

The default waiting room holds eight requests, excluding the active generation.
`EMBER_MOE_QUEUE_DEPTH` overrides this nonnegative bound (zero disables waiting).
`EMBER_MOE_QUEUE_WAIT` overrides the maximum wait, default `10m`, using a positive
Go duration string. Invalid settings log once at queue initialization and fall
back to defaults. FIFO uses explicit waiter handoff; cancelled and expired waiters
are removed. The generation owns the slot through executor cleanup and
`SubmitMoe`, then releases it on handler return.

A full queue returns “MoE queue full, retry later”; expiry returns
“MoE queue wait timeout, retry later”. Both use HTTP 503, `Retry-After: 1`, and the
existing `requestError` JSON or streaming error framing. There is no hold yet,
so rejection never calls `CancelMoe`. Client cancellation also exits before freeze.
Set `EMBER_MOE_SERIALIZE=0` to bypass all queue initialization and acquisition.
Other values enable serialization.

Only `serveMoe` generation dispatch acquires the queue. Non-MoE dispatch and audit
paths do not acquire it. `finishMoe` only polls ledger state and audit verdicts;
MoE numeric replay is external, as documented in `cmd/emberd/main.go`. Replay
never enters `serveMoe` and is not serialized by this queue.
