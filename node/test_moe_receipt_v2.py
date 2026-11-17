"""Transport v2 preserves logical v1 signatures, roots and bindings exactly."""
import base64
import hashlib
from pathlib import Path
import sys
import tempfile
import unittest

sys.path[:0] = [str(Path(__file__).resolve().parents[1] / 'proto'),
                str(Path(__file__).resolve().parents[1] / 'core-py'),
                str(Path(__file__).resolve().parent)]

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import verify
from ember_core import moe as moe_core
from ember_pb2 import (ChainGenesis, ModelManifest, MoeExecutionData,
                       PipelineExecutionReceipt)
from auditor.receipt_source import load_moe_receipt
from moe_client import MoeAssembler
from moe_rpc import ReceiptSigner


def signed_fixture():
    key = Ed25519PrivateKey.from_private_bytes(bytes(range(32)))
    manifest = ModelManifest(chain_id='c' * 24)
    genesis = ChainGenesis(chain_id=manifest.chain_id,
                           execution_profile_ver='p' * 12)
    signer = ReceiptSigner(key, genesis, manifest)
    request_id = 'r' * 38
    segments = [signer.segment(request_id=request_id, kind='expert', frame=1,
                                layer=i % 16, expert=i % 64,
                                input_hash=b'i' * 32, output_hash=b'o' * 32,
                                routing_record_hash=b'r' * 32,
                                params_hash=b'p' * 32, token_rows=1)
                for i in range(130)]
    hashes = [moe_core.segment_receipt_hash(segment) for segment in segments]
    frames = [signer.frame(request_id, 1, hashes) for _ in range(4)]
    return PipelineExecutionReceipt(
        request_id=request_id, input_tokens=1, output_tokens=1, elapsed_ms=1,
        output_commitment=b'o' * 32,
        moe=MoeExecutionData(segment_receipts=segments, frame_receipts=frames))


def wire(receipt):
    return receipt.SerializeToString(deterministic=True)


class MoeReceiptV2Tests(unittest.TestCase):
    def test_real_signatures_and_roots_survive_pure_expansion(self):
        original = signed_fixture()
        v1 = wire(original)
        compact = moe_core.compact_moe_receipt(original)
        compact_bytes = wire(compact)
        expanded = moe_core.expand_moe_receipt(compact)
        self.assertEqual(wire(expanded), v1)
        self.assertEqual(wire(original), v1)
        self.assertEqual(wire(compact), compact_bytes)
        for segment in expanded.moe.segment_receipts:
            verify(segment)
        hashes = [moe_core.segment_receipt_hash(s) for s in expanded.moe.segment_receipts]
        for frame in expanded.moe.frame_receipts:
            verify(frame)
            self.assertEqual(frame.segments_root, moe_core.frame_root(hashes))
        expanded.moe.segment_receipts[0].node_id = 'changed clone'
        self.assertEqual(wire(original), v1)
        self.assertEqual(wire(compact), compact_bytes)

    def test_each_header_constant_is_inside_existing_signed_payload(self):
        original = signed_fixture()
        for field, value in [('chain_id', 'other-chain'),
                             ('manifest_root', b'x' * 32),
                             ('execution_profile_ver', 'other-profile'),
                             ('request_id', 'other-request')]:
            compact = moe_core.compact_moe_receipt(original)
            setattr(compact.moe.header, field, value)
            if field == 'request_id':
                # Keep aggregate/header structurally consistent so existing
                # cryptographic verification, rather than shape checks, fails.
                compact.request_id = value
            expanded = moe_core.expand_moe_receipt(compact)
            with self.subTest(field=field), self.assertRaises(InvalidSignature):
                verify(expanded.moe.segment_receipts[0])

    def test_emission_is_default_off_and_assembler_flag_is_explicit(self):
        original = signed_fixture()
        default = ReceiptSigner.execution(original)
        self.assertEqual(wire(default), wire(original))
        self.assertEqual(default.moe.format_version, 0)
        compact = ReceiptSigner.execution(original, receipt_v2=True)
        self.assertEqual(compact.moe.format_version, 2)
        self.assertEqual(wire(moe_core.expand_moe_receipt(compact)), wire(original))
        manifest = ModelManifest()
        self.assertFalse(MoeAssembler([], None, manifest, ChainGenesis()).receipt_v2)
        self.assertTrue(MoeAssembler([], None, manifest, ChainGenesis(),
                                     receipt_v2=True).receipt_v2)

    def test_strict_formats_and_duplicate_constant_fields(self):
        original = signed_fixture()
        for version in (1, 3):
            original.moe.format_version = version
            with self.assertRaises(ValueError):
                moe_core.expand_moe_receipt(original)
        original.moe.format_version = 0
        original.moe.header.chain_id = 'unexpected'
        with self.assertRaises(ValueError):
            moe_core.expand_moe_receipt(original)
        original.moe.ClearField('header')
        compact = moe_core.compact_moe_receipt(original)
        self.assertEqual(wire(moe_core.compact_moe_receipt(compact)), wire(compact))
        compact.moe.segment_receipts[0].chain_id = 'duplicate'
        with self.assertRaises(ValueError):
            moe_core.expand_moe_receipt(compact)
        compact = moe_core.compact_moe_receipt(original)
        compact.moe.frame_receipts[0].request_id = 'duplicate'
        with self.assertRaises(ValueError):
            moe_core.expand_moe_receipt(compact)
        compact = moe_core.compact_moe_receipt(original)
        compact.moe.ClearField('header')
        with self.assertRaises(ValueError):
            moe_core.expand_moe_receipt(compact)

    def test_exact_field_savings_and_representative_frame_ratio(self):
        original = signed_fixture()
        compact = moe_core.compact_moe_receipt(original)
        for a, b in zip(original.moe.segment_receipts, compact.moe.segment_receipts):
            self.assertEqual(a.ByteSize() - b.ByteSize(), 114)
            self.assertEqual(a.signature, b.signature)
        for a, b in zip(original.moe.frame_receipts, compact.moe.frame_receipts):
            self.assertEqual(a.ByteSize() - b.ByteSize(), 40)
            self.assertEqual(a.signature, b.signature)
            self.assertEqual(a.segments_root, b.segments_root)
        self.assertLess(len(wire(compact)), .8 * len(wire(original)))

    def test_archive_anchors_wire_root_and_logical_receipt_ref(self):
        original = signed_fixture()
        compact = moe_core.compact_moe_receipt(original)
        raw = wire(compact)
        receipt_ref = hashlib.sha256(b'ember/pipeline-receipt/v1\x00' + wire(original)).hexdigest()
        hold = {'id': original.request_id, 'receipt_ref': receipt_ref,
                'moe': {'chunk_total': 1, 'chunk_bytes': len(raw),
                        'chunk_root': base64.b64encode(hashlib.sha256(raw).digest()).decode()}}
        with tempfile.TemporaryDirectory() as root:
            directory = Path(root) / original.request_id
            directory.mkdir()
            (directory / 'receipt.pb').write_bytes(raw)
            self.assertEqual(load_moe_receipt(hold, root), raw)
            hold['receipt_ref'] = hashlib.sha256(b'ember/pipeline-receipt/v1\x00' + raw).hexdigest()
            with self.assertRaisesRegex(ValueError, 'receipt ref'):
                load_moe_receipt(hold, root)
        legacy = {'id': original.request_id, 'receipt_ref': receipt_ref,
                  'moe': {'receipt': base64.b64encode(raw).decode()}}
        self.assertEqual(load_moe_receipt(legacy), raw)


if __name__ == '__main__':
    unittest.main()
