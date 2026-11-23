# Gateway chain bearer bindings

`emberd gateway --chain-auth-file FILE` reads a nonempty JSON array from a
mode-0600 file. Tokens must contain at least 32 bytes and must be unique.
Each entry must have exactly one nonempty `key` or `address` field:

```json
[
  {"token": "REPLACE_WITH_A_LONG_RANDOM_PAYER_TOKEN", "key": "/secrets/payer.key"},
  {"token": "REPLACE_WITH_A_LONG_RANDOM_WORKER_TOKEN", "address": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
]
```

`key` retains its custody semantics: read the local Ed25519 seed/private key
from a mode-0600 file, derive its account address, and enable signing.
`address` accepts exactly 64 hexadecimal characters (normalized to lowercase).
It binds an authorization identity and loads no private key. Never copy a
worker private key to a gateway to configure this binding.

Address credentials can upload image spool data only for holds whose executor
matches their address. Image absence/status/receipt/content reads require the
payer or executor address. Spool/absence attestations are signed with the
separate gateway image key. The worker signs its own `submit_image` envelope;
the gateway verifies and forwards the envelope without rewriting it.

Address credentials cannot request image generations or chat completions,
even if a different custody credential exists for the same address. Read-only
quotes remain available. Missing custody keys also fail before signing in the
ledger and image transaction backend. Image `image_received` uses the payer's
custody key; `assign_audit` and `settle` use the coordinator key. Address binding
is intended for workers, not a replacement for payer custody.

This mapping alone does not register, fund, or stake an account, and it does
not change chain protocol validation.
