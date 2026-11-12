# Spark token pricing

A model's signed `ChainGenesis` commits three nonnegative integer prices in
Spark subunits (`1 Spark = 1,000,000 subunits`):

| Parameter | Meaning |
| --- | --- |
| `price_input` | Charge per token in the tokenized prompt, including the chat template |
| `price_output` | Charge per generated token |
| `price_context` | Rate for prompt-token × generated-token interactions, per 1024 interactions |

For input count `N_in` and actual output count `N_out`, consensus charges:

```text
input   = price_input × N_in
output  = price_output × N_out
context = floor(price_context × N_in × N_out / 1024)
fee     = input + output + context
```

The divisor **1024** is a protocol constant. The context term accounts for
decoding against the input KV context. Arithmetic uses exact integer
intermediates and rejects charges that exceed signed 64-bit accounting limits.
Each generated token counts, including a generated end-of-sequence token.

## Quote before inference

`GET /v1/pricing` returns the gateway's model/profile, the three prices, the
divisor and the quote path. These prices must match the signed profile anchored
in the consumer's selected chain; the signed profile is the authority.

Send the same authenticated JSON body used for `/v1/chat/completions` to
`POST /v1/quote`. The gateway formats the same chat template and obtains the
exact count from the serving tokenizer. A quote creates no hold and generates
no output. For example:

```json
{"model":"ember-example","messages":[{"role":"user","content":"Hello"}],"max_tokens":100}
```

The response itemizes the worst-case fee:

```json
{"model":"ember-example","input_tokens":33,"max_output_tokens":100,
 "components":{"input":66,"output":300,"context":3,"total":369},
 "freeze":369,"context_divisor":1024}
```

This example uses prices `2`, `3`, `1`. The quote is for that prompt, profile
and output cap. A changed prompt or cap requires a new quote. Quotes use normal
authentication, admission and rate limits, and require the serving tokenizer
to be available. Adapters without exact token counting fail closed.

## Freeze, verify, settle

For profiles charging input or context, the serving stack tokenizes before
funds are frozen. A signed `ExecutionCommitment` binds the request ID, prompt
hash, model/profile, sampling parameters, input count and maximum output count.
The whole-model executor signs its commitment. The pipeline coordinator signs
the commitment obtained with the same tokenizer used by the pipeline client.

Consensus freezes the signed pricing policy and commitment in the hold and
verifies the reserve by evaluating the fee formula at `max_output_tokens`:

```text
freeze = price_input × N_in + price_output × max_out
         + floor(price_context × N_in × max_out / 1024)
```

Multiplication precedes division. Flooring `price_context × N_in / 1024`
before multiplying by `max_out` can under-reserve; it is not a safe freeze
bound. The expression above gives the exact worst-case charge, including
fractional-rate cases.

The execution receipt must match the committed input count and stay within
the output cap. Whole-model receipts also reference the exact signed
commitment. Consensus independently recomputes the charge on receipt submission
and again at final settlement. A mismatched submitted amount is rejected.
Independent reference replay also checks the prompt's token count for these
profiles. Tokenization is attested and audited; validators do not run the
tokenizer or model themselves.

After the existing audit/dispute flow, acceptance charges the actual output
count and releases `freeze − fee`. Cancellation, failure, exposure limits and
slashing retain their existing rules. Input fees are part of accepted execution
settlement; a cancelled request is refunded under the existing rules.

Role shares and scarcity multipliers redistribute within this fee. Their sum
equals the fee; pricing issues no new Spark and does not draw from the subsidy
pool. A shard/range pipeline splits the entire fee using the existing executed
work-unit weights and largest-remainder allocation, including its existing
tie-break rule. Pricing does not change those weights.

## Compatibility and rollout

With `price_input = price_context = 0`, the formula is exactly
`price_output × N_out`, and the reserve is exactly `price_output × max_out`.
The new protobuf fields default to zero and are omitted from serialized
profiles/commitments. Existing signed profiles, signatures and legacy receipt
bytes remain valid. The legacy generation path preserves its receipt format;
no input charge is inferred from its historical whitespace count. Newly
created holds for anchored output-only profiles also verify their output fee
against the signed policy. Historical holds without pricing snapshots retain
their original transition rules.

Validators and serving components must support the new fields before a priced
profile is enabled. The gateway requires the consensus ledger backend for
input/context pricing; the legacy SQLite backend refuses it. Signed profiles
are immutable: publish a new profile to introduce prices. This implementation
does not change the parameters of any existing public profile or upgrade a
running public network.

## Input-weighted auditor compensation: specified follow-on

Status: specified follow-on 2026-11-11. Pricing v2, described above, is now merged
and supplies the authenticated token
counts and fee calculation. This section preserves that design; it does not
activate an input-weighted auditor payment rule.

The current whole-model scarcity policy splits an existing request fee among
executor, auditor and consumer roles. Its auditor payment goes to the final
assigned auditor. The current pipeline settlement credits executed stage owners
and has no corresponding auditor-pool split. The follow-on must use pricing v2's
authenticated token counts and fee calculation consistently across both paths.
It must not infer that today's pipeline code already pays a 15% pool.

### Fee boundary and authoritative work

For an accepted request r, let F_r be the actual fee calculated by pricing v2 from
the committed input/output counts and frozen model pricing. Preserve that
policy's executor pool E_r, auditor pool A_r and consumer allocation C_r:

```
E_r + A_r + C_r = F_r
```

The auditor allocation below redistributes A_r only. It does not increase the
fee, borrow from another request, consume subsidy, or change the executor and
consumer allocations. A nominal 15% auditor role can be affected by the existing
scarcity policy and integer rounding; use its actual integer pool, not a second
independent 15% calculation. In a pipeline, apply the auditor-role split before
allocating the remaining executor pool by executed work.

An eligible audit work item must be part of a consensus-validated assignment and
signed verdict bound to the exact receipt, model/profile, challenge and execution
scope. Specify a domain-separated work ID committing to the receipt reference,
audit round, assigned auditor and audited scope (whole model or exact stage/range).
Any new work/count metadata must be committed before consumer observations are
signed, so the consumer-observation scheme continues to bind the final aggregate receipt.

Each work item w supplies N_w, its committed input-token length, validated from
the underlying pricing v2 execution receipt and assignment rather than an auditor's
claimed wall time, claimed GPU cost or unsigned gateway counter. The model's
pinned tokenizer and prompt commitment determine the count. Whole-model work
uses the request's committed input count. Pipeline stage work inherits that
count unless the future execution protocol independently authenticates a
different input scope. Replaying the same assignment or resubmitting its verdict
must not create additional work. Multiple token/layer checks within one assigned
replay job are one item, because they share prefix reconstruction.

If there are multiple explicitly assigned replay jobs, each distinct job may
count once for its assigned auditor. Executor identities and unassigned or
invalid signatures are ineligible. A confirming auditor can be paid for its own
distinct assigned job; duplicate deliveries of that job cannot be paid twice.
The assignment policy must bound job count and scope before execution to prevent
an auditor from manufacturing work to increase its weight. Prefix-rebuild cost
also depends on forced output prefix and layer range; this version weights only
authenticated input length. A later cost model must explicitly version any such
additional factors and cannot silently substitute self-reported timings.

### Deterministic allocation

Aggregate valid assigned work by auditor account a:

```
W_a = sum(N_w for unique eligible work items assigned to a)
W = sum(W_a)
q_a, remainder_a = divmod(A_r * W_a, W)
left = A_r - sum(q_a)
```

Use unbounded intermediate integer arithmetic. Give one additional fee subunit
to each of the first `left` accounts ordered by descending remainder, breaking
ties by ascending canonical account ID. Aggregate accounts before rounding;
work-item order and transaction order must not change the result. Persist the
pool, work IDs/counts, account weights, quotients, remainders and final payments
in queryable settlement state so anyone can recompute them.

Only positive, bounded committed input counts are eligible. A zero auditor pool
pays zero and needs no rounding. A positive auditor pool with no valid positive
work must fail accepted settlement; use the existing unresolved-audit/service-
failure route rather than quietly crediting a coordinator. The fee must fit
within the reservation. Credit overflow must reject the whole transaction
atomically. The required conservation identities are:

```
sum(auditor_payments) = A_r
E_r + sum(auditor_payments) + C_r = F_r
reservation_refund + F_r = reserved_amount
```

Current rejected/service-failure requests receive a full request-fee refund.
They therefore have no positive request-fee auditor pool under this follow-on.
Paying dispute reviewers or rewarding confirmed detection from another source
requires a separately specified funding rule; the payer-directed slash cannot
silently become an auditor fee.

### Examples and acceptance fixtures

These are design arithmetic fixtures. Consider
two requests with 16 output tokens each, input lengths 64 and 4,096, illustrative
input/output prices 1 and 10 subunits, and role weights 70/15/15 with no scarcity
adjustment. The existing largest-remainder role rule uses executor/auditor/
consumer order for ties:

| Request | Input | Output | Fee | Executor pool | Auditor pool | Consumer |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Short | 64 | 16 | 224 | 157 | 34 | 33 |
| Long | 4,096 | 16 | 4,256 | 2,979 | 639 | 638 |

One assigned full-model auditor receives 34 and 639 respectively. If two auditors
each replay the same full input, their weights are equal within each request:
17/17 for Short, 320/319 for Long (the smaller account ID receives the extra
subunit). The absolute increase across requests comes from pricing v2's input-aware
fee and thus its larger auditor pool. Input weighting alone cannot increase a
single auditor's payment from a fixed output-only fee; this dependency is why
the allocator depends on the pricing v2 fee boundary.

For the allocator's unequal-work fixture, take an already fixed pool of 975 and
two valid work weights 64 and 4,096. Payments are exactly 15 and 960, summing to
975. This tests the general work allocator, not a claim that a current homogeneous
whole-model request contains differing input lengths. Differing weights require
independently authenticated work scopes or differing assigned work-item counts.
An integration must never let an auditor invent shorter or longer scopes just
to satisfy the fixture. With pool 1 and equal weights, the smaller account ID
gets 1 and the other gets 0; no fee subunit disappears.

Implementation acceptance requires the two equal-output/different-input request
fixtures, within-request unequal-weight and tie fixtures, full conservation and
reservation checks, whole-model and pipeline paths, single/zero-pool cases,
int64-boundary arithmetic, invalid or altered count commitments, duplicate work,
unassigned signers, conflicting scopes, independent confirmation and refund
paths. Tests must distinguish changing the total pool through input pricing
from redistributing an existing pool among auditors. GPU measurement can later
calibrate a richer cost model but cannot authorize an unsigned weight.

### Integration and activation

Implement against the merged pricing v2 receipt and pricing APIs. Expected reconciliation points are chain state,
whole-model/pipeline settlement, receipt validation, gateway fee reservation,
receipt-bound consumer observations, and the attributed-revenue basis. Introduce
explicit policy/schema versions and coordinated validator activation for any
consensus change. Preserve historic settlements and their old rule. This design
leaves the current runtime payment behavior in place until that integration is
reviewed and tested.
