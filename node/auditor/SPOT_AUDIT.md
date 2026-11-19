# v14 auditor entry points

The existing signed `audit` / resumable `audit_begin`, `audit_chunk`,
`audit_commit` tunnel accepts a **metadata-only** job:

```json
{"kind":"spotcheck","genesis":"<pinned genesis bytes in hex>","mode":"normal"}
```

`mode` is `normal`, `substitute`, `escalation`, or `unopened`. Assignment,
commitment, challenge steps, challenge height, retry round and deadlines come
from the finalized `/hold/<request>` query. Escalation mode is allowed only for
chain round `esc`. Substitute mode recomputes every challenged step, without
reading any previous auditor progress. No executor tensor retransmission is
needed for delivered steps. Gateway supplements become available in the same
spool; missing steps produce a service error, never a fabricated verdict.

Join configuration:

- `--spot-spool-dir PATH`: read-only mounted gateway `OpeningService` spool,
  or authenticated durable mirror with the same metadata and local raw-file
  references. References must remain below the configured root.
- `--spot-context-dir PATH`: defaults to `STATE/spot-context`; contains
  `<request-id>.json` from the operator's authenticated request archive.
- The registered reference runtime must be CPU BF16. GPU jobs do not silently
  fall back to a different numerical path.

The context pins `request_id`, `profile_id`, `total_steps`, `input_anchor`,
`output_anchor`, `claimed_tokens`, `epsilon`, and `operations`. `operations` maps
one-based step strings to `input` / `output` dtype+shape and `operator`:
`start_layer`, `end_layer`, `has_embedding`, `has_lm_head`. A terminal token
projection uses `output_kind: "tokens"`; otherwise output is an activation.
Input token vectors are batch-expanded for the existing embedding forward.
The fixture in `test_spot_audit.Fixture` is an executable small context example.
The request archive must supply authenticated payer/input and claimed-output
token anchors and signed profile formats; an unsigned tunnel job cannot
provision or override them. Enabled chain equality parameters override archived
`epsilon`. The worker checks total steps against the finalized model.

`DirectorySpool(root, attestor=...)` supports directly requesting a gateway
absence statement. The read-only mirror adapter also reads gateway-issued
absence documents from spool; a missing local file never authorizes a claim.
For `unopened` mode, the job must additionally carry `spool_attestation` in the
chain's signed `SpoolAttestation` format, and the pinned context must include
`gateway_public` (hex). Both the gateway's detailed absence document and the
chain-format signature are verified; the auditor never re-signs gateway facts
as though they came from the gateway. See `unopened_action` for exact fields
and domain separators. The signed outer chain transaction is the auditor's
submitted accusation; its detailed signed statement is retained on disk.

Evidence is persisted beneath `STATE/spot-evidence/<request>/`. Each failure
bundle includes copied raw tensors, their signed SHA-256 digests, both Merkle
openings, pinned replay context, CPU semantic version, result and auditor
signature. `reproduce_evidence(path, registered_cpu_engine)` reproduces a result
without the source gateway spool or original job. The independent auditor uses
its own registered weights. `verdict.json` is the complete evidence index; its
hash becomes the signed verdict's evidence reference.

`spot_wire` supplies an auditor-local v14 descriptor with fields 14/15/16,
matching line A, until shared generated protobuf files arrive. It uses ordinary
deterministic protobuf signatures and automatically prefers the merged schema.
No shared generated file is changed by this line. `SpotStepResult` currently
has no individual evidence-reference field on line A; per-step references are
therefore in the content-addressed evidence index covered by `evidence_ref`.

Transport chunk hashing, disk spooling, resume offsets, multihome claims and
cached-verdict nonce refresh remain the established paths. Inference locks are
held for one replay step; disk reads, evidence writes and chain queries run
outside the inference lease. These are client implementation entry points;
release integration must provision the request archive and spool mirror and
align gateway attestation formats and legacy chain rejection validation (see
REPORT-C.md). This change does not activate an upgrade or touch production.
