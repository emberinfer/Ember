# Reasoning request contract (AppVersion 8)

Omitted thinking_enabled preserves the historical max_tokens decimal hash.
Explicit thinking_enabled, thinking_budget and answer_budget use version 2:
`ember-sampling-v2|max_tokens=N|thinking_enabled=true|thinking_budget=T|answer_budget=A`
(or lowercase false). SHA256 of these UTF-8 bytes is SamplingParamsHash.
The signed ExecutionCommitment also carries sampling_params_version=2 and each
parameter so replay can reconstruct the exact prompt and budget boundaries.
Legacy orders retain version 0 and their original bytes.

The gateway accepts these request fields and forwards them in tokenize/infer jobs.
The Transformers adapter applies enable_thinking through the chat template.
Adapters without that capability fail closed. Pipeline reasoning is not yet
implemented and is rejected explicitly; whole-model reasoning is supported.

Budgets must sum to at most max_tokens. OutputTokens remains the total generated
output, including reasoning; ReasoningTokens + ContentTokens equals OutputTokens.
Input, output and total billing are unchanged. Reasoning starts in the template's
thinking region and ends at </think>. Exceeding either independent budget aborts
without a settlement receipt; thinking never borrows answer capacity. Explicit
off rejects reasoning tags, including tags split across token fragments.
Audit replay reconstructs the template from the frozen commitment and counts the
token evidence independently. Ordinary old requests keep their original prompt.
