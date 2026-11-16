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

On the task machine, the unchanged forwarding path delivered 200.00/200.01/200.00
tok/s. With instrumentation on it delivered 199.99/200.00/199.99 tok/s. Agent
queue residence averaged 0.035–0.045ms/token and gateway SSE write/flush averaged
0.025–0.032ms/token. This rules out a fixed per-token 0.5s queue wait or 125ms
forwarding timer on this path. A queue timeout is an idle wait, not a pacing delay.

No forwarding pacing fix is justified by this result. It does not establish the
cause of the production slowdown: production engine execution, topology, network,
and backend reader are not reproduced. The HTTP vLLM/llama.cpp adapters explicitly
buffer a non-streaming completion; that affects first-token latency and produces
a burst, not a steady 7–8 token/s forwarding cadence. Transformers uses full-prefix
`use_cache=False` inference and shard pipelines perform synchronous stage forwards
per decode step; those are engine execution paths, not SSE pacing. Do not attribute
the 35B deployment to any of them without its actual configuration and trace.

For a slow production request, compare engine inter-arrival time, queue residence,
and gateway write/flush time. High engine inter-arrival time locates the delay
upstream of forwarding. High write/flush time identifies downstream backpressure.
If the backend reader buffers SSE, its fix is incremental event consumption with
no per-event sleep/batching timer and prompt flushing to its caller. That backend
is outside this repository and was neither inspected nor changed here.
