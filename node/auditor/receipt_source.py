"""MoE audit receipt acquisition using a finalized chain hold as the anchor.

The external fleet driver can call ``load_moe_receipt(hold, evidence_root)``
with its chain-query result before building an audit job. Chunked receipts
are copied by the gateway to ``evidence_root/<hold-id>/receipt.pb``. If this
copy is unavailable, the block-history tool can restore the same file; this
loader never substitutes unauthenticated bytes or permits an audit without
both the whole-receipt hash and the chain's domain-separated receipt ref.
"""
import base64
import hashlib
from pathlib import Path
import re

from ember_pb2 import PipelineExecutionReceipt

_MAX_RECEIPT_BYTES = 256 * 1024 * 1024
_SAFE_HOLD_ID = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,255}\Z')


def _chain_bytes(value, field):
    try:
        if not isinstance(value, str):
            raise ValueError('expected base64 string')
        return base64.b64decode(value, validate=True)
    except (ValueError, TypeError) as exc:
        raise ValueError(f'invalid chain {field}') from exc


def load_moe_receipt(hold, evidence_root=None):
    """Return authenticated proto bytes; missing/corrupt archives raise.

    ``hold`` must be obtained from the finalized chain, rather than supplied
    by the gateway. Legacy holds use their inline receipt. Chunked holds
    require a local gateway archive (or an independently reconstructed copy).
    """
    hold_id = hold.get('id')
    if not isinstance(hold_id, str) or not _SAFE_HOLD_ID.fullmatch(hold_id):
        raise ValueError('unsafe MoE hold id')
    moe = hold.get('moe')
    if not isinstance(moe, dict):
        raise ValueError('not a MoE hold')
    root = None
    if moe.get('chunk_total', 0):
        root = _chain_bytes(moe.get('chunk_root'), 'chunk root')
        if len(root) != 32:
            raise ValueError('invalid chain chunk root')
        if evidence_root is None:
            raise ValueError('chunked receipt requires an archive root')
        archive_root = Path(evidence_root).resolve()
        path = (archive_root / hold_id / 'receipt.pb').resolve()
        if not path.is_relative_to(archive_root):
            raise ValueError('receipt archive escapes evidence root')
        # Bound the read even if an untrusted local archive has been replaced.
        with path.open('rb') as stream:
            raw = stream.read(_MAX_RECEIPT_BYTES + 1)
        if len(raw) != moe.get('chunk_bytes'):
            raise ValueError('receipt archive length differs from chunk manifest')
        if hashlib.sha256(raw).digest() != root:
            raise ValueError('receipt archive differs from chunk root')
    else:
        raw = _chain_bytes(moe.get('receipt'), 'MoE receipt')
    if not raw or len(raw) > _MAX_RECEIPT_BYTES:
        raise ValueError('invalid MoE receipt size')
    receipt = PipelineExecutionReceipt.FromString(raw)
    if receipt.request_id != hold_id or not receipt.HasField('moe'):
        raise ValueError('receipt differs from MoE hold')
    try:
        receipt_ref = bytes.fromhex(hold['receipt_ref'])
    except (KeyError, ValueError, TypeError) as exc:
        raise ValueError('invalid chain receipt ref') from exc
    canonical = receipt.SerializeToString(deterministic=True)
    expected_ref = hashlib.sha256(b'ember/pipeline-receipt/v1\x00' + canonical).digest()
    if len(receipt_ref) != 32 or expected_ref != receipt_ref:
        raise ValueError('receipt differs from chain receipt ref')
    return raw


def moe_receipt_for_job(job):
    """Resolve optional finalized hold metadata before auditor replay.

    Legacy external drivers that already provide an authenticated receipt
    retain their existing job interface. Drivers supplying ``hold`` activate
    archive resolution and both anchor checks; a conflicting inline receipt
    is rejected rather than silently used.
    """
    if 'hold' not in job:
        return bytes.fromhex(job['receipt'])
    raw = load_moe_receipt(job['hold'], job.get('evidence_root'))
    if 'receipt' in job and bytes.fromhex(job['receipt']) != raw:
        raise ValueError('job receipt differs from finalized MoE receipt')
    return raw
