import hashlib
import io
import json
import tracemalloc
import unittest
from unittest.mock import patch

from ember_pb2 import PipelineExecutionReceipt, ShardStepReceipt, StageLatencyWitness
from evidence import canonical
from pipeline_spool import JobSpool


class PipelineSpoolTest(unittest.TestCase):
    def test_identical_frame_and_receipt_wire(self):
        expected = PipelineExecutionReceipt(request_id='job', input_tokens=8000,
            output_tokens=2, elapsed_ms=123)
        invocations, openings, failovers = [], [], []
        encoded = []
        with JobSpool() as spool:
            for i in range(2):
                receipt = ShardStepReceipt(request_id='job', step=i)
                witness = StageLatencyWitness(stage_receipt_ref=b'x'*32, started_mono_ns=i+1)
                expected.stage_receipts.add().CopyFrom(receipt)
                expected.latency_witnesses.add().CopyFrom(witness)
                spool.receipt(receipt)
                spool.witness(witness)
                invocation = dict(request='ab', response='cd', profile='ef', stage_ms=1.25)
                opened = dict(token=42, topk=[[42, 1.0]], activations={'a':'00'})
                item = dict(token=42, topk=[[42, 1.0]], activations={'a':hashlib.sha256(b'\0').hexdigest()})
                spool.append('invocations', invocation)
                spool.step(item, opened)
                invocations.append(invocation); openings.append(opened); encoded.append(item)
            failure = dict(step=0, stage=0, node_id='n', error='\u4e2d\nfail')
            spool.append('failovers', failure); failovers.append(failure)
            expected.output_commitment = hashlib.sha256(canonical(encoded)).digest()
            self.assertEqual(spool.root(), expected.output_commitment)
            base = PipelineExecutionReceipt(); base.CopyFrom(expected)
            base.ClearField('stage_receipts'); base.ClearField('latency_witnesses')
            output = io.StringIO(); spool.emit_result(base, output)
            self.assertEqual(output.getvalue(), json.dumps(dict(type='result',
                receipt=expected.SerializeToString(deterministic=True).hex(),
                openings=openings, invocations=invocations, failovers=failovers),
                separators=(',', ':'))+'\n')

    def test_disk_growth_without_resident_evidence_growth(self):
        # Each row is new, so retaining references would exceed 32 MiB.
        tracemalloc.start()
        try:
            with JobSpool() as spool:
                for i in range(128):
                    spool.append('invocations', {'request': str(i) + 'a'*(256*1024)})
                current, peak = tracemalloc.get_traced_memory()
                self.assertGreater(spool.arrays['invocations'].tell(), 32*1024*1024)
                self.assertLess(current, 1024*1024)
                self.assertLess(peak, 2*1024*1024)
                self.assertEqual(len(spool.arrays), 5)
                files = list(spool.arrays.values())
            self.assertTrue(all(f.closed for f in files))
        finally:
            tracemalloc.stop()

    def test_disk_failure_is_not_retryable_stage_error(self):
        with JobSpool() as spool:
            with patch.object(spool.arrays['invocations'], 'write',
                              side_effect=OSError('disk full')):
                with self.assertRaisesRegex(RuntimeError, 'spool write failed'):
                    spool.append('invocations', {'request': 'ab'})

    def test_exception_closes_spools(self):
        with self.assertRaisesRegex(ValueError, 'abort'):
            with JobSpool() as spool:
                files = list(spool.arrays.values())
                raise ValueError('abort')
        self.assertTrue(all(f.closed for f in files))
