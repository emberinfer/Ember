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

## 2. Explicit CUDA runtime family

Read-only evidence: public RPC `http://relay.emberinfer.org:39657`, ABCI `/nodes`,
height **414581**. ResourceProfile protobuf signatures were verified with
ember_core.verify; each listed registration was unexpired at that height.
This authenticates the signed runtime declarations, not an independent proof
of RPC height, uptime or remote GPU hardware.

| Node account | Model | Engine/version | torch | CUDA | Accelerator | Expires height |
| --- | --- | --- | --- | --- | --- | --- |
| 71e326435e721d66f6285759a2d5be56d90849e5a6e48cff24b4e95fe2b8c48a | 27B | vllm/unknown (remote) | not declared | not declared | not declared | 614566 |
| 5c2decbc9ce1c0d7bf937dd0c4a8917ca4ea5da947dc0b8e6d931e28ff393dd1 | 1.5B | transformers/4.45.2 | 2.5.1+cu124 | 12.4 | NVIDIA A16 | 610094 |
| 74a141e61d84b64f6db80c1a74358bfe32b12c0538fee21b2ca49b3a1660e25e | 1.5B | transformers/4.45.2 | 2.5.1+cu124 | 12.4 | RTX 2080 Ti | 614509 |
| e0d9d8825fa7646a5cd7b69ca243761a5318804b5071ef6bd054c2f211d17856 | 1.5B | transformers/4.45.2 | 2.5.1+cu124 | 12.4 | RTX 2080 Ti | 614507 |

The 27B remote profile cannot substantiate a vLLM torch/CUDA pin. The explicit
x86_64 Transformers CUDA family uses the measured active CUDA combination,
shared pinned SPI requirements and driver >=550.54.14. CPU selection is preserved
and prints an NVIDIA notice without changing families. Missing GPU, old driver,
unsupported architecture and unknown family fail before copying.

Both amd64 and arm64 unsigned regression bundles build and pass import closure;
only amd64 includes requirements-cuda.txt. Real fresh-prefix installs of both
CPU and CUDA completed from the final bundle, with the temporary venv probe.
The CUDA wheel hash matches the upstream index:
`bf6484bfe5bc4f92a4a1a1bf553041505e19a911f717065330eb061afe0e14d7`.
Downloads were slow; real pip installation used the verified wheel plus locally
cached dependency wheels, without altering runtime requirements or the serving
environment. CPU actual runtime is 2.5.1+cpu / CUDA None / Transformers 4.45.2;
CUDA actual runtime is 2.5.1+cu124 / 12.4 / Transformers 4.45.2 and a real device
allocation passed on RTX 2080 Ti, driver 580.105.08. Adapter declarations continue
to come from loaded torch/Transformers rather than configured strings.

Real CPU/CUDA forced-prefix replay on the downloaded 24-layer model measured
same-device distances 0 for both prompts; cross-device union distances were
0.03990639675440458 and 0.018803347543083035. The explicit acceptance-only policy
records these measurements and uses observe bands; it makes no production false
rate or cross-device slashing claim. The installed command also built and
conformance-verified a new dry-run profile from its packaged builder modules,
proving this path works outside the source checkout.
