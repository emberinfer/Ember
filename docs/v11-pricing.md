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
migration remains a separate activation. New chains start at version 11.

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

## Activation transactions

Do not submit before activation. Set `RPC`, `CHAIN_ID`, `GOVERNOR_KEY` and `NONCE`
(the governor account's next nonce). These commands preserve example coverage
targets 4/3; replace those targets with the intended existing values if different.
The old gpu-v1, fp8-v1 and split-v1 cards receive no override.

```sh
for row in \
  'ember-qwen2.5-1.5b-unified-v1 229 234' \
  'ember-qwen3.6-35b-a3b-fp8-unified-v1 458 571' \
  'ember-olmoe-1b-7b-moe-v1 195 217' \
  'ember-kimi-k3.1-pro-v1 16660 25630'
do
  read -r profile input output <<< "$row"
  emberchain tx --rpc "$RPC" --key "$GOVERNOR_KEY" \
    --chain-id "$CHAIN_ID" --nonce "$NONCE" \
    --action "{\"type\":\"set_pricing\",\"model_id\":\"$profile\",\"pricing\":{\"target_executors\":4,\"target_auditors\":3,\"executor_weight\":80,\"auditor_weight\":15,\"consumer_weight\":5},\"token_prices\":{\"price_input_per_1k\":$input,\"price_output_per_1k\":$output,\"price_context_per_1k\":0}}" || break
  NONCE=$((NONCE + 1))
done
```

For 512 input / 256 output tokens at multiplier 1.0, the fixture totals are
178, 382, 156 and 15092 sub respectively (context zero).

## Validation and compatibility notes

Regression coverage includes artifact-only component equality (including OLMoE
at multiplier 980000), per-term ceil arithmetic, all four activation tables,
override/multiplier freeze and settlement agreement, frozen-table immutability,
version gating, byte-identical save/load and AppHash, targeted queries, activation,
and profile-isolated gateway cache fallback with a hard error before first fetch.

Repository-wide validation also exercises the Python suite, standalone SPI
conformance script, the pricing activation test and public security/documentation gates.
The baseline snapshot contains unrelated failures: the recorded hashes of three
experimental JSON fixtures differ from their committed
bytes; historical synthetic redaction fixtures trigger the all-refs secret scan;
and existing FLR calibration/model identity documentation fails the public-docs
lint. These files and signed artifacts are left unchanged by the pricing change.
