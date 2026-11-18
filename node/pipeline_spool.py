"""Disk-backed job evidence; preserve the single JSON-line executor protocol."""
from contextlib import ExitStack
import hashlib
import json
import tempfile

from ember_pb2 import PipelineExecutionReceipt


class JobSpool:
    # TemporaryFile is unlinked on POSIX, including when the executor is killed.
    # Files live on the working filesystem, never the RAM-backed default /tmp.
    def __init__(self):
        self.files = ExitStack()
        self.arrays = {}
        self.counts = {}
        try:
            for name in ('invocations', 'openings', 'failovers', 'receipts', 'witnesses'):
                self.arrays[name] = self.files.enter_context(
                    tempfile.TemporaryFile(mode='w+', encoding='ascii', dir='.'))
                self.counts[name] = 0
        except BaseException:
            self.files.close()
            raise
        self.commitment = hashlib.sha256(b'[')

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return self.files.__exit__(*exc)

    def write(self, name, part):
        try:
            self.arrays[name].write(part)
        except OSError as exc:
            # A disk failure is fatal, not a retryable stage transport error.
            raise RuntimeError('pipeline evidence spool write failed') from exc

    def append(self, name, value):
        if self.counts[name]:
            self.write(name, ',')
        for part in json.JSONEncoder(separators=(',', ':')).iterencode(value):
            self.write(name, part)
        self.counts[name] += 1

    def step(self, encoded, opened):
        from evidence import canonical
        if self.counts['openings']:
            self.commitment.update(b',')
        self.commitment.update(canonical(encoded))
        self.append('openings', opened)

    def receipt(self, value):
        wrapper = PipelineExecutionReceipt()
        wrapper.stage_receipts.add().CopyFrom(value)
        self.write('receipts', wrapper.SerializeToString(deterministic=True).hex())

    def witness(self, value):
        wrapper = PipelineExecutionReceipt()
        wrapper.latency_witnesses.add().CopyFrom(value)
        self.write('witnesses', wrapper.SerializeToString(deterministic=True).hex())

    def root(self):
        result = self.commitment.copy()
        result.update(b']')
        return result.digest()

    def copy(self, name, output):
        source = self.arrays[name]
        source.seek(0)
        while True:
            part = source.read(64 * 1024)
            if not part:
                break
            output.write(part)
            output.flush()

    def emit_result(self, receipt, output):
        # Deterministic protobuf order: request_id (1), stage_receipts (2),
        # scalar fields (3..6), latency_witnesses (7). Repeated entries were
        # serialized by protobuf itself, preserving every original wire byte.
        head = PipelineExecutionReceipt(request_id=receipt.request_id)
        tail = PipelineExecutionReceipt()
        tail.CopyFrom(receipt)
        tail.ClearField('request_id')
        output.write('{"type":"result","receipt":"')
        output.write(head.SerializeToString(deterministic=True).hex())
        self.copy('receipts', output)
        output.write(tail.SerializeToString(deterministic=True).hex())
        self.copy('witnesses', output)
        output.write('"')
        for name in ('openings', 'invocations', 'failovers'):
            output.write(',"' + name + '":[')
            self.copy(name, output)
            output.write(']')
        output.write('}\n')
        output.flush()
