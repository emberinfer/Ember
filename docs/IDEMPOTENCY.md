Request idempotency
===================
Freeze actions (freeze, freeze_pipeline, freeze_moe) accept optional
idempotency_key: lowercase hex SHA256(payer_account_bytes || client_request_nonce).
The payer account bytes are the hex-decoded account ID. A uint64 nonce is eight
big-endian bytes; opaque client nonce bytes are concatenated verbatim, without a
separator. This can be recomputed offline. Keep the key across broadcast retries.
A thinking-off fallback resend after truncation is a NEW logical request and
must use a new nonce/key. Different keys create independent requests (with distinct
hold IDs). An absent key preserves legacy hold-ID behavior.

The persisted idempotency_index maps keys to the original hold. A key is bound
to that hold's payer: only that payer may replay it, and Action.Payer must still
match the original payload. The hold stores the key and exact original freeze
payload. For an existing key, Apply compares economic projections of the stored
payload and incoming Action. Equal economic terms return the hash of the original
FreezeTransaction, even after a different executor is selected or the commitment
is re-signed. Replays create no hold, debit, or account-nonce consumption; the
return occurs before nonce validation. Changed economic terms or another signer
are rejected with `idempotency key reused with different payload`.

A first use of a key creates and registers the hold normally. An absent key
retains the legacy HoldID first-data-finality behavior, including its exact
payload comparison. Resolve the original hold using the index in /state and bind
subsequent operations to that original hold ID. Rerouting a retry returns the
original result; it does not change the original hold's executor assignment.

The comparison copies the entire Action and zeros only the transport/routing
fields below, replaces authorizations with coverage projections, canonicalizes
the commitment, then compares canonical JSON. **Every unlisted Action field is
strict/economic by default**, including future fields.

| Class | Fields | Comparison |
| --- | --- | --- |
| Transport | Action.HoldID, Action.IdempotencyKey | Ignored |
| Transport | Envelope nonce | Outside Action; ignored for keyed return |
| Routing | Action.Node | Ignored |
| Routing | PipelineAuthorization.NodeID, Profile, Reserved | Ignored; Profile/Reserved are derived from registration at freeze time |
| Routing | MoeAuthorization.NodeID, Profile, Reserved | Ignored; Profile/Reserved are derived from registration at freeze time |
| Routing/transport | PricingCommitment.RequestId, Signer, Signature | Ignored; RequestId is the transport HoldID and the executor may re-sign |
| Economic | Action.Type, Payer, Amount, Tier, ModelID, GenesisHash, ProfileHash, Profile | Exact |
| Economic | Action.ExpectedPrice (both fields), TotalLayers, MoeInputTokens, MoeMaxTokens | Exact |
| Economic | PipelineAuthorizations coverage | Ordered (StartLayer, EndLayer) pairs; stage boundaries define coverage and credit splits |
| Economic | MoeAuthorizations coverage and Kind | Ordered attention (StartLayer, EndLayer) spans; sorted set of expert (Layer, Expert) units across all expert authorizations, independent of order/grouping or node ownership. Unknown kinds and non-coverage fields remain strict. |
| Economic | PricingCommitment.SamplingParamsVersion, ThinkingEnabled, ThinkingBudget, AnswerBudget, ChainId, InputCommitment, ProfileVer, SamplingParamsHash, Seed, ClientNonce, InputTokens, MaxOutputTokens, TokenizerVersion | Exact, via deterministic protobuf marshal after removing only routing/transport fields |
| Economic | All other Action fields | Exact; no implicit routing exemptions |

Seed and the commitment ClientNonce are deliberately strict/economic despite
possible ambiguity: retries must preserve both. Unknown commitment fields are
also retained under the strict-default rule. If PricingCommitment cannot decode
as ExecutionCommitment, its bytes are compared exactly. If the stored
FreezePayload cannot decode as Action, replay is rejected. The stored
freezePayload() and RequestID derivation are unchanged.

TODO gateway wiring: the current gateway/ledger Backend freeze API carries no
client-supplied logical nonce, and its retry paths allocate attempt-specific hold
IDs and signed tokenization commitments. Correct end-to-end HTTP retry wiring
requires preserving the economic commitment terms and extending the API
with a client nonce; deriving a key from the transaction nonce would be incorrect.
Chain-side support is available now; legacy gateway calls remain compatible.
