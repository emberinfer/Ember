# Ember Engine SPI v1 Conformance

An adapter exposes `create_adapter(endpoint)` and returns an object implementing
`capabilities()`, `generate(prompt, seed, count)`, `replay(prompt, seed, tokens)`
and `close()`. It may import only `ember_spi` from this kit plus its own runtime
dependencies. Steps contain a nonnegative token ID, exact text fragment, unique
finite top-k scores including the chosen token, activation bytes and score kind.

Capabilities must declare the engine/runtime honestly. Claiming `full` evidence
requires real raw logits, required layer openings and forced-prefix replay in an
actual model adapter. The included toy adapter is structural conformance only and
must never register on a production model profile.
