# Engine SPI v1

Status: normative adapter contract. Architecture and trust:
PRODUCTION_ARCH.md; downgrade/privacy risks: THREAT_MODEL.md.
Ledger consensus decisions in LEDGER_DECISION.md do not change this interface.

## Boundary and lifecycle

The framework may call only adapter load/configuration, capabilities, resource
metadata, prefill, decode, generate, forced-prefix replay and close. It must not
inspect a Transformers model, CUDA tensor layout, llama.cpp context or vLLM
scheduler. Correct output and required evidence under the execution Profile are
the obligations; no particular internal inference implementation is mandatory.

A factory selects an adapter by explicit engine name. Constructors load a verified
local model/shard or bind an explicitly configured external HTTP service. Framework
fetcher verifies local manifest hashes first. Remote service identity/model digest
is an operator trust assumption until a backend attestation protocol exists; do
not claim a remote model alias verifies weight hashes. Selective shard loading is
a separate stage role; full-model HTTP adapters cannot claim it.

Python contract (types in [node/engine/spi.py](../node/engine/spi.py)):

```python
adapter.capabilities() -> EngineCapabilities
adapter.resource_metadata() -> dict
session = adapter.prefill(prompt: str, sampling: SamplingParams) -> DecodeSession
step = adapter.decode(session) -> Step | None
adapter.generate(prompt, seed, count) -> Iterator[Step]  # compatibility convenience
adapter.replay(prompt, seed, tokens: list[int]) -> list[Step]
adapter.close() -> None
```

`prefill` creates request-local state and an absolute monotonic deadline; it may
lazily submit one server-side completion. `decode` returns the next logical token
or None at EOS/limit; remote batching may have computed all tokens already.
Capabilities distinguish native incremental execution from buffered delivery.
A session belongs to one adapter, is single-consumer, and is released on close,
error or exhaustion. Adapters must not allow two sessions to corrupt hidden-state
hooks; the reference local adapter is single-request. Cancellation stops delivery
and closes local iterators; HTTP timeout is bounded but does not prove cancellation
of work already running in a remote server.

`SamplingParams`: seed (signed 64-bit), max_tokens (positive bounded integer),
temperature (v1 supports greedy 0 only), top_k (evidence count), timeout_seconds.
Unsupported sampling/evidence combinations fail before execution; never silently
ignore requested stochastic sampling. Current reference profile is greedy, k=16;
seed is bound for reproducibility even when greedy ignores it. Prompt rendering
stays byte-for-byte compatible with the old gateway (no implicit chat template).
Replay teacher-forces the exact committed token prefix at each position, never
free-generates a replacement. Unsupported replay raises UnsupportedCapability.

## Evidence and capabilities

Step retains the compatibility fields: token (integer ID), text (possibly empty for EOS),
topk (ID/value pairs), activations (named layer -> opening bytes). Additive typed
fields distinguish raw logits from log probabilities. For raw-logit adapters the
legacy committed serialization MUST remain identical. No placeholder activations,
synthetic token IDs, logprobs relabeled as logits, or invented remote device facts.
For a buffered remote response text may be delivered in one Step while other Step
texts are empty; concatenation must equal the exact server completion. Token IDs
and probability rows must still align one-to-one.

| Evidence level | Required data | May satisfy existing Transformers Profile? |
|---|---|---|
| none | Actual token IDs and output text | No |
| topk-logprobs | Actual token IDs, finite normalized logprob candidates per token | No: log-softmax has lost its logit offset; no layer evidence |
| full | Raw top-k logits plus specified layer float32 LE openings and commitments | Only when all pinned runtime/Profile requirements also match |
| mock | Deterministic synthetic evidence, explicitly test-only | Never for a real-weight manifest |

Capabilities include SPI version, engine name/version, device class (cpu/cuda/
pipeline/remote/ mock), numerical format, evidence levels, score kind, maximum k,
forced-prefix replay support, model/shard roles, and native incremental support.
External service version/device/format may be unknown and must say so. Signed
ResourceProfile carries this declaration as an additive nested protobuf message;
absence remains decodable for legacy vectors, but new profiles require explicit
capability checks. Engine version labels are declarations, not attestation.

Existing full-evidence chains require raw logits + activation openings + replay;
llama-server and OpenAI-compatible vLLM HTTP expose a lower evidence grade without
an engine extension. Registering them against a full-evidence chain must fail.
A separate future signed profile may choose topk-logprobs and its weaker guarantees.
Such a profile requires separate review and calibration.

Commitment serialization lives in node/evidence, signing in core-py. Adapters emit
opening bytes; the framework hashes/signs them. KV stays local; migration and replay
use tokens; see [architecture](PRODUCTION_ARCH.md). A shard adapter identifies its layer range and artifact
digest; stage transport wraps its forward operation without assuming model internals
in gateway, tracker, scheduler or account logic. The Qwen2-specific stage executor is a separate reference stage implementation;
see [sharding](SHARDING.md) for its authenticated lifecycle.

## Errors, limits and compatibility

Stable exceptions: UnsupportedCapability (admission error), EngineProtocolError
(malformed/nonfinite/misaligned server output), EngineUnavailable (transport/server
failure), EngineTimeout (deadline), plus explicit memory/resource errors. None is
an automatic fraud verdict. Caller fences failed attempts and releases sessions.
HTTP requests have bounded read/connect timeout and response size. Explicit CUDA
stays strict; auto observes >=16 GiB available before load and <=8 GiB sampled RSS
and allocator budget. These are not kernel-enforced peak limits.

The SPI preserves existing import names, constructors, raw opening encoding, token
selection and full-model CPU execution. Existing protobuf field numbers and
signature domains are unchanged. Capability absence is accepted only for known
legacy profiles. New versioned profiles must fail closed on missing capability.

Versioned numerical auditing adds a new immutable numeric policy/Profile rather than changing legacy exact
ranked-ID behavior. Sparse top-k union comparison must disclose missing-score
estimation; unsupported pairs produce an explicit graded/inconclusive conclusion.

## Contract acceptance

Fixed prompt/seed cases check exact concatenated text, real token IDs, score type,
finite/unique top-k pairs, opening layer coverage, replay declaration, unsupported
sampling, timeout and malformed HTTP failures. Local fake HTTP servers test wire
contracts only. Real Transformers weights plus original real-smoke validate the
reference adapter. Cross-engine output equivalence cannot be claimed from fixtures
or from differently quantized models. Raw engine API sources (accessed 2026-11-08):
`https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md` and
`https://github.com/vllm-project/vllm/blob/main/vllm/entrypoints/openai/completion/protocol.py`.
Wire versions drift; adapters accept documented strict subsets, failing on others.
