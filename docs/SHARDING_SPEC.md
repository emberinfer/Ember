# General sharding specification, version 2

This specification succeeds the layer-only model in SHARDING.md. Normative
units describe execution and attribution; they do not imply that every
architecture family has an executor. The implementation table below distinguishes
implemented Qwen2 units from specification-only cuts.

## Profile and unit identity

A profile fixes its signed model manifest, origin artifacts, dtype, engine/runtime,
tokenizer, topology, audit evidence and tolerance bands. The domain-separated
profile hash and manifest root identify these choices. A unit is identified by
that profile plus architecture family, unit kind, half-open block range and,
where applicable, half-open expert range or intra-block part. Changing cuts,
router policy, tied-weight handling or boundary representation requires a new
signed profile. Endorsements do not authorize a different layout.

| Family | Atomic unit and allowed cuts | Boundary state | Status |
| --- | --- | --- | --- |
| Dense transformer (Qwen2 reference) | embedding; complete decoder block; final normalization + lm_head. Layer cuts only between complete blocks. Adjacent blocks may be grouped. | Token IDs enter embedding; BF16 hidden tensor [1, sequence, hidden] crosses block boundaries; head produces ordered top-16 raw logits and the selected token. | Implemented: legacy combined embedding/first blocks and last blocks/head; explicit embedding-only and lm_head-only stages. |
| Llama-like dense transformers | Same atomic units; RoPE/norm/residual ordering is architecture-specific and must be fixed in the profile. | Hidden tensor, position IDs, mask semantics and cache policy. | Specification only; generic whole-model adapter support is not a partial-stage implementation. |
| Sparse transformer / MoE (Qwen MoE, Mixtral, DeepSeek families) | Complete block by default. An expert-range cut groups complete expert FFNs [e0,e1) within one named layer. Router and reduction are separate units; shared experts are separate or explicitly included once. | Router input and exact selected expert IDs, routing weights, token permutation, capacity/drop policy and deterministic reduction order. Expert input/output commitments name layer and expert range. | Specification only in this release; no small compatible MoE checkpoint is available in the acceptance environment. |
| Attention / MLP inside a block | Attention (including Q/K/V, mask and rotary policy), residual/norm, MLP and final residual are distinct units. Cuts may occur only at the named architecture-specific sub-operation boundary, never through an implicit residual edge. | Residual stream plus normalized attention/MLP input; attention position/mask state; KV state where incremental execution is enabled. Every live residual edge must be committed. | Specification only. Reference implementation executes complete decoder blocks. |
| Tensor/head/expert tensor parallelism | Only an explicitly declared tensor slice and collective/reduction contract may be an atomic unit. Arbitrary byte/tensor cuts are forbidden. | Partition indices, global shapes, collective membership/order, pre/post-reduction commitments. | Specification only; no collective executor or settlement claim. |

The reference implementation uses no KV cache: each step recomputes the full
forced token prefix. A future incremental profile must commit the prior KV root,
sequence length, cache positions, position/mask policy and resulting KV root.
Cache transfer alone never proves valid execution. Cross-runtime calibration
must be explicit; an unsupported runtime pair is inconclusive, not slashable.

## Reference manifest and boundaries

`ShardSpec` fixes index, start_layer/end_layer, embedding/head ownership, artifact
names, parameter hash and optional unit_kind. Layer coordinates always refer to
actual decoder layers [a,b). An embedding-only unit is [0,0) with embedding
ownership and unit_kind=embedding; the standalone head is [L,L) with head
ownership and unit_kind=lm_head. A decoder unit has a<b and unit_kind=layers.
Empty decoder ranges, interior embedding/head units, duplicate ownership,
missing units, gaps and overlaps are invalid. Legacy manifests leave unit_kind
empty and retain combined endpoint stages.

For ordering only, the embedding occupies position [0,1), decoder layer k
occupies [k+1,k+2), and the head occupies [L+1,L+2). These positions are not
reported as extra model layers. A valid per-token pipeline covers [0,L+2)
exactly once. A legacy combined first/final stage includes its corresponding
endpoint position. This makes [embedding][layer range(s)][lm_head] unambiguous
without changing the meaning of the existing layer coordinates.

An activation commitment at decoder boundary k is SHA-256 over:

```
"ember/activation-boundary/v1\0" || manifest_root ||
uint32_be(k) || uint32_be(rank) || each uint32_be(shape_dimension) || raw_bf16
```

The reference tensor is dense, contiguous, little-endian BF16 with shape
[1,sequence,hidden], and preserves the full prefix. Profile artifacts fix the
shape/model dimensions and numerical format. Embedding output commits boundary
0; layer [a,b) consumes a and produces b; head consumes boundary L. The first
unit's token IDs and final logits/token are bound by signed request/response
hashes rather than being mislabeled hidden activations. Every invocation also
binds request ID, token step, profile/manifest, executed range, unit kind,
parameter hash, scheduler-signed request hash and response hash. Neighbors'
activation commitments must match; invocations/openings retain the bytes so an
auditor can recompute them. Signature/commitment consistency is necessary but
does not replace numerical replay.

## Execution evidence and auditing

The scheduler sends signed, nonce-protected requests over certificate-pinned
channels. Each executor signs its own ShardStepReceipt. The identity-less
aggregate cannot invent a stage identity or claim an unexecuted range. Consensus
freezes stake-backed, unexpired registrations for each authorized stage and
failover alternative before execution, then verifies signatures, those frozen
bindings, exact per-token unit coverage and adjacent activation commitments.
The gateway additionally validates the complete request/response hidden-state
chain. The audit job binds every opening to the aggregate receipt reference.

The reference auditor reconstructs the complete pipeline from signed responses,
checks token-prefix and hidden-state continuity, and replays the generated token
prefix on the complete model. Sampled layer activations and top-k raw logits are
compared under the signed band. Standalone embedding/head units add no fictional
sampled decoder layer. For specification-only MoE/intra-block profiles, replay
must reproduce the router, expert assignment/reduction and every residual edge;
a whole-layer opening without those details cannot attribute an expert or
attention/MLP fault.

The first consensus implementation selects a genesis-authorized full-model
auditor whose identity differs from every authorized stage. Audit signatures
and selection proof are required before settlement. This pipeline reference
path is not yet the registered distributed whole-model audit-worker route.
Two independent auditor identities are required for rejection/slashing; absence
of a second reference produces service failure. No claim of independent
organizations or hardware follows from distinct identities on one host.

## Settlement attribution

For a legacy pinned layout, work remains executed decoder layers times output
tokens. A profile with explicit boundary units assigns weight 1 to each
embedding/head invocation and one per executed decoder layer; total weight is
L+2 per output token. No stage is paid for advertised but unexecuted coverage.
Integer fees split by verified work using largest remainders, with account ID
as the deterministic tie-break; all subunits are conserved. Alternatives reserve
stake exposure, but only executed signed invocations receive credits. Combined
legacy endpoint units keep their previous layer weights and payouts.

For MoE expert-range profiles the manifest must fix a work metric before any
registration: e.g. expert-token invocations weighted by signed expert dimensions,
plus separately weighted router/reducer/shared-expert units. Routed expert
counts must come from committed routing evidence and audited execution, never
an executor's unverified FLOP claim. Attention/MLP profiles similarly fix positive
part weights and prohibit counting overlapping residual work twice. These
metrics are specified here and are not implemented or accepted as consensus
credits in this release.

Expired registrations cannot authorize new holds; stake remains exposed for
pending holds. Rejected/inconclusive executions refund the consumer; confirmed
faults can slash only signed, credited fault identities. Missing evidence or an
unsupported cut must fail closed. Non-streaming failover can select an authorized
alternative with the same executed unit boundary; already-streamed output is
not transparently migrated. Dynamic sub-range capabilities and measured latency are implemented separately
in [SCHEDULING.md](SCHEDULING.md); the pinned reference layout retains the
unit and attribution rules above.
