`sd15-fp32-v1.json` pins every file loaded by the image adapter. Only the three
fp32 safetensors components and four slow CLIP tokenizer assets are loaded.
Download with `PYTHONPATH=core-py:proto:node python -m fetch_t2i
--destination /path/to/sd15` (add `--force` to re-download). The helper explicitly
disables environment proxies and verifies each file before replacing it.
The scheduler JSON and pipeline defaults are not read. Runtime versions are in
`node/requirements-t2i.txt`; install these in a dedicated inference environment.

`load_engine('diffusers-t2i', model_path=..., calibration=...)` is fail closed:
construction checks assets; the factory runs all 15 frozen T0 vectors plus a
full local replay against the reference ETV1 trajectory. Calibration JSON has
`request`, `reference_paths`, `latent_hashes`, `max_distances`, each list having
N+1 entries. Operational references and limits must be authenticated from the
published T4 calibration profile. `image-conformance` bootstraps local-only
references for repeatability testing; these are not cross-device certification.

JOIN accepts `--engine diffusers-t2i --model-path ... --image-calibration ...`.
It additionally checks the signed genesis execution profile and manifest pins.
`--image-pixel-policy` is a pinned T4 JSON with `mode` (`relative_l2`,
`within_one`, `either`, `both`), `max_relative_l2`, `min_within_one_fraction`.
There are no production numerical threshold defaults.

The T3 host provides `args.image_transport` when calling `agent.join.join(args)`
(or `agent.image_transport` for an embedded Agent) before serving image jobs:

* `gateway_public_key`: registered gateway Ed25519 raw public bytes.
* `spool(png, binding_hex, hold_id, hold)`: deliver PNG, return signed canonical
  Go `SpoolAttestation` JSON bytes. The client verifies signature and bindings.
* `opening(job, finalized_context)`: fetch the package requested by the chain
  challenge, returning the dictionary produced by `ImageClient.opening`.
  Tensor values are ETV1 bytes, not paths supplied by remote jobs.

The agent sends `image_result` with the existing signed chain TX envelope for
`submit_image`; audit returns `audit_result` with signed protobuf verdicts and
ordinary signed TX envelopes. The gateway submits these using its existing
transport and delivers the receipt to the chain (`image_received`, or includes
`spool` in submission). No gateway endpoint is inferred from a job. Missing
transport fails before image execution. This is the intentional T2/T3 seam.

Challenges use T1 leaf IDs 1..N. Leaf 0 is always opened to re-tokenize the
on-chain prompt/negative and verify the first anchor, including for challenges
that only draw interior steps. The core client also accepts explicit leaf 0 for
conformance testing. Generation and audit both call `DiffusersT2I.step` and
`Formula`; there is no second replay scheduler. Pixels use NumPy ties-to-even
rounding to RGB uint8, row-major; PNG uses compression 9, optimize false, no
metadata. Pixel hashes are the primary anchor, PNG hashes transport integrity.

Evidence is fsynced under request hash, refuses overwrite, and is not pruned
until the host observes finalized settlement and calls `mark_settled` plus the
configured safety margin. Failed delivery/submission retains the trajectory.
Do not use untrusted job fields as settlement time or audit context.

Local test (never sends a chain transaction):

    PYTHONPATH=core-py:proto:node python -m agent image-conformance \
      --model /path/to/sd15 --output /path/to/new/evidence --steps 2
