"""Attempt-independent, disk-backed audit reassembly on the existing tunnel.

EvidenceChunk supplies the sequence number and whole-job identity. Signed
TunnelFrames authenticate every operation; acknowledgements hash each piece,
and commit verifies both length and the complete SHA-256 before JSON parsing.
"""
import hashlib
import json
import tempfile
import time

from ember_pb2 import EvidenceChunk

CHUNK_BYTES = 1 << 20


class AuditTransfer:
    def __init__(self):
        self.file = None
        self.manifest = None
        self.request = None
        self.hashes = []
        self.updated = 0

    def close(self):
        if self.file is not None:
            self.file.close()
        self.file = None
        self.manifest = None
        self.request = None
        self.hashes = []

    @staticmethod
    def _manifest(raw):
        m = json.loads(raw)
        if (m.get('version') != 1 or type(m.get('size')) is not int
                or m['size'] <= CHUNK_BYTES
                or m.get('chunks') != (m['size'] + CHUNK_BYTES - 1) // CHUNK_BYTES
                or len(m.get('sha256', '')) != 64
                or len(bytes.fromhex(m['sha256'])) != 32):
            raise ValueError('invalid audit transfer manifest')
        return m

    def accept(self, frame):
        now = time.monotonic()
        if self.file is not None and now - self.updated > 600:
            self.close()
        if frame.kind == 'audit_begin':
            m = self._manifest(frame.payload)
            if self.manifest != m or self.request != frame.request_id:
                self.close()
                # Anonymous mode-0600 temporary storage; respects TMPDIR. One
                # active transfer per auditor, matching gateway busy admission.
                self.file = tempfile.TemporaryFile(prefix='ember-audit-')
                self.manifest, self.request = m, frame.request_id
            self.updated = now
            return bytes.fromhex(m['sha256'])
        if self.file is None or frame.request_id != self.request:
            raise ValueError('audit transfer not begun')
        chunk = EvidenceChunk.FromString(frame.payload)
        m = self.manifest
        if chunk.manifest_root.hex() != m['sha256'] or chunk.index >= m['chunks']:
            raise ValueError('audit chunk identity or index mismatch')
        expected = min(CHUNK_BYTES, m['size'] - chunk.index * CHUNK_BYTES)
        if len(chunk.data) != expected or chunk.index > len(self.hashes):
            raise ValueError('audit chunk length or sequence mismatch')
        digest = hashlib.sha256(chunk.data).digest()
        if chunk.index < len(self.hashes):
            if self.hashes[chunk.index] != digest:
                raise ValueError('conflicting duplicate audit chunk')
        else:
            self.file.write(chunk.data)
            self.hashes.append(digest)
        self.updated = now
        return digest

    def finish(self, frame):
        m = self._manifest(frame.payload)
        if (self.file is None or m != self.manifest or self.request != frame.request_id
                or len(self.hashes) != m['chunks']):
            raise ValueError('incomplete audit transfer')
        try:
            self.file.seek(0)
            # The existing replay API consumes a whole JSON job. Transport is
            # bounded per frame; replay still needs memory proportional to job.
            raw = self.file.read()
            if len(raw) != m['size'] or hashlib.sha256(raw).hexdigest() != m['sha256']:
                raise ValueError('audit transfer integrity mismatch')
            return raw
        finally:
            self.close()
