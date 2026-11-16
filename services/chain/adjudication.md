# AppVersion 10 adjudication

Whole-model cases use up to three independent qualified auditors, selected by
existing finalized post-commit entropy draws without replacement. Reputation
provides neither seats nor voting weight. Two valid reject evidences from distinct
auditors must bind the same result commitment and canonical cause. Each also
binds a replay record, first-divergence position and applicable tolerance.
Pipeline/MoE holds retain their previous behavior and do not enter this machine.

A sampled escalation requires a valid first rejection, completed uncorroborating
second review, and another qualified node. The first auditor deposits two units
before the third assignment takes effect. Consumer appeals on unsampled delivery
are allowed through delivery + AppealWindowBlocks (default 39268), deposit two
units and draw two auditors. Divergent reviews may append one third auditor.
Appeal settlements do not add executor audited accepts to the sampling tier.
Third assignments contain only the auditor and selection proof; the third auditor
must commit the canonical third-auditor hash described below before revealing. Earlier
conclusions and divergence positions are not part of its assignment payload.
Public historical chain data cannot be hidden from an observer.

One unit is floor(actual fee * 1500 / 10000), the existing auditBudget basis,
including when a frozen compensation policy is present. Cases cap work at three
units. Upheld deposits are refunded, with actual costs recorded as PenaltyCosts
against the existing liable-party penalty. Both whole-model executor slash sites
(including already-settled consumer appeals) consume this receivable first:
min(PenaltyCosts - PenaltyCostsPaid, actually collected slash) goes to the audit
pool, PenaltyCostsPaid increases by that amount, and the payer receives the
remaining slash. No additional penalty is created. Partial collection leaves the
remaining receivable outstanding. If recordOffense authorizes no slash (gray or
refund-only), nothing is consumed. A zero receivable is balance-identical to the
previous slashOffender path. Forged claimant slashes and pipeline/MoE settlement
are unchanged. Proven intentional
forgery consumes the deposit for costs first and invokes the existing 20x/repeat
stake penalty hook. Ordinary unsuccessful claims pay only added work (all appeal
work, one third audit on sampled escalation), refunding any remainder. No-fault
invalid paths fully refund the deposit and book costs to the audit pool as the
protocol dispute budget. Costs charged to a deposit are capped by locked funds.
No ordinary disagreement automatically selects the Forged outcome.

## Signed recompute bindings

AuditVerdict fields 11–13 are optional int64 first_divergence, string tolerance,
and bytes recompute_ref. The typed Go and Python protobufs are regenerated with
protoc 35.1 and protoc-gen-go 1.36.6; an unmodified-schema spike reproduced both
checked-in files byte for byte. A divergence is absent when no positional failure
was found, including honest accepts and non-positional objective failures.
Reject divergence positions must be nonnegative and, when output length is known,
strictly less than AuditOutputTokens. Structural failures at the end of a missing
or extra prefix can therefore be unfit for ADJ while retaining legacy grading.

Tolerance identifiers are the offense Band for token/numerical classes (nonempty),
and literal `exact` for objective offenses. Without an offense the equality path
uses `token-equality-v1`; the numeric path uses lowercase hex SHA256 of the actual
ToleranceBand's deterministic protobuf serialization (as in grading.py).

The recompute reference is SHA256 of this concatenation, with no implicit framing:

```
"ember/recompute/v1\0" | auditor_id UTF-8 | NUL |
challenge_ref (32 raw bytes) | evidence_ref (32 raw bytes) |
verdict UTF-8 | NUL | first_divergence (signed int64 big-endian; -1 if absent) |
tolerance UTF-8 | NUL | n_out_billed (uint32 big-endian) |
distance count (uint32 big-endian) | each distance (float64 big-endian bits)
```

NaN/Inf distances invalidate the binding. The worker maps nonfinite comparison
results to 1e30 before binding and signing. On valid bindings, ReplayRecord is
lowercase hex recompute_ref, so independent auditors bind distinct replay records
even when they opened the same evidence manifest. ResultCommitment remains the
hold receipt reference and Cause remains the first grade's class:fault:band.
A present malformed reference, mismatched preimage or tolerance class, or invalid
reject position produces invalid ADJ evidence, never the old reference fallback.
The binding verifies signed recompute assertions; calibrated verification and
existing offense-grade checks still govern admissibility and penalties.

The third commitment is lowercase hex SHA256 of the following UTF-8 string:

```
"ember/third/v1\0" + verdict + NUL + auditor + NUL + result_commitment + NUL +
replay_record + NUL + decimal_first_divergence_or_minus_one + NUL +
tolerance + NUL + cause
```

No trailing NUL is added. Go/Python golden tests pin both hashes, including
negative-zero distance bits and a UTF-8 auditor. Round-three worker responses
include pre_envelope (commit, account nonce+1) followed by envelope (audit reveal,
nonce+2). The ledger validates the assigned third auditor and third_commit phase,
broadcasts the commitment first, and aborts reveal submission if it fails.

## Legacy preservation

Old/privacy verdicts without recompute_ref retain their old reference but no
positional/tolerance evidence and cannot enable ADJ escalation or conviction.
When no bound case exists, these verdicts follow the pre-ADJ ladder without adding
an AdjudicationCase, preserving Holds, Accounts and Evidence JSON byte for byte.
For an existing sampled case, round-two conviction also explicitly preserves the
pre-existing corroboration rule: outcome == reject and FirstVerdict == reject,
regardless of bindings; the transaction handler has checked grade agreement.
Bound-evidence counting continues to govern escalation eligibility, third-auditor
resolution and consumer appeals. Consumer appeals have no legacy ladder.
