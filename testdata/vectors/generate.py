"""Regenerate signed protobuf vectors with a fixed, publicly known test key."""
import json
import pathlib
import sys
sys.path[:0] = [str(pathlib.Path(__file__).resolve().parents[2] / "proto"), str(pathlib.Path(__file__).resolve().parents[2] / "core-py")]
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
import ember_pb2 as pb
from ember_core import sign, digest

key = Ed25519PrivateKey.from_private_bytes(bytes(range(32)))
items = [
    pb.ChainGenesis(chain_id="ember-vector", model_manifest_root=b"m"*32, tokenizer_hash=b"t"*32, quant="mock", dtype="fp16", execution_profile_ver="v1", tolerance_table_ref="test", price_input=1, price_output=10, audit_params=pb.AuditParams(new_node_percent=30, mature_node_percent=5, floor_percent=2, inline_percent=3, canary_percent=1, slash_multiplier=20, min_stake_multiplier=50, exposure_divisor=5, dispute_seconds=60, numeric_tolerance=0.01)),
    pb.ModelManifest(chain_id="ember-vector", artifacts=[pb.Artifact(name="test", sha256=b"a"*32, size=12, source="file")], license="test"),
    pb.ResourceProfile(node_id="node", chain_id="ember-vector", gpu="mock", memory_bytes=1024, engine_ver="mock-v1", expires_unix=2000000000),
    pb.ExecutionCommitment(request_id="req", chain_id="ember-vector", input_commitment=b"i"*32, profile_ver="v1", sampling_params_hash=b"p"*32, seed=42, client_nonce=b"fresh"),
]
chunks = []
previous = b""
for seq, upto in enumerate((16, 20)):
    c = pb.ChunkRoot(request_id="req", seq=seq, upto_step=upto, merkle_root=bytes([seq+1])*32, prev_root=previous)
    sign(c, key)
    chunks.append(c)
    previous = digest(c)
items.extend(chunks)
items.extend([
    pb.ExecutionReceipt(request_id="req", chunk_roots=chunks, input_tokens=2, output_tokens=20, node_id="node"),
    pb.AuditChallenge(receipt_ref=b"r"*32, positions=[1, 10, 19], layers=[2, 4], deadline_unix=2000000000, vrf_proof=b"proof"),
    pb.AuditVerdict(challenge_ref=b"c"*32, auditor_id="auditor", distances=[0.001, 0.002], verdict="accept"),
    pb.LedgerTx(to="node", amount=123, type="subsidy", tx_id="tx"),
])
for item in items:
    if not item.signature:
        sign(item, key)
print(json.dumps([{"type": type(i).__name__, "wire": i.SerializeToString(deterministic=True).hex(), "digest": digest(i).hex()} for i in items], indent=2))
