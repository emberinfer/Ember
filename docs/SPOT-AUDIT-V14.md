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
values, before signature normalization. `ModelManifest.spot_step_count` (20)
is the signed number of replay steps. If absent, v14 requires a nonzero
`submit_pipeline.spot_step_count`, frozen into the hold with the commitment.
This is a signed submission assertion, not proof of the executed operator count:
full opening must disclose exactly N leaves and out-of-range sampled steps are
binding failures. Never substitute output-token counts or mutable registrations.
The production unified scheme has 30 atoms (28 layers plus embedding/head),
not a declared 62-step layout. Gateway capture/routing and fault attribution
remain integration blockers; this branch is not ready for activation.

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
validation bypasses those two legacy gates; settlement attribution for
grade-free spot rejections is still unresolved in this integration branch.
Challenge signatures, selection proofs, and evidence references remain mandatory.
Numerical tolerance and tensor verification remain auditor responsibilities.

A first reject_step preserves its signed verdict and chooses the second auditor
through existing corroboration selection. Full accept records good_faith_auditor,
invalidates the first reject for settlement, and does not count misjudgment.
Full reject must corroborate a bad step in the first verdict and matching fault
attribution/grade. This positive evidence enters existing fraud settlement and
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
