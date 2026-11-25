# Version-11 persistence and transaction core

The source base AppVersion constant is 11. The optimizations described here are
gated at state version 11 or later. See the [upgrade procedure](UPGRADE.md) to
check the live version.

## Transaction isolation

`transaction_copy.go` isolates mutable exported state while retaining the legacy
JSON Clone path for older versions. Block-boundary copies detach block-hash data.
Finalization hashes canonical state bytes; commit persists the selected buffer.
Protobuf copy handling preserves the legacy JSON projection rather than copying
protobuf synchronization internals. Source: `services/chain/transaction_copy.go:73`.

## History and durability

Block-hash retention uses a 2,000-block live window. Older hashes remain available
through archive-aware reads; late assignment and appeals can need historical
entropy. Closed holds remain live while appeal, deposit or third-commitment rights
are pending. Terminal history moves to the archive without rewriting signed bytes.
`/hold/<id>` looks in live and archived data. A raw `/state` dump is therefore not
a complete historical hold listing.

Archive files live under `<state-file>.archive/`, keyed by canonical state hash.
Commit durably writes archive data before replacing state. Missing or cyclic
archive chains fail load. Copy state plus archives, or use a complete exported
snapshot. History storage still grows; this is not automatic garbage collection.
See `services/chain/history_archive.go:17` and `services/chain/holds_archive.go`.

## Verification scope

The repository includes differential and archive-recovery tests. No machine-local
speedup, byte-count reduction or live activation measurement is asserted here.
Benchmark results depend on the supplied snapshot, workload and machine; record
those inputs before comparing measurements.
