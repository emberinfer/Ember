# Running Engine SPI v1 adapters

Reference contract: ENGINE_SPI.md. No engine server is installed or launched by
these adapters. `ember-node join --engine transformers --model-path <verified-dir>`
uses the existing full-evidence contract. Run the adapter contract tests
from the repository root after installing the reference Python dependencies:

```sh
PYTHONPATH=core-py:proto:node python3 -m unittest discover -s node -p test_engine_spi.py -v
EMBER_NODE=/path/to/verified-release/bin/ember-node bash spi-kit/run-conformance.sh
```

Direct adapter use against a separately operated, compatible HTTP endpoint:

```python
from engine import load_engine, SamplingParams
engine = load_engine('llamacpp', endpoint='http://127.0.0.1:8080',
                     model='Qwen2.5-1.5B-Instruct-GGUF')
# For vLLM: name='vllm', endpoint='https://engine.example.org', model='<served-model-name>'.
session = engine.prefill('hello', SamplingParams(seed=7, max_tokens=8))
try:
    while (step := engine.decode(session)) is not None:
        print(step.text, end='')
finally:
    engine.close()
```

llama.cpp uses native `/completion`, `return_tokens=true`, `n_probs`,
`post_sampling_probs=false` and the `completion_probabilities[].top_logprobs`
ID/logprob schema. Older `probs` schemas/probability values are not silently
converted. vLLM uses `/v1/completions`, `logprobs` and
`return_tokens_as_token_ids=true`; token strings must have `token_id:<integer>`
form. Both are buffered completions; concatenated text is exact, first Step
contains response text and subsequent Steps contain empty text. TTFT measured
through these adapters includes the entire buffered completion.

Pass `api_key` to `load_engine` from a secret store/environment, never a committed
file. The join command reads `EMBER_ENGINE_API_KEY`. It accepts `--engine llamacpp`
or `--engine vllm` plus `--engine-endpoint`, `--engine-model`, `--engine-version`,
but **rejects** them on all currently published full-evidence chains. This is the
intended outcome: these HTTP APIs cannot provide activation openings, raw logits
or teacher-forced replay. No lower-evidence production chain is supplied by these adapters.
Direct adapter use is available for external-engine contract/integration work.

Remote device, numeric format and version default to remote/unknown; an operator
version label is a declaration, not attestation. A model alias does not verify
remote artifact hashes. No cross-engine numerical equivalence or trustworthy
inference settlement follows from local fixture tests. Raw logits and normalized
logprobs are different evidence types and have different commitments.

The public adapter tests use synthetic HTTP fixtures; passing them does not
establish real-server compatibility. See the [changelog](../CHANGELOG.md) for
release changes.
