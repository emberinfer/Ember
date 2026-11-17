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
# For vLLM: name='vllm', endpoint='https://your-server', model='<served-model-name>'.
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
form. llama.cpp buffers completions. vLLM streams by default and uses incremental
SSE reading; `EMBER_VLLM_STREAM=0` restores buffering. Fallback is allowed only
before emitting a step, never after a stream has started. Buffered delivery can
put all response text in the first step and empty text in subsequent steps;
concatenation must reproduce the response exactly. A streaming API does not
remove the requirement to return real token IDs and the profile's evidence.

Pass `api_key` to `load_engine` from a secret store/environment, never a committed
file. The join command reads `EMBER_ENGINE_API_KEY`. It accepts `--engine llamacpp`
or `--engine vllm` plus `--engine-endpoint`, `--engine-model`, `--engine-version`,
but full-evidence profiles still reject adapters missing raw logits, layer openings or replay. This is the
intended outcome: these HTTP APIs do not supply full activation/raw-logit evidence. vLLM does implement teacher-forced scoring via prompt logprobs for compatible equality profiles. The live 35B unified profile uses vLLM on GPU nodes.
Direct adapter use is available for external-engine contract/integration work.

Remote device, numeric format and version default to remote/unknown; an operator
version label is a declaration, not attestation. A model alias does not verify
remote artifact hashes. No cross-engine numerical equivalence or trustworthy
inference settlement follows from local fixture tests. Raw logits and normalized
logprobs are different evidence types and have different commitments.

The public adapter tests use synthetic HTTP fixtures; passing them does not
establish real-server compatibility. See the [changelog](../CHANGELOG.md) for
release changes.
