# v11 PERF A: chain core

Scope: subitems 1–4 only. No broadcast confirmation, audit backoff or targeted
query optimization. All changed execution/persistence/pruning semantics require
`State.Version >= 11`. Version 4–10 loading remains supported. AppVersion is 11.
No new exported consensus fields were introduced.

## Transaction isolation and canonical bytes

`transaction_copy.go` isolates mutable exported fields using Go copies and shares
BlockHashes, which Apply never writes. The block boundary copies that map before
insertion/pruning. ABCI proposal/finalization use Apply's atomic publication and
need no outer per-transaction snapshot in v11. CheckTx uses a private block copy.
Legacy versions retain their original JSON Clone path and serialization shape.

The Clone formerly at state.go:544 was the inner hot-path Apply snapshot; it now
uses transactionCopy. The Clone formerly at adjudication.go:253 was also on the
hot path (appeal and escalation planning); it uses transactionCopy as well. Its
small per-hold JSON copy remains necessary to detach the caller's hold pointer.
`State.Clone` remains available as the legacy/differential oracle and public copy.
Protobuf copying drops unknown wire fields recursively, matching the original
encoding/json projection instead of copying protobuf internal synchronization.

FinalizeBlock computes SHA256 of one canonical State.Bytes result and retains
those bytes in Application. Commit uses exactly that buffer. There is no change
to the full-state JSON hash definition. Archive JSON encodes historical deltas,
not another full State. SaveState remains tmp+rename; v11 additionally fsyncs.

## Block hash readers and retention

Repository-wide production Go readers:

* `services/chain/validators.go`, Membership.validate/readyValid: ReadinessBlocks
  is constrained to 1..1000, so L_readiness=1000.
* `services/chain/assignment.go`, assignmentSeed: 1..64 hashes immediately after
  FreezeHeight. Those seed blocks can be arbitrarily old: an unresolved request
  or a subsequent consumer appeal can trigger another draw. Neither HoldTimeout
  nor the *seed span* is a global maximum historical age. Adjudication's default
  appeal window is 39268, but configurable and relative to delivery, not freeze.

There is no finite global L in the existing rules. A finite-global-L assumption
would be incorrect. W=2000 covers twice the bounded readiness lookback; heights
less than Height-W are removed from consensus and retained in the archive.
assignmentSeed and readyValid both use the archive-aware blockHash accessor.
No evidence verifier independently reads BlockHashes; record verification uses
AssignmentRecord.SeedBlocks (RecomputeAssignment). Older versions never prune.

## Hold closure and readers

Closed labels are `settled`, `cancelled`, `slashed`:

* `state.go`, applyActionInternal: submit requires frozen; audit/assignment/settle
  require dispute; cancel requires frozen; expire requires frozen or dispute.
  settleAt produces closed states, never consumes one as a new active state.
* `pipeline.go`, submitPipeline and pipeline settlement, and `moe.go`, submitMoe
  and MoE settlement, similarly require active holds for new transitions.
* **Exception:** `adjudication.go`, registerAppeal can reopen a closed whole-model
  hold as dispute; commitThirdAdjudication can write an outstanding commitment
  without checking the closed label. Consequently none of the three labels
  alone is a sufficient terminal predicate.

terminalHold only archives closed records with no pending deposit or third
commitment and no remaining consumer-appeal window. It conservatively considers
appeal eligibility even if the current parameter disables adjudication. Every
nonterminal record stays in Holds. The first v11 FinalizeBlock sweeps existing
terminal records; subsequent blocks sweep newly closed/eligible ones. It does
not silently close existing appeal rights.

Historical reads:

* LookupHold and `/hold/<id>` check live then archive and return the complete hold.
* Apply's idempotency index and first-data checks use LookupHold. Actions that
  reference archived records restore a private full copy for the original
  validation/error/idempotency semantics; next finalization rearchives terminal
  copies. Conflicting payloads still fail without mutation.
* settledWorkCount (probation), distributeAuditPool (all historical epochs), and
  checkExitWindows (future expiry/settlement liabilities) include archived holds.
* exposure, contribution InFlight and ledger RecoverFrozen only count active
  holds, all of which remain in consensus. Migration folds in audit_sampling.go
  and liveness.go execute before v11 and therefore before any archival.
* Privacy holds remain active until ordinary finalization. Historical privacy
  events operate on PrivacyState; referenced hold validation gets a private copy.
* Ledger AuditRoute preserves the full `/hold` result when v11 `/state` omits a
  closed record, while refreshing active assignments from `/state` as before.
* The old ops/f5-gpu-stage.py drill enumerates settled records directly from a
  saved raw state; raw v11 consensus dumps intentionally omit archived holds.
  Historical operators should use `/hold/<id>` or LoadState + LookupHold.

## Archive durability, snapshots and recovery

Archive location: `<state-file>.archive/<canonical-state-sha256>.json`.
Each immutable delta contains its parent key and complete removed records. The
archive never appears in State JSON/AppHash. Commit fsyncs the archive delta and
directory first, then writes/fsyncs/renames state and fsyncs the state directory.
A restart selects only the delta chain named by the committed state's hash, so
an orphan written before a crash cannot introduce a hold from an uncommitted
block. Failed Commit leaves the staged state available for retry.

LoadState fails on missing referenced archives or cycles; it never silently
continues with missing history. SaveState to another directory exports a complete
archive root, so the exported snapshot is independently loadable. Copying only
a v11 JSON file is insufficient: copy its adjacent archive directory as well, or
use SaveState for a self-contained export. Archive storage and in-memory history
continue to grow; initial archive writing/reloading is outside the hot consensus
serialization budget. Current archive chain loading is linear in retained deltas.
Orphan files are harmless and are not automatically garbage-collected here.

## Validation and measurements

Tests cover seeded random transfer/stake/error sequences and constructed hold
transitions under versions 10 and 11 against the original JSON Clone oracle;
post-debit overflow rollback; CheckTx committed-state isolation; protobuf JSON
projection; staged/hash/persisted-byte identity; v10 production snapshot exact
roundtrip; fixed v10 block AppHashes; full-record archival and cold idempotent
rerouting; changed-payload rejection; retained appeal/deposit/commitment rights;
historical reputation/exit/audit-pool reads; multi-commit cold archive traversal;
missing archives; and crashes/failures before consensus state rename.

Machine-local artifacts (data disk):

* `/root/data/perf-baseline.txt`
* `/root/data/apphash-v10-oracle.txt`
* `/root/data/perf-numbers.txt`
* `/root/data/perf-all-tests.txt`
* `/root/data/perf-build.txt`

Run with PATH containing /usr/local/go/bin and GOCACHE/GOPATH/GOTMPDIR set to
`/root/data/{gocache,gopath,gotmp}`. PERF_SNAP unset skips benchmarks.
PERF_V11=1 enables v11 on the snapshot; PERF_WARM=1 measures steady state after
one activation block. PERF_ORACLE selects the frozen v10 hash list (only the
baseline run uses PERF_RECORD=1). PERF_LIVE selects the copied production fixture.
No production tree was edited, and no production process was restarted.

Measured snap-v9 wall times (baseline 1x / post-activation steady 3x): empty
Finalize 1795.5 → 50.3ms; five-transfer Finalize 10934.4 → 187.0ms; Hash
895.2 → 8.8ms; normal SaveState 596.4 → 15.7ms; v11 Commit 16.0ms.
First v11 Finalize on the unpruned snapshot: empty 289.6ms / five 1546.3ms.
Shared-machine load and different sample counts limit speedup interpretations.
Live activation: 29,208,218 → 7,448,054 canonical bytes; holds 412 → 112;
block hashes 94,148 → 2,001. All Go packages tested and built successfully.
