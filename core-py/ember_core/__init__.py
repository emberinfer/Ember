"""Deterministic protobuf signatures and evidence checks shared with core-go."""
import hashlib
import math
from google.protobuf.message import Message

SPARK = 1_000_000


def node_id(public: bytes) -> str:
    return hashlib.sha256(public).hexdigest()


def join_challenge(nonce: bytes) -> bytes:
    return b'ember/tunnel-join/v1\x00' + nonce
SIGNED = {"ChainGenesis", "ModelManifest", "ResourceProfile", "ExecutionCommitment", "ChunkRoot", "ExecutionReceipt", "AuditChallenge", "AuditVerdict", "LedgerTx", "ShardStepReceipt", "TunnelFrame", "ShardForwardRequest", "StageLatencyWitness"}


def payload(obj: Message) -> bytes:
    name = obj.DESCRIPTOR.name
    if name not in SIGNED:
        raise ValueError("unsupported signed object")
    clone = type(obj)()
    clone.CopyFrom(obj)
    clone.ClearField("signature")
    return b"ember/signed/v1/" + name.encode() + b"\x00" + clone.SerializeToString(deterministic=True)


def digest(obj: Message) -> bytes:
    return hashlib.sha256(payload(obj)).digest()


def sign(obj: Message, key) -> None:
    from cryptography.hazmat.primitives import serialization
    obj.signer = key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    obj.signature = key.sign(payload(obj))


def verify(obj: Message) -> None:
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    if len(obj.signer) != 32 or len(obj.signature) != 64:
        raise ValueError("invalid signature length")
    Ed25519PublicKey.from_public_bytes(obj.signer).verify(obj.signature, payload(obj))


def verify_chunks(chunks, request_id: str, total: int) -> None:
    previous, step = b"", 0
    for i, chunk in enumerate(chunks):
        verify(chunk)
        if (chunk.request_id != request_id or chunk.seq != i or not step < chunk.upto_step <= step + 16
                or chunk.prev_root != previous or len(chunk.merkle_root) != 32):
            raise ValueError(f"invalid chunk {i}")
        previous, step = digest(chunk), chunk.upto_step
    if step != total:
        raise ValueError("chunk coverage mismatch")


def within_tolerance(a, b, tolerance):
    return (len(a) == len(b) and len(a) > 0 and tolerance >= 0 and math.isfinite(tolerance)
            and all(math.isfinite(x) for x in [*a, *b])
            and math.sqrt(sum((x - y) ** 2 for x, y in zip(a, b))) / max(math.sqrt(sum(y*y for y in b)), 1e-12) <= tolerance)
