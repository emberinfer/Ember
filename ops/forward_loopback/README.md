# Local forwarding measurement

This opt-in harness starts a synthetic 200 token/s engine inside the real Python
`Agent.serve`, a local TCP gRPC `services/tunnel` server, the real Go gateway,
and a local TCP HTTP SSE reader. It creates only temporary ledger/evidence data.
It never contacts a production endpoint or starts a model server.

From the repository root, with a Python environment containing the node dependencies:

```sh
EMBER_LOOPBACK_PYTHON=/path/to/python go test ./services/gateway -run '^TestForwardLoopback$' -v -count=1
EMBER_FORWARD_TRACE=1 EMBER_LOOPBACK_PYTHON=/path/to/python go test ./services/gateway -run '^TestForwardLoopback$' -v -count=1
```

Three trials each deliver 400 tokens. `delivered_tok_s` uses `(tokens - 1)` over
first-to-last SSE token time; `total_s` includes admission, execution, evidence,
and receipt processing. The fake engine schedules tokens at absolute 5ms deadlines
so generator scheduling drift does not disguise forwarding overhead.

`EMBER_FORWARD_TRACE=1` logs one agent and one gateway summary per execution
attempt, with counters, timestamps, elapsed time and milliseconds per token.
Agent timestamps are monotonic nanoseconds on that host; gateway timestamps are
Unix nanoseconds. Compare durations rather than subtracting clocks across hosts.
No prompt/token text is logged. The flag is off by default.

The harness measures synthetic forwarding only. It cannot establish model
throughput or diagnose a deployed engine without its configuration and trace.
vLLM now supports incremental SSE by default; llama.cpp retains buffering.
Transformers and sharded execution have separate generation costs. Do not infer
the live 35B path from historical adapter behavior.

For a slow production request, compare engine inter-arrival time, queue residence,
and gateway write/flush time. High engine inter-arrival time locates the delay
upstream of forwarding. High write/flush time identifies downstream backpressure.
If the backend reader buffers SSE, its fix is incremental event consumption with
no per-event sleep/batching timer and prompt flushing to its caller. A separate backend reader must be inspected in its own source; no behavior of an external application is asserted here.
