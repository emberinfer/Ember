# Audit design and limits

## Evidence and execution

The execution path signs commitments and receipts binding request, profile,
manifest and output. Full-evidence policies commit top-k logits and selected
hidden-layer openings; equality profiles use their declared token evidence.
Verify signatures and openings before comparing results. Capabilities and
runtime declarations are statements, not hardware attestation.

## Replay and equality

Teacher forcing is the default registered-worker path for `token-equality-v1`.
The worker scores the committed prefix; `UnsupportedCapability` falls back to
regeneration. An explicit `EMBER_AUDIT_TEACHER_FORCING=0` selects regeneration.
Transformers and vLLM have forced-scoring implementations. vLLM uses server
`prompt_logprobs`; a missing response capability can trigger fallback.

The comparator checks token IDs through the first divergence, with epsilon 1.0.
A margin greater than epsilon rejects. A known alternative within epsilon can
accept even though its token ID differs; an absent alternative within the
margin is inconclusive. Invalid/incomplete evidence cannot be called exact
agreement. Do not describe this as unconditional byte-identical output enforcement.
See `node/auditor/equality.py:14` and [teacher forcing](TEACHER-FORCING-AUDITS.md).

Legacy signed numerical policies retain their own metrics, runtime-pair bands and
opening requirements. Unknown or observation-only pairs cannot authorize numerical
penalties. See [calibration](AUDIT_CALIBRATION.md) and [GPU rules](GPU_EXECUTION.md).

## Sampling and assignment

A hold freezes canonical request identity and assignment policy. Receipt submission
must commit evidence before audit resolution. The assignment transaction must be
later than submission. The entropy seed is SHA-256 of request identity and M
finalized block hashes immediately after **freeze height** (default M=3).
Those blocks are not necessarily after the evidence commit. The gateway cannot
choose the auditor. Draw records support offline recomputation; candidate
eligibility also requires finalized registration and cooldown state.

On this chain the base sampling tiers are 30% at zero audited accepts, 5% at 20
and 2% at 200. Only audited accepts with Actual at least 16 sub advance progress.
Amount weighting, penalty-box audits, per-epoch pool and cooldown rules apply;
see [sampling](AUDIT_SAMPLING.md). Unsampled settlement is explicitly unaudited.
Speed scores never enter consensus sampling or trust reputation.

## Corroboration and adjudication

Conviction requires two valid rejection evidences, not a gateway accusation.
AppVersion 10 whole-model adjudication can add at most one auditor after the
second. Thus an initially uncorroborated rejection is not necessarily final:
a qualified third review may supply corroboration. Consumer appeals and the
third review use deposits and signed recompute bindings. Pipeline/MoE holds
retain their own audit paths. See [adjudication](../services/chain/adjudication.md).

## Settlement, slashing and availability

Freeze a bounded hold, execute, commit signed evidence, resolve sampled audit, then settle using actual token counts. Accepted fees allocate 80% to execution, 15% to audit and 5% back to the caller. Unused reservation is refunded. The live hold timeout is 5,000 blocks; expiry refunds unresolved holds through `expire_hold` and gateway recovery, rather than a wall-clock timer.

Expiry is a chain transaction at/after the frozen expiry height; gateway recovery
submits it. A stopped chain cannot advance the timeout. Successful HTTP output
is not proof of final settlement.

The source supports objective, numerical and token offense paths and configurable
safety modes. Confirmed penalties use a 20x revenue basis, stake floor, repeat
ladder and per-block budget. Adjudication costs can consume collected penalties;
MoE has a separate honest-work payment waterfall. See [slashing](SLASHING.md).
Missing reference service, unsupported runtime, timeout or transport failure alone
is not fraud evidence.

### Consumer gray governance

The `misjudge.consumer_gray` policy can freeze normal charging for subsequent
gray/pending-review outcomes based on already-settled consumer history. It does
not turn a gray result into an accepted audit or permit automatic numerical
slashing. The source defaults use a 1,000-block window, global minimum 20 samples
at 50% gray, or pair minimum 10 samples, 5 gray, at 50%. Current chain policy must
be queried; these are library defaults. See `services/chain/consumer_gray.go:15`.

## Security limits

Signed evidence attributes a statement; it does not prove fresh computation,
physical device identity, independent ownership or complete correctness.
Sparse observations, cached evidence, colluding auditors and key compromise
remain risks. Economic deterrence requires actual detection probability and
collectible stake; a nominal multiplier cannot prove it. Testnet credit has no
monetary value. Compensation is bounded by protocol allocations and does not
guarantee coverage of real replay costs. See [compensation](AUDIT_COMPENSATION.md)
and [threat model](THREAT_MODEL.md).
