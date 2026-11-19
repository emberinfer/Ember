"""Integrity, ordering, retry, identity and cleanup of audit pieces."""
import hashlib
import json
from pathlib import Path
import unittest

from agent.audit_transfer import AuditTransfer, CHUNK_BYTES
from ember_pb2 import EvidenceChunk, TunnelFrame


class AuditTransferTest(unittest.TestCase):
    def setUp(self):
        self.receiver = AuditTransfer()
        self.addCleanup(self.receiver.close)
        self.data = b'x' * CHUNK_BYTES + b'y'
        self.root = hashlib.sha256(self.data).digest()
        self.manifest = json.dumps(dict(version=1, sha256=self.root.hex(),
                                       size=len(self.data), chunks=2)).encode()

    def frame(self, kind, payload, request='req'):
        return TunnelFrame(request_id=request, kind=kind, payload=payload)

    def begin(self):
        return self.receiver.accept(self.frame('audit_begin', self.manifest))

    def chunk(self, index, data, request='req'):
        return self.receiver.accept(self.frame('audit_chunk', EvidenceChunk(
            manifest_root=self.root, index=index, data=data).SerializeToString(), request))

    def finish(self):
        path = Path(self.receiver.finish(self.frame('audit_commit', self.manifest)))
        self.addCleanup(path.unlink, missing_ok=True)
        return path.read_bytes()

    def test_retry_and_roundtrip(self):
        self.assertEqual(self.begin(), self.root + (0).to_bytes(4, 'big'))
        self.chunk(0, self.data[:CHUNK_BYTES])
        self.assertEqual(self.begin(), self.root + (1).to_bytes(4, 'big'))  # resume prefix
        self.chunk(0, self.data[:CHUNK_BYTES])  # lost piece ack
        self.chunk(1, b'y')
        file = self.receiver.file
        self.assertEqual(self.finish(), self.data)
        self.assertTrue(file.closed)
        self.assertIsNone(self.receiver.file)

    def test_order_identity_length_and_duplicates(self):
        self.begin()
        for index, data, request in [(1, b'y', 'req'), (0, b'x', 'req'),
                                     (0, self.data[:CHUNK_BYTES], 'other')]:
            with self.assertRaises(ValueError):
                self.chunk(index, data, request)
        self.chunk(0, self.data[:CHUNK_BYTES])
        with self.assertRaises(ValueError):
            self.chunk(0, b'z' * CHUNK_BYTES)
        with self.assertRaises(ValueError):
            self.finish()
        self.chunk(1, b'y')
        self.assertEqual(self.finish(), self.data)

    def test_whole_job_hash_and_cleanup(self):
        self.begin()
        self.chunk(0, b'z' * CHUNK_BYTES)
        self.chunk(1, b'y')
        with self.assertRaises(ValueError):
            self.finish()
        self.assertIsNone(self.receiver.file)

    def test_replacement_and_expiry(self):
        self.begin()
        old = self.receiver.file
        self.receiver.accept(self.frame('audit_begin', self.manifest, 'new'))
        self.assertTrue(old.closed)
        self.receiver.updated -= 601
        with self.assertRaises(ValueError):
            self.chunk(0, self.data[:CHUNK_BYTES], 'new')
        self.assertIsNone(self.receiver.file)
