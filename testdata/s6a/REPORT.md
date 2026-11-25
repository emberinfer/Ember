# S6a snapshot archive policy

Starting master HEAD after pull: `1dbe8cfb013f4229d9b6a9535f797c3ee682c4a2`.

Scope: isolated tests only; no production access. Snapshot format, limits,
consensus serialization and VERSION remain unchanged.

## Implementation and acceptance

- Default inclusion remains true. `include_archive=false` skips archive pinning
  entirely; package entries are only `state` and `state.chunk-spool/*`.
- Format remains 1; 256 MiB / 256 chunk limits, VERSION, consensus and state
  serialization are unchanged. Optional `archive_included` metadata is logged by
  Offer and Apply; omitted legacy metadata defaults to true.
- Inspection found `loadArchive` required the state-hash archive root. A local,
  exact-root `.archive-omitted` sidecar permits intentionally omitted history
  across restart. It never enters the transport tar or state bytes. Missing
  unmarked roots, unrelated markers, and missing parents still fail (tested).
- Empty archive/spool directories replace stale target dependencies before the
  atomic state-file installation. Required protocol spool validation and AppHash
  recomputation remain intact.
- `TestSnapshotArchivePolicy` first commits real archival hashes and a terminal
  hold, then retains 300 valid immutable orphan archive deltas. It packages the
  same state with both policies: true = **1,556,480 bytes / 302 archive entries**;
  false = **7,680 bytes / zero archive entries** (about 203 times smaller).
- Both restored homes restart and match every baseline AppHash at H=5002..5022
  (21 following blocks). Newly archived H=3002 remains readable after another
  restart. Required spool bytes survive both policies.
- Actual omitted-history behavior: internal block-hash lookup returns `nil`;
  `/hold/old-audit` returns ABCI code 0 and JSON `null` (explicit no entry).
  No invented hash/hold and no crash. Current/hot records remain available.
- `TestLocalNetworkStateSync` runs both policies over real local CometBFT P2P,
  with distinct RPC witnesses and real archive files before transfer. Both
  restore into fresh homes and follow at least 22 blocks, comparing each height
  against donor headers and a snapshot-disabled replay baseline. See network.log.

## Validation commands

Go is explicitly prefixed on PATH. Cross-language tests use the existing
Python dependency environment, without any production access:

```sh
PATH=/usr/local/go/bin:/root/autodl-tmp/fix3/s4-test-venv/bin:$PATH \
EMBER_AUDIT_TEST_PYTHON=/root/autodl-tmp/fix3/s4-test-venv/bin/python go test ./...
PATH=/usr/local/go/bin:/root/autodl-tmp/fix3/s4-test-venv/bin:$PATH \
EMBER_AUDIT_TEST_PYTHON=/root/autodl-tmp/fix3/s4-test-venv/bin/python go test -race ./...
PYTHONPATH=release:node:core-py:proto \
/root/autodl-tmp/fix3/s4-test-venv/bin/python -m pytest -q
```

Initial unconfigured runs exposed required Python interpreter/import-path setup;
final logs record reruns with those environment requirements set.

Final standard Go suite: PASS (all packages; go-full.log).
Final Python suite: 586 passed, 3 skipped, 4 dependency deprecation warnings,
68 subtests passed in 135.35 seconds (python-full.log).
The remote branch inventory contains only `master`.
Final full Go race suite: PASS, no race reports (go-race.log); chain package
completed in 218.487 seconds. `git diff --check` also passed.

Implementation commit: `52e4f2f`; guide commit: `e8bd38f`. Each step was
committed on master and rebased against origin/master before pushing.
