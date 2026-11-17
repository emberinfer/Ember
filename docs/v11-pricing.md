# Governance token pricing

AppVersion 11 adds an optional `token_prices` object to `set_pricing` and each
model record, alongside `pricing` (coverage targets and fixed 80/15/5 weights).
The object is a complete table of integer sub per 1,000 tokens. Presence selects
all three override values, including explicit zero context. Omission in an action
preserves an existing override; profiles without one retain artifact pricing.
Versions below 11 ignore the object entirely. Unset fields are omitted from state
and frozen holds, preserving version 10 serialized bytes and AppHash.

Existing chains activate 10 → 11 through the existing coordinated
`--membership-upgrade` manifest at its exact activation height. The version 9 → 10
migration remains a separate activation. The source base constant initializes new states at 11; existing chains can advance through the coordinated ladder to 12.

Composition: select override ?? artifact. For overrides, independently calculate
`ceil(input_price * input_tokens / 1000)` and
`ceil(output_price * output_tokens / 1000)`. Context is
`ceil(context_price * input_tokens * output_tokens / (1000 * 1024))`, using the
same per-1K units while retaining the interaction. Sum these terms, then apply
`floor(base_total * MultiplierPPM / 1000000)`. Without an override, the historical
artifact formula and context floor are unchanged. Arbitrary precision intermediate
arithmetic rejects final int64 overflow. The existing supply/demand corridor,
epoch settings and step logic are unchanged. Frozen holds copy the table and
multiplier, so later governance changes do not change settlement.

The gateway's consensus ledger queries `/effective-price/<profile>` through ABCI.
This targeted response contains role pricing, optional token table, artifact
prices, multiplier and effective height; it contains no full state or artifacts.
Quotes and freezes use this data, never local genesis prices. A failed query logs
a WARNING and uses the last successful in-memory response for that same profile.
Without a cached response it returns an error. Restart clears this cache. Chain
freeze validation remains authoritative and rejects amounts that no longer match.

## Current effective tables

The live tables are already effective, not pending activation instructions:

| Profile | Input sub/1,000 tokens | Output sub/1,000 tokens |
| --- | ---: | ---: |
| `ember-qwen2.5-1.5b-unified-v1` | 229 | 234 |
| `ember-qwen3.6-35b-a3b-fp8-unified-v1` | 458 | 571 |
| `ember-olmoe-1b-7b-moe-v1` | 195 | 217 |
| `ember-kimi-k3.1-pro-v1` | 16660 | 25630 |

Kimi is published but has no serving nodes. Current context override is zero.
For 512 input / 256 output at multiplier 1.0, totals are 178, 382, 156 and
15092 sub respectively. See [pricing](PRICING.md) for intrinsic derivation,
rounding and the supply/demand multiplier. Query effective prices instead of
replaying a historical governor action with guessed coverage targets.

The live chain is `ember-testnet-1`, with genesis dated 2026-11-14 and five Ledger nodes. Live AppVersion 12 activated at height 116582. The source base constant is 11; the coordinated upgrade ladder advances the running state to 12.
