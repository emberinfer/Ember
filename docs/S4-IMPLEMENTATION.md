# S4 implementation and acceptance record

Starting HEADs after pull --rebase on 2026-11-25:

- Public master: `5853c30056ed45736990871371a9a8b622892745`.
- ember-web main: `ac80c8dc24fce5651b2dbfdd60314ca8432f4d8c`.

No production host, service, funds or registration was changed. Public RPC reads
are evidence from an operator endpoint, not independent light-client proofs.
All publication transactions below use the isolated `s4-isolated` chain at
127.0.0.1:28757, newly generated disposable keys and genesis-only test balances.
VERSION, chain rules and state serialization are unchanged.

## 1. Publication

The old builder minimum of 28 came from the descriptive [0,14,27] sample list
and the adapter's matching default hooks, not consensus. The builder now accepts
positive real layer counts; adapter defaults remain [0,14,27] on existing
models and select valid indices on smaller models. Policy validation permits
explicit ordered unique numeric indices; publication binds them to actual config
and conformance evidence. Existing signed profiles are untouched.

The command reuses build.py, build_audit_profile.py, prepare, verify and the
committed tx client. The new read-only derive-compute CLI calls core.DeriveCompute
used by existing consensus; safetensors headers supply actual shapes. No fake
layer counts or model-specific exceptions are used. Missing policy is an argparse
error, with no build or submission. Failed builds leave no prepared output;
uncertain tx outcomes retain a marker and refuse automatic rebroadcast.

A real 28-layer Qwen2.5-1.5B-Instruct from the existing model directory, pinned
revision `989aa7980e4cf806f80c7fef2b1adb7bc71aa306`, passed dry run, conformance,
submission and exact-byte/hash readback at isolated height 457:
`C0C3276C456B4B0DD0D0FF9F3996F7C4E69EE51FC6421A864B2FBCD0FF41BF5B`.
The repeated command passed readback only; account remained balance 9900,
nonce 1 after a single 100-unit bond. Measured same-host CPU policies are
observe-only acceptance fixtures, not production calibration claims.

Publishing guides in both repositories use the new command, full pinned origin
and explicit measured policy. Public supervisor supply discovery is automatic;
runtime support and qualified live supply still determine readiness.

The real 24-layer Qwen2.5-0.5B-Instruct was downloaded directly through hf-mirror
at revision `7ae557604adf67be50417f59c2c2f167def9a775`. Its safetensors SHA256
matches the pinned repository LFS object:
`fdf756fa7fcbe7404d5c60e26bff1a0c8b8aa1f72ced49e7dd0210fe288fb7fe`.
It passed build, measured-policy preflight, conformance (replay distance 0),
submission and exact-byte/hash readback at isolated height 2688:
`378F825C27A1E689D775E6E8C9A53FDE715ABF1063EF7A7C1DFB36DAF2F93EC8`.
Its policy samples actual layers [0,12,23]. The repeated command left account
balance 9800, nonce 2: two distinct model publications, two bonds total.

Evidence: `testdata/s4/publish*.log`, account-after-repeat05.json and test logs.
Full `go test ./...` and `go test -race ./...` passed. Python's complete
node/core-py/chain-genesis/release/deploy/security suite passed (567 passed,
3 skipped, 68 subtests). Initial collection needed the security import path;
eight initial failures required diffusers 0.35.1, installed in a dedicated test
venv without changing the serving environment. Web Python suite: 12 passed;
site build passed. Missing policy, RPC failure vs absence, malformed layer
indices, corrupt readback, wrong publisher and invalid publication have tests.
