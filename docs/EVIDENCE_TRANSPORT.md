# Evidence transport

Single-executor full-evidence requests use a governed token frame bound and a
content-addressed off-chain store. Numerical comparison, auditor assignment,
fees, refunds, and settlement shares retain their existing rules.

## Parameter and compatibility

`parameters.evidence.max_frame_tokens` is an unsigned integer in the chain's
application genesis. Omitted or zero selects **512 tokens**. It is immutable for
the chain, like the other genesis parameters; changing it requires the coordinated
new-genesis procedure in [Upgrade](UPGRADE.md). The gateway reads `/parameters`
before dispatch, and includes the resolved value as `evidence_max_frame_tokens`
in the signed execution job. Nodes, gateway admission of evidence frames,
chain receipt validation, the fetch server, and auditors use this value.

A token here means one output-token opening, including its logits and activation
bytes. A frame also has a 3 MiB serialized JSON byte budget, leaving room for its
protobuf envelope under gRPC's standard 4 MiB message limit. This byte budget can
produce smaller chunks; it is not another token cap. A single opening larger
than this byte budget fails the request.

When all openings fit the token and byte bounds, the node sends the existing
canonical JSON `evidence` frame and signs the unchanged receipt representation
with no manifest field. Existing single-frame evidence bytes and verdict
references remain unchanged. The signed execution commitment groups of 16 steps
are independent of transport chunks and retain their existing representation.

## Manifest and transfer

For larger bundles, the executor:

1. Splits the ordered openings into nonempty JSON arrays, respecting both bounds.
   JSON is UTF-8, keys sorted, compact separators, and nonfinite numbers forbidden.
2. Computes each chunk hash as SHA-256 of the exact JSON array bytes.
3. Builds a binary Merkle tree over the ordered chunk hashes. An interior node is
   `SHA256(0x01 || left || right)`; an odd last node is duplicated at each level.
4. Signs an `ExecutionReceipt` containing `evidence_manifest` with `merkle_root`,
   `chunk_count`, `total_tokens`, and ordered `chunk_hashes`.
5. Sends ordered `evidence_chunk` tunnel frames containing serialized
   `EvidenceChunk {manifest_root, index, data}`, followed by the signed receipt.

The gateway writes chunk bytes durably to `<db>.evidence` under each chunk’s hexadecimal SHA-256 filename using
atomic rename. It verifies each received index, root, hash, token bound, and total
coverage against the signed manifest before submission. It queues an audit job
containing the receipt and manifest, without inline openings. The chain validates
the manifest structure, Merkle root, and receipt token count, and anchors the
complete manifest in the hold's `evidence_manifest` field. Receipts exceeding the
token bound without a manifest are rejected. Manifests must contain at least two
chunks; byte-based splitting may require a manifest below the token bound.

## FetchEvidence RPC

`NodeTunnel.FetchEvidence(TunnelFrame) -> EvidenceChunk` uses the existing signed
frame format. The request has:

- `kind = "fetch_evidence"`;
- `request_id` equal to the finalized hold ID;
- `node_id` equal to the SHA-256 identity of the signing key;
- `payload` containing a serialized `EvidenceChunk` with only `manifest_root`
  and the zero-based `index` set.

The server verifies the signature and reads the finalized hold. Only its assigned
auditor, executor, or payer can fetch its evidence. An unassigned auditor has no
access. The requested root must equal that hold's anchored root, and the index
must be listed in its manifest. Unknown/unauthorized roots and callers return
`PERMISSION_DENIED`, unknown indices return `NOT_FOUND`, malformed queries return
`INVALID_ARGUMENT`, and missing/corrupt files return `UNAVAILABLE`. Reads may be
retried; signed fetch requests do not mutate account nonces or settlement state.
Use the same pinned TLS channel as the tunnel; plaintext is limited to explicit
loopback testing.

Before serving a chunk the server verifies its stored hash and token bound.
Before use the assigned auditor independently verifies every fetched hash,
manifest root, frame coverage, and final token count, then runs the existing
signed-opening checks and replay. The signed `AuditVerdict.evidence_ref` cites
the manifest root; the challenge still binds the signed receipt. The chain rejects
chunked verdicts citing another root. The local reference path also verifies
stored chunks before passing reassembled openings to its replay subprocess.

Missing, unfetchable, malformed, or hash-mismatched chunks never authorize an
accept verdict. An assigned worker signs `inconclusive` with the manifest root;
the existing chain rules derive `service_failure`, and the coordinator settles
with the existing refund rules. If the worker or its connection fails entirely,
existing hold expiry and recovery refund the request. An incomplete execution
transfer fails before submission and follows the existing cancellation path.

## Operations and limits

The SPI generation deadline defaults to 300 seconds. For slow, long generations,
set the node's `join --generation-timeout` (seconds) and the gateway's
`--request-timeout` to sufficient budgets; for example, `1200` and `20m`.
Set `--audit-timeout` to allow full-prefix replay as well. These operator time
budgets do not change evidence framing or inference outputs.

Keep the evidence directory together with the gateway database and audit spool
across restarts. Pending audits require these files. This MVP has no automatic
retention/garbage collection, range fetching, resumable upload, or per-request disk
quota; provision disk and retain chunks for as long as counterparties need them.
Tunnel replies apply backpressure instead of a fixed message-count queue, and
heartbeats continue during continuous token output. Pipeline receipts keep their
existing transport. This change requires application
version 4 because manifest validation and verdict binding change consensus
transaction acceptance; do not mix validator application versions.
