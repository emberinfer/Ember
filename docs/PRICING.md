# Current token pricing

1 Spark = 1,000,000 integer subunits (`sub`). Testnet Spark is valueless test credit.
Current intrinsic model prices, in sub per 1,000 tokens, are:

| Model | Input | Output |
| --- | ---: | ---: |
| Qwen2.5-1.5B | 229 | 234 |
| Qwen3.6-35B-A3B FP8 | 458 | 571 |
| OLMoE-1B-7B | 195 | 217 |
| Kimi K3.1 Pro | 16660 | 25630 |

Kimi is published but has no serving nodes. A price does not establish availability.

## Intrinsic derivation and publication

Using consistently scaled F (FLOPs per token) and T (total parameters), the protocol derivation is:

```text
output = pc*F + ps*T
input  = pc*F + (1/8)*ps*T
pc = 0.0737 sub/GFLOP
ps = 0.00368 sub/(billion parameters * token)
```

The quoted storage coefficient is approximately 1/40 of the per-parameter compute price `2*pc`; the supplied decimal coefficients do not specify additional precision.
Ledger nodes recompute F and T from submitted config and weights-manifest shape
materials and reject inconsistent declared values. The shape inventory is signed
and checked against config and artifact size bounds; this does not prove that it
exhaustively matches authenticated weight-file headers. See [compute derivation](compute-v11.md).

## Effective prices, quotes and settlement

AppVersion 11 supports governor `set_pricing` overrides. Gateways query
`/effective-price/<profile>` and use the effective table from chain. Local
`services/chain/genesis_prices.json` class prices are retired calibration data,
not current live prices. Do not copy their small-device classes into quotes.

Each override input/output term is `ceil(price * tokens / 1000)`; sum the terms.
The optional context override term is
`ceil(context_price * input_tokens * output_tokens / (1000*1024))`.
Current listed prices have zero context term. Legacy artifact-only profiles retain
per-token artifact pricing and the historical context floor; do not apply their
units to the live per-1,000-token tables.

The consumer multiplier is applied as `floor(base_total * multiplier_ppm / 1000000)`.
On this chain it moves within [0.8, 2.0], at most 10% relative change per
1,000-block epoch. The library default epoch is 100 blocks; chain configuration
is authoritative. Only accepted settled demand enters the controller: unmet,
failed and expired requests never raise prices. Coverage uses registrations,
not observed heartbeats. See `services/chain/supply_demand.go`.

Use authenticated `POST /v1/quote` with the intended chat-completion body before
inference. The serving tokenizer signs input count, tokenizer identity, prompt
commitment and output cap. A quote creates no hold. Freeze pins effective prices,
multiplier and maximum fee. Receipt submission and settlement recheck actual
counts and fee. A stale price stamp rejects freeze before locking funds.
Same-version input recount mismatch is an objective fault; unavailable tokenizer
artifacts or a version mismatch cause service failure, not a numerical penalty.

Accepted fees split 80% execution / 15% audit / 5% returned to caller, with
integer remainder handling. Sharded execution allocates its execution share by
verified executed work. Refund the unused hold; never bill the maximum output
cap as if all tokens ran. Sampling and audit-pool distribution affect audit
payments, not the 80/15/5 gross allocation. See [audit compensation](AUDIT_COMPENSATION.md).
