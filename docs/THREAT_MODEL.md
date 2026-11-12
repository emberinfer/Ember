# Ember threat model

Operators and security reviewers can use this document to assess authority,
attack surfaces and remaining guarantees. See [architecture](PRODUCTION_ARCH.md),
[engine evidence](ENGINE_SPI.md), [ledger ADR](LEDGER_DECISION.md) and the
[audit design](AUDIT_DESIGN.md) and [changelog](../CHANGELOG.md). Deployment mode matters: SQLite development mode
and CometBFT settlement have different trust boundaries.

## Assets, adversaries and assumptions

Protect consumer balances, node stake, model/Profile integrity, settlement
reproducibility, service availability and prompt confidentiality to the extent
explicitly offered. Keys may be stolen, nodes may lie about hardware, identities
are cheap, and auditors/gateways can share an operator without declaring it.
Adversaries include malicious nodes/stages (compute substitution), consumers
(replay/overspend/resource exhaustion), validators/coordinators (censorship,
assignment manipulation, fraudulent settlement), and network attackers
(eavesdropping, partition, eclipse, flood). Model publishers and dependencies may
also be malicious. No permissionless operator is inherently trusted.

In chain mode, CometBFT orders deterministic account/hold transitions under a
less-than-one-third Byzantine voting-power assumption. Genesis validator membership
is static; a multi-validator deployment does not establish independent ownership.
SQLite mode trusts one coordinator for accounting and audit signing. Gateways in
either mode retain custodial consumer-account mappings. Auditor identities and
runtime declarations are not hardware or ownership attestation.

## Attack inventory

| Attack / failure | Implemented protection | Residual risk / further work |
|---|---|---|
| Sybil identities and colluding auditors | Stake-backed registrations, producer excluded from registered audit assignment, different assigned reject confirmer | Borrowed stake, valueless faucet credit and hidden common operators weaken economic deterrence |
| Fabricated execution or substitute model | Signed chunks/openings, forced-prefix replay, top-k and required-layer comparison | Correct caches, model imitation and recomputation after evidence demand can evade screening |
| Evidence downgrade | Profile/genesis bindings, typed capabilities and fail-closed full-evidence admission | Self-reported engine/device facts are not attestation; weaker Profiles require separate review |
| Replay or cross-attempt mixing | Signed work nonces, fenced request/attempt IDs, receipt binding, ledger transaction nonces | Key compromise and incorrectly configured gateways remain risks |
| Malicious gateway or seed | Nodes verify signed work and pinned model artifacts; chain validates reservations and assigned verdicts | Gateway custody, censorship, prompt observation and service denial remain possible |
| Malicious validator or quorum | CometBFT consensus and deterministic integer state transitions | Quorum capture, long-range/key compromise and centralized genesis membership |
| Overspend or cancellation after output | Bounded holds, integer fee checks and deterministic refund/settlement | Failed streams may already have exposed output; no atomic delivery/payment guarantee |
| Resource exhaustion | Body/token/concurrency bounds, deadlines, expiring holds, rate limits and bounded shard tensors | Distributed floods, tokenizer/inference cost asymmetry and relay bandwidth bottlenecks |
| Network MITM or replay | Pinned TLS, signed scheduler/stage messages and persistent nonce fencing | Endpoint/timing leakage, censorship, compromised keys and availability loss |
| Prompt theft | Transport TLS and restricted spool permissions | Gateways, executors and auditors see needed plaintext; no confidential inference guarantee |
| Malicious artifacts or dependencies | Signed artifact hashes, path checks, safetensors, disabled remote model code and release checksums | A valid hash may pin a backdoor; signing-key compromise and parser vulnerabilities |
| Stage substitution or stolen stage payment | Signed topology, stage identities, activation continuity, per-stage work credits and fault attribution | Full-model reference authority is separate from registered whole-model routing; shared ownership remains possible |
| Timeout used as fraud evidence | Explicit inconclusive handling and height expiry/refunds | Availability failures still cost time and may prevent settlement |
| Floating-point drift falsely rejected | Immutable metric and runtime-pair bands, second local replay for numerical rejection, observation-only unsupported comparisons | Sparse logits, limited calibration and correlated references; GPU cross-class enforcement requires an explicit signed band |

## Security invariants and verification

1. Evidence requirements come from the signed Profile, never a weaker advertisement.
   Unknown versions, missing fields and invalid shapes fail closed. See
   [adapter tests](../node/test_engine_spi.py).
2. Verify signatures, chunk sequence and opened-byte commitments before numerical
   comparison. New tolerances require a new signed Profile. Unknown/observation
   pairs cannot authorize numerical slashing. See
   [numeric tests](../node/test_audit_numeric.py) and [GPU tests](../node/test_gpu_audit.py).
3. Timeouts and reference disagreement alone do not prove fraud. An inconclusive
   hold needs a valid resolving result or expiry; no inference correctness claim
   follows from consensus finality.
4. Balance, stake and hold transitions conserve value except explicit issuance/burn.
   Verify duplicate/nonces, refunds, finality and ordered replay with
   [chain tests](../services/chain/state_test.go).
5. Security testing should include Sybil/common-operator assignments, collusion,
   stale work, cross-attempt frames, evidence downgrade, resource saturation,
   partitions and poisoned discovery endpoints. Keep configurations and results
   reproducible without exposing secrets.
6. Verify release signatures on a clean machine, exercise key rotation and backup
   restore, and inventory prompt retention. Contract tests alone are not an
   independent security audit or public-network acceptance.

Numerical audits are screening, not execution proofs. Identity diversity does not
prove ownership diversity; governance and issuance can centralize control. Neither
BFT finality nor HTTPS removes these risks.

Scarcity pricing uses prior-finalized stake-backed registration coverage, never
gateway liveness observations. TTL locks prevent active role removal, shortened
expiry and early unbonding; multipliers cap at 2x and shares conserve the existing
fee. Multiple stake-backed identities can still belong to one operator, and
coordinated expiry or service withholding may improve an operator's economics.
This is bounded fee redistribution, not a proof against all economic collusion.
