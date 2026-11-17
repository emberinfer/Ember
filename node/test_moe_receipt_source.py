"""The gateway archive and block-history fallback share the same chain anchors."""
import base64
import copy
import hashlib
from pathlib import Path
import sys
import tempfile
import unittest

# The test remains directly runnable using the repository's node-test convention.
sys.path[:0] = [str(Path(__file__).resolve().parents[1] / 'proto'),
                str(Path(__file__).resolve().parent)]

from ember_pb2 import MoeExecutionData, PipelineExecutionReceipt
from auditor.receipt_source import load_moe_receipt, moe_receipt_for_job


class MoeReceiptSourceTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        receipt = PipelineExecutionReceipt(request_id='hold-1', input_tokens=1,
                                           output_tokens=1, elapsed_ms=1,
                                           moe=MoeExecutionData())
        self.raw = receipt.SerializeToString(deterministic=True)
        self.ref = hashlib.sha256(b'ember/pipeline-receipt/v1\x00' + self.raw).hexdigest()
        self.hold = {'id': 'hold-1', 'receipt_ref': self.ref,
                     'moe': {'chunk_total': 1, 'chunk_bytes': len(self.raw),
                             'chunk_root': base64.b64encode(hashlib.sha256(self.raw).digest()).decode()}}
        self.path = self.root / 'hold-1' / 'receipt.pb'
        self.path.parent.mkdir()
        self.path.write_bytes(self.raw)
        self.path.chmod(0o600)

    def test_archive_and_legacy_have_identical_bytes(self):
        self.assertEqual(load_moe_receipt(self.hold, self.root), self.raw)
        legacy = {'id': 'hold-1', 'receipt_ref': self.ref,
                  'moe': {'receipt': base64.b64encode(self.raw).decode()}}
        self.assertEqual(load_moe_receipt(legacy), self.raw)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)

    def test_job_uses_archive_and_checks_inline_receipt(self):
        job = {'hold': self.hold, 'evidence_root': self.root}
        self.assertEqual(moe_receipt_for_job(job), self.raw)
        job['receipt'] = self.raw.hex()
        self.assertEqual(moe_receipt_for_job(job), self.raw)
        job['receipt'] = '00'
        with self.assertRaises(ValueError):
            moe_receipt_for_job(job)
        self.assertEqual(moe_receipt_for_job({'receipt': self.raw.hex()}), self.raw)

    def test_missing_and_corrupt_archive_fail_closed(self):
        self.path.unlink()
        with self.assertRaises(FileNotFoundError):
            load_moe_receipt(self.hold, self.root)
        # Block-history recovery can restore exactly the original file.
        self.path.write_bytes(self.raw)
        self.assertEqual(load_moe_receipt(self.hold, self.root), self.raw)
        self.path.write_bytes(b'x' * len(self.raw))
        with self.assertRaisesRegex(ValueError, 'chunk root'):
            load_moe_receipt(self.hold, self.root)

    def test_each_anchor_is_required(self):
        for field, value in [('chunk_root', base64.b64encode(b'x' * 32).decode()),
                             ('chunk_bytes', len(self.raw) + 1)]:
            hold = copy.deepcopy(self.hold)
            hold['moe'][field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                load_moe_receipt(hold, self.root)
        hold = copy.deepcopy(self.hold)
        hold['receipt_ref'] = '00' * 32
        with self.assertRaisesRegex(ValueError, 'receipt ref'):
            load_moe_receipt(hold, self.root)

    def test_unsafe_hold_ids_and_symlink_escape_fail_closed(self):
        for hold_id in ['../hold-1', '/hold-1', '.', '..', 'a/b', '', 'x' * 257]:
            hold = copy.deepcopy(self.hold)
            hold['id'] = hold_id
            with self.subTest(hold_id=hold_id), self.assertRaises(ValueError):
                load_moe_receipt(hold, self.root)
        with tempfile.TemporaryDirectory() as outside:
            target = Path(outside) / 'receipt.pb'
            target.write_bytes(self.raw)
            self.path.unlink()
            self.path.symlink_to(target)
            with self.assertRaisesRegex(ValueError, 'escapes'):
                load_moe_receipt(self.hold, self.root)

    def test_wrong_request_and_missing_chain_fields_fail_closed(self):
        legacy = {'id': 'hold-2', 'receipt_ref': self.ref,
                  'moe': {'receipt': base64.b64encode(self.raw).decode()}}
        with self.assertRaisesRegex(ValueError, 'differs from MoE hold'):
            load_moe_receipt(legacy)
        hold = copy.deepcopy(self.hold)
        del hold['receipt_ref']
        with self.assertRaises(ValueError):
            load_moe_receipt(hold, self.root)
        hold = copy.deepcopy(self.hold)
        hold['moe']['chunk_root'] = '!invalid!'
        with self.assertRaises(ValueError):
            load_moe_receipt(hold, self.root)
        with self.assertRaises(ValueError):
            load_moe_receipt(self.hold)


if __name__ == '__main__':
    unittest.main()
