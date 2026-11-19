# Pipeline spot audit v14

Activation follows the signed membership upgrade ladder: a manifest loaded from
version 13 advances once, exactly at its activation height, to 14. The genesis
AppVersion constant stays 11. No deployment or production transaction is needed
or performed by this change.

## Wire and identity

`submit_pipeline` adds the JSON/base64 `commitment_root`, exactly 32 nonzero
bytes. The hold pins it unchanged. All added state fields are omitted when
absent, preserving pre-v14 serialization. Pre-v14 envelope decoding continues to
ignore commitment_root/spool as unknown fields, including arbitrary JSON
values, before signature normalization. Pipeline steps are generated-token rounds
(ruling 7), not layer atoms. N is frozen from the signed submission and must
equal the aggregate receipt's output_tokens. The manifest count field remains
reserved and unused for pipeline N. Gateway evidence wiring remains blocked as
described below; this branch is not ready for activation.

The assigned hold exposes `pipeline.spot`:
`request_id`, `auditor`, ascending unique `step_set`, `challenge_height`, `round`
(`"1"`, `"2"`, or `"esc"`), deadlines, receipt state, and previous challenge
records. Round 2 preserves the exact set. Escalation opens all 1..N, reusing
already-spooled data off chain. Hold assignments retain seed blocks and pool.

Initial assignment and sampling share the existing assignment seed, but for
v14 pipeline requests the entropy anchor is submission (commitment inclusion),
not freeze. The seed is SHA256(request-id bytes || M future finalized hashes).
Step selection independently hashes ASCII `EMBER-SPOT-STEPS-V1` || seed ||
counter-u64be, uses unbiased rejection sampling and partial Fisher-Yates,
then sorts the k results. k is 1/6/15 for existing sampling tiers 0/1/2,
clamped to N. It does not change request sampling probabilities.

Substitutes use the first finalized block after the missed-verdict event.
Selection executes only in the following block, with existing eligibility,
prior-assignment exclusion, and contribution reservation. A depleted pool
leaves the obligation pending until another candidate becomes eligible or the
5,000-block dispute window refunds; it never chooses an arbitrary node.

## Opening evidence

The assigned auditor signs an ordinary envelope with action `spot_received`
(and hold_id), or a valid signed audit verdict implicitly acknowledges receipt.
First-verdict deadline starts when receipt is acknowledged. Without any
acknowledgment, the auditor still has an outer deadline of opening deadline +
60; silence cannot prevent the auditor timeout. A pending, timely accusation
pauses that timer while its 60-block counterproof window runs. For a substitute the
already-received spool remains usable and the deadline is 120 blocks.

`spot_unopened` carries `spool`, a gateway-signed absence attestation. The
transaction envelope supplies the auditor signature. The chain requires an
existing challenge, expired opening deadline, current assigned auditor, signed
absence covering that deadline, and no previous receipt or pending accusation.

`spot_counterproof` is signed by an authorized executor and carries a positive
timely gateway receipt. Both attestations have these ordered JSON fields:
`request_id`, `challenge_height`, `step_set_hash` (base64), `gateway`,
`received` (bool), `height`, `signature` (base64, omitted while signing).
Signature bytes are Ed25519 over ASCII `EMBER-SPOT-SPOOL-V1`, one zero byte,
and canonical Go JSON of those fields with signature omitted. The gateway is
an active on-chain ResourceProfile with role `gateway`; its registered signer
must map to its node identity. `step_set_hash` is SHA256 of ascending step
indices concatenated as u64be. Receipts must be dated no later than the opening
deadline; absence must be dated at or after it. Challenge identity is exact.

Counterproof is available through accusation height + 60 inclusive. After that,
block finalization confirms the accusation, records executor unopened counts,
and resends round 1 with the same set and a fresh 120-block deadline. Receipt
in round 2 records one minor executor event. A second confirmed accusation, or
one in escalation, cancels/refunds using existing service-failure settlement,
records `audit_verdict=reject_unopened`, and records a major executor event.
No fraud slash follows silence. A successful counterproof records a major
against the accusing auditor and zero executor violations for that accusation.

## Verdicts and settlement

Signed AuditVerdict fields 14/15/16 are per_step, verdict_distance, verdict_kind.
Each step contains step, outcome (match/mismatch/binding_fail/format_fail), and
first_divergence_distance. Full review includes every profile step in order.
The chain rejects missing/duplicate/out-of-order steps, invalid outcomes,
nonfinite/negative distances, incorrect maximum distances or inconsistent kind.
The existing challenge signature, selection proof, evidence reference, offense
grade and fault attribution checks apply to legacy verdicts. Spot verdict
validation and corroborated spot settlement bypass those two legacy gates.
Settlement attributes the offense to the actual credited hold executors.
Challenge signatures, selection proofs, and evidence references remain mandatory.
Numerical tolerance and tensor verification remain auditor responsibilities.

A first reject_step preserves its signed verdict and chooses the second auditor
through existing corroboration selection. Full accept records good_faith_auditor,
invalidates the first reject for settlement, and does not count misjudgment.
Full reject must corroborate a bad step in the first verdict from a different
auditor; legacy fault attribution/grade is not required. This positive evidence enters existing fraud settlement and
slash arithmetic (20x fee policy), bypassing legacy numerical refund-only/gray
classification only for the completed v14 spot corroboration path.

Unresolved pipeline commitments refund at the existing dispute deadline during
block finalization. No accept payment can bypass required receipt and verdict.

## Availability

`/availability/<node-id>` returns minor/major events, window counts, level,
pause_until, and separate executor/auditor challenged_total/unopened_total.
An aggregate opening obligation is charged to the sorted unique credited
execution participants; authorized but unexecuted standbys do not acquire
availability events. These are reporting inputs only; no statistical inference runs in consensus.
Window membership is 0 <= current-height - event-height < window length:
50,000 minor, 200,000 major. Levels automatically fall as events expire; pause
remains until its height. Every new event at T2/T3 starts/extends pause by 30,000
blocks; ordinary per-block refresh never extends it. A paused executor cannot
submit a pipeline receipt, including a request frozen before the pause.

Each new **major** event whose post-event level is T3 burns min(1,000, stake).
The event reaching the threshold counts; later major events each count again.
Minor events and refreshes never charge. There is no negative stake, payout,
20x silence penalty, or dependence on map iteration. Hold processing is sorted
by hold ID so concurrent penalties and refunds have deterministic order.

## Merkle interoperability

`services/chain/testdata/spot-merkle-v1.json` is an independent Python hashlib /
struct golden fixture, containing exact tensor bytes, all leaf components,
three leaf hashes and an odd-width root. The Go tests check these constants,
left/right proofs, self-pairing, and tampering. Helpers implement PARAMS section
1 exactly; the chain stores the submitted root without parsing leaf data.

## Data-plane wiring status (G, incomplete)

Pipeline N is the submission-bound generated-token count. Version 14 rejects
`spot_step_count != PipelineExecutionReceipt.output_tokens`; challenges and
escalation use the frozen count. `ModelManifest.spot_step_count` remains reserved
and is not enabled as a pipeline N source.

Two independent, corroborating spot rejects settle against the actual credited
hold executors, without legacy `offense` or `fault_node_ids`. Existing slash
arithmetic, stake floors, block caps and payer refunds remain unchanged.

Ruling 10 supersedes the Ruling 9 concatenation contract. Pipeline leaves now
have library builders in `node/spotcheck.py` and
`services/gateway/spot_records.go`: hash each original protobuf record, combine
request digests in execution order, and combine response digests followed by
the original opening digest. Both implementations enforce opening count equals
receipt output tokens and contiguous zero-based request-step coverage. The
pipeline format discriminator is `pipeline-record-hashes-v1`; ETV1 helpers stay
separate for range/shard. The signed two-stage golden fixture is
`node/auditor/testdata/pipeline-record-v1.json`.

Python `OpeningService.open` dispatches pipeline manifests to record disclosure;
round two reuses the selected set and escalation supplies only uncovered steps.
This is a library API, not an enabled gateway submission/delivery path. Its
caller must supply readers for original durable invocation and opening records.
The existing tensor cleanup and auditor paths do not yet support these pipeline
packages. No activation or complete end-to-end implementation is claimed.

One specification clarification remains: the prescribed next-step disclosure
contains only its first request and request digests, while the verification
requirement asks to recompute both next-leaf input and output hashes. Next input
can be recomputed; next output also requires next response digests and the next
opening digest, which are absent. Verifying a Merkle proof binds the supplied
next output digest but does not recompute it from record hashes. Either the
verification requirement or the disclosure set needs a coordinated clarification.
The verifier has not been weakened to silently choose between those contracts.
