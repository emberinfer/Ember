# Audit design and economic limits

Operators, auditors and security reviewers can use this document to understand
what Ember checks, how those checks affect payment, and which attacks remain.
This describes the current source implementation; deployed networks must also pin
their signed Profile and ledger policy. See the [threat model](THREAT_MODEL.md),
[calibration rules](AUDIT_CALIBRATION.md) and [architecture](PRODUCTION_ARCH.md).

## The honesty inequality

Let R be a request's honest gross reward, gR the compute-cost saving from cheating,
p the probability that the request is actually audited, q the conditional
probability that cheating produces a confirmed, enforceable rejection, and L the
collectible stake penalty. A conservative sufficient deterrence condition is:

`p × q × L > g × R`.

This omits additional losses such as forfeited payment, and assumes penalties are
collectible and have economic value to the operator. It is a design test, not a
measured guarantee. q depends on the attack, tolerance, auditor honesty and ability
to obtain confirmation. Stake caps, simultaneous liabilities, correlated auditors
and valueless testnet credit can invalidate a nominal multiplier calculation.

The whole-model consensus path computes each offense from the larger of
`slash_multiplier × actual_charge` (default multiplier 20) and the stake floor, applies the repeat multiplier, and
limits the payment by remaining stake and the block budget described below.
The fee may be split among executor, auditor and consumer, so actual_charge need
not equal executor reward R. Admission limits aggregate reserved exposure to one
fifth of stake. This does not collateralize a 20-fold penalty on every concurrent
hold, or establish a lower bound on q. Recompute the inequality using collectible
stake, actual fee shares and measured attack-specific detection rates.

## Evidence binding and layered openings

The [evidence encoder](../node/evidence/__init__.py) commits each generated token,
raw top-k scores and SHA-256 hashes of selected activation bytes. It groups up to
16 steps into signed chained `ChunkRoot` records; `prev_root` binds the preceding
signed digest. The field named `merkle_root` holds a hash of the canonical JSON
group; this path does not implement selective Merkle inclusion proofs. The final
receipt commits the complete encoded output and its chunk chain.

The [whole-model auditor](../node/auditor/__init__.py) verifies the receipt,
chunk signatures/sequence/counts, each opened group and the full output commitment
before numerical comparison. For signed numerical policies it requires exactly
16 top-k entries and exactly layers 0/14/27 at every output position. It validates
finite, unique raw-logit candidates and checks that the committed greedy token is
a maximum of those scores. Input token counts are also recomputed for Profiles
with input/context pricing. Malformed required openings or hash mismatches are
objective evidence failures; missing service responses are availability failures.

Activations are the selected layers' last-prefix-position hidden vectors encoded
as float32 little-endian bytes. They constrain fabricated execution evidence but
do not prove that every intervening layer ran. Signatures attribute claims; they
do not establish truthful hardware declarations or independent ownership.

## Forced-prefix replay and audit cost

The [Transformers adapter](../node/engine/adapters/transformers.py) tokenizes the
prompt and replays each committed output position using the producer's exact
previous tokens. It never compares two freely generated continuations: a token
fork would otherwise make later differences ambiguous. The adapter computes the
next-token logits and selected hidden vectors on each growing prefix using
`use_cache=False`, then appends the committed token.

This implementation loops over all output positions and repeats prefix forwards.
It is not a single batched teacher-forcing pass or a three-position spot check.
Layer selection is sparse; position coverage is complete. Replay can cost as much
as, or more than, generation. No fixed audit-overhead percentage or prefill speedup
is established by the source. Budget full replay, confirmation, model loading,
evidence transfer and queue delays when estimating operating costs.

## Sampling and auditor assignment

The [draw helper](../services/verifier/verifier.go) can select a percentage of
receipts by hashing a deterministic Ed25519 signature of the receipt digest.
This is a verifiable coordinator draw, not an unpredictable public random beacon.
The [gateway audit loop](../cmd/emberd/main.go) calls it with **100 percent** on
its subprocess path. Its challenge lists first/middle/last positions and a
one-minute deadline; these metadata do not limit the Python worker's full replay.
Whole-model challenge metadata lists layers 0/14, while signed numerical policy
requires all three layers 0/14/27; the worker enforces the latter.

Chain routing freezes a canonical request identity when the payer's first freeze
enters a block: `request_id = H(payer, nonce, commitment_bytes)`. Payer length is
encoded as an eight-byte big-endian prefix, followed by payer bytes, the eight-byte
big-endian envelope nonce, and the exact signed ExecutionCommitment bytes. Older
output-only requests without an ExecutionCommitment use the canonical freeze
Action JSON as commitment bytes. Existing client hold IDs remain lookup aliases;
the frozen Hold `request_id` is the consensus identity. Re-sending the identical
freeze payload is an idempotent success, including after settlement, and never
freezes or pays twice. Altering the payload under an existing hold ID fails closed.
Changing a payload requires a new hold ID, nonce, request identity and freeze.

[Consensus selection](../services/chain/assignment.go) uses only
`H(request_id || H_block(h+1) || ... || H_block(h+M))`, where `h` is freeze
height. Request identity is decoded to its 32 hash bytes before concatenation.
`assignment.m_blocks` defaults to 3 and `assignment.cooldown_requests` to 8;
both are governance parameters pending calibration and frozen with each request.
All future hashes must have arrived in finalized blocks before assignment can
succeed. Receipt digests, output data, audit round and assignment timing are
absent from the seed. A confirmation uses the same seed with the first auditor
excluded. Existing challenge consumers continue to receive the 32-byte seed in
their selection-proof field; this field contains no VRF proof.

[Eligibility](../services/chain/routes.go) uses finalized active stake-backed
registrations, matching model/Profile, replay capability, engine/torch runtime
ID and device; it excludes the producer identity. Pipeline selection excludes
all authorized executors. After 8 consecutive successful assignments of the same
executor/auditor pair (including confirmation rounds), that auditor is skipped
for the executor. The next successful draw of a different auditor resets the
streak to one for the new pair. A pipeline applies this rule separately to each
executor. If rotation leaves no eligible candidate, assignment fails closed;
it does not silently reuse the cooling pair. An assignment record v1 stores the
request identity, freeze height, future block hashes (three by default), sorted
candidate pool, eligible mask, seed and winner. `RecomputeAssignment` permits
offline draw reproduction; verifying pool eligibility additionally requires the
finalized registration and pair-streak state. This route requires an audit result
for settlement rather than offering a reputation-based sampling rate.

The block-hash construction is not a VRF or an unbiased beacon proof. Timing,
validator influence, registration control and common ownership remain relevant.
A different identity is not evidence of a different operator.

## Numerical policy and confirmation

Signed genesis selects the metric, calibration digest and exact runtime-pair
bands; a job's unsigned tolerance cannot replace them. The
[policy loader](../node/auditor/policy.py) verifies genesis, producer Profile,
receipt identity and model bindings. Verdict `audit_policy_ref` binds the raw
genesis SHA-256. Unknown pairs, observation bands and invalid/nonfinite reference
comparisons are inconclusive for numerical enforcement.

| Metric | Implemented comparison | Enforcement boundary |
|---|---|---|
| Legacy ranked | Ranked token IDs must match; relative L2 for scores and layers | Historical policy retains its original asymmetric formula |
| `union-interval-v1` | Censored top-k interval gaps, normalized symmetrically; maximum with layer relative L2 | Signed same-runtime/device enforcement, or explicitly supported pipeline band; other cross-runtime pairs observe only |
| `rank-probability-v1` | Maximum of rank distance / 0.4, conditional top-k TV / 0.5, union distance / 0.15 and layer relative L2 | Exact signed CUDA/class fingerprints; each band has positive tolerance below 1; explicit cross-class bands supported |

The [comparators](../node/auditor/numeric.py) take maxima across every audited
position and required layer. Layer relative L2 uses the reference norm with a
1e-12 floor; union distance uses the larger censored vector norm with the same
floor. Missing logits remain unknown, and conditional top-k TV is not
full-vocabulary TV. Tolerance is per signed pair; there is no universal 0.001 or
0.5 acceptance rule. [GPU execution](GPU_EXECUTION.md) gives the metric details.

Confirmation depends on the execution route:

- The [subprocess worker](../node/auditor/__main__.py) loads a second local engine
  after a numerical dispute. It rejects only when that execution also disputes,
  is finite under numerical policy, and selects the same band. A changed band or
  disagreement yields inconclusive. Objective opening failures can reject on the
  first execution. Two local executions need not be independent machines/operators.
- The [registered worker](../node/auditor/worker.py) performs one replay and signs
  a reject for an enforced numerical mismatch. Consensus requires a first reject
  to be confirmed by a different assigned auditor identity, excluding the producer
  and first auditor. This two-identity rule also applies to objective rejects on
  that path. A second accept resolves to accept; inconclusive or no available
  confirmer cannot cause slashing.

Consensus validates assignment proofs, signatures and references. It consumes
signed verdicts rather than rerunning floating-point inference or proving the
reported distance. Malicious eligible auditors can therefore still fabricate
results, including false accepts.

## Pipeline audit and attribution

The [pipeline auditor](../node/auditor/pipeline.py) reconstructs signed stage
invocations, verifies complete ordered coverage, exact token prefixes and byte-for-byte
hidden-state handoffs, and checks aggregate openings/output commitment. It then
uses a full-model reference, including reference hidden vectors at executed cuts
for dynamic ranges. It does not establish a partial-weight-only auditing protocol.

For GPU pipelines every stage/reference pair needs an enforced signed band and
all selected thresholds must be equal. The subprocess confirms numerical disputes
with a second replay under the same policy/metric/tolerance and the same nonempty
fault set. Layer/boundary distances map to their executed owners; logit distance
maps to the terminal stage. The implementation's fault extraction tests those
unscaled distances against tolerance; it does not attribute rank/TV-only failures
or rescale the fault test to match every GPU metric component. A discrepancy
without attributable faults cannot establish that a particular stage should pay
a penalty. See [sharding](SHARDING.md) for the separate genesis-authorized
pipeline-auditor route and its limits.

## Settlement, slashing and availability

In [consensus settlement](../services/chain/state.go), acceptance after the dispute
height refunds unused reservation funds and distributes the verified actual fee
under the model's fee-share policy. Confirmed rejection refunds the entire
reservation. Signed verdicts must carry an offense class: `objective` for current
signature/hash/format faults, or `numerical` with a finite nonnegative score,
band and length class. Input-token grading is deferred; no tokenizer registry or
N_in offense verdict is available. Two independently assigned references must
agree on class, fault kind and numerical band/length class. Numerical settlement
uses the lower of their scores and the threshold pair frozen at request entry.
Unknown numerical policies and missing grading fail closed.

Consensus stores version-2 offense records with class, score, band, length class,
threshold pair (null for objective faults), status, node and hold. Numerical
thresholds are governance entries per model/band/output-length range, with
`theta_soft < theta_hard`, honest-score fixture data, calibration reference and a
placeholder flag. Band identifiers in the registered worker are SHA-256 of the
deterministically serialized signed `ToleranceBand`. Threshold pairs are frozen
with the request; the current slash mode applies at settlement.

| `misjudge.slash_mode` | Objective | Numerical |
| --- | --- | --- |
| `off` | Refund, no slash | Refund, no slash |
| `refund_only` | Slash | Refund, no slash |
| `gray_gate` (genesis default) | Slash | Below soft: refund; gray: refund and observe; hard: slash, unless queued by prior gray repeats |
| `full` | Slash | Slash (pre-safety-valve numerical treatment) |

In `gray_gate`, scores in `[theta_soft, theta_hard)` produce gray observations.
After `misjudge.gray_repeat_limit=3` observations in the rolling governance window
(default `window_blocks=1000`), subsequent above-soft numerical events enter
`pending-review`, refund the payer, and never auto-slash. Queued observations also
count within that window; an event exactly one window old expires. The cumulative
per-node observation counter and the review state persist on chain. The appeals
flow is designed but NOT yet implemented: there is no appeal transaction, review
panel, or automatic resolution of the queue. Governors may update the safety
policy through signed `set_misjudge` transactions; this does not resolve reviews.

For slash-eligible offenses, the per-offense calculation remains
`min(stake, max(20 * revenue, ceil(stake * slash_floor_bps / 10000)) * ladder)`.
The ladder is 1/2/4/8 within the existing repeat window. All offenses for one node
in one block share a single ladder step. `misjudge.cap_block_bps=500` bounds their
aggregate payment using stake immediately before the first slash in that block.
The integer limit is `max(floor(stake_start * cap_bps / 10000),
ceil(stake_start * slash_floor_bps / 10000))`, preserving the protected floor even
for a one-subunit stake or a governance floor above the cap. Later offenses still
compute their individual floor before applying the remaining aggregate budget;
they may transfer zero after that budget is exhausted. The block's cap and ladder
are fixed by its first slash. Penalties transfer stake to the payer; none is burned.

[Pipeline settlement](../services/chain/pipeline.go) applies the same grading,
mode, ladder and block budget separately to credited nodes in the agreed signed
fault set. Other stages lose no stake. Service failure refunds without slashing.
Expired frozen/disputed holds can be refunded by height; a missing auditor,
timeout or lost connection alone is not fraud evidence. The durable spool retries
unfinished audit jobs, but successful HTTP output does not imply final settlement.

The [SQLite backend](../services/ledger/ledger.go) supplies development accounting
with coordinator authority and a minimum rejection multiplier of 20. It must not
be confused with the consensus path's independently assigned signed verdict rules.
There is no automatic stake-aging reputation, paid appeal/third-reference
tribunal, fourteen-day earnings vesting or automatic auditor-penalty system in
these mechanisms.

## Residual risks and necessary measurements

An executor can reuse correct cached evidence, outsource to a genuine model,
imitate the sparse observations or recompute after receiving an evidence demand.
Tolerance creates room for substitution; tightening it can reject honest runtime
drift. Sparse logits and selected layers leave unobserved behavior unconstrained.
Colluding auditors, Sybil registrations and validator/gateway concentration can
reduce effective q. Neither signatures nor finality establishes fresh execution,
physical machine identity, confidential inference or model quality.

Before asserting economic deterrence, publish attack-specific detection and
false-rejection results on held-out prompts/contexts, exact runtime pairs, full
replay/confirmation cost, timeout/refund rates and collectible exposure. Include
cache, quantized/substitute model, wrong-prefix and fabricated-evidence attacks.
These measurements must support the chosen p, q, L and g in the inequality;
fixture tests alone cannot supply them.

Audit compensation policies, cited-prefix receipts, request audit escrow,
assignment work limits and the operator epoch report are described in
[AUDIT_COMPENSATION](AUDIT_COMPENSATION.md). Policy-free models retain their
historical settlement path. Measured small-fee load does not meet the profitable
production-replay target; admission refuses known underfunded replay work.
