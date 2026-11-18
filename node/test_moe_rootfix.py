"""Regression coverage for request isolation and signed evidence bindings."""
import sys
import unittest
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor

sys.path[:0] = [str(Path(__file__).resolve().parents[1] / p)
                for p in ('proto', 'core-py', 'node')]

import torch
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import sign
from ember_pb2 import ChainGenesis, ModelManifest, PipelineExecutionReceipt
from engine.moe import AttentionStage, ExpertUnit, MoeWeights
from moe_rpc import (FrameContext, ReceiptSigner, verify_frame_segments,
                     verify_bound_signature)
from moe_client import verify_execution_bindings
from test_moe_engine import tiny_model_dir


class BindingTests(unittest.TestCase):
    def setUp(self):
        self.key = Ed25519PrivateKey.generate()
        self.signer = ReceiptSigner(self.key, ChainGenesis(chain_id='test'), ModelManifest())
        self.segment = self.signer.segment(request_id='a', frame=0, kind='attention')

    def test_signature_error_differs_from_foreign_binding(self):
        seal = self.signer.frame('a', 0, [])
        seal.request_id = 'b'
        with self.assertRaisesRegex(ValueError, '^signature invalid$'):
            verify_frame_segments(seal, 'a', 0, self.signer.node, [])
        sign(seal, self.key)
        with self.assertRaisesRegex(ValueError, 'signature valid.*seal request_id'):
            verify_frame_segments(seal, 'a', 0, self.signer.node, [])

    def test_seal_accepts_segment_set_and_rejects_missing_segments(self):
        from ember_core import moe as core
        second = self.signer.segment(request_id='a', frame=0, kind='attention', layer=1)
        hashes = [core.segment_receipt_hash(s) for s in (self.segment, second)]
        seal = self.signer.frame('a', 0, hashes)
        verify_frame_segments(seal, 'a', 0, self.signer.node, [self.segment, second])
        # Protocol frame_root sorts its leaves; receipt order is not a binding.
        verify_frame_segments(seal, 'a', 0, self.signer.node, [second, self.segment])
        with self.assertRaisesRegex(ValueError, 'seal segment'):
            verify_frame_segments(seal, 'a', 0, self.signer.node, [self.segment])

    def test_valid_signature_cannot_bind_foreign_request(self):
        receipt = PipelineExecutionReceipt(request_id='b')
        receipt.moe.segment_receipts.append(self.segment)
        with self.assertRaisesRegex(ValueError, 'segment request_id'):
            verify_execution_bindings(receipt)


class IsolationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.directory, _ = tiny_model_dir()
        cls.weights = MoeWeights(cls.directory)
        from ember_pb2 import MoeLayout
        cls.layout = MoeLayout(num_layers=2)

    def stage(self):
        return AttentionStage(self.weights, self.layout, 0, 2)

    def callback(self, context, layer, expert, rows):
        self.assertEqual(context.routing['layer'], layer)
        self.assertIn(layer, context.a_att)
        return ExpertUnit(self.weights, layer, expert).forward(rows)

    def test_concurrent_requests_equal_serial_and_returns_survive_next_frame(self):
        def run(stage, rid, ids):
            stage.expert_call = self.callback
            return stage.forward_frame(rid, 0, input_ids=ids,
                                       frame_context=FrameContext(rid, 0))
        stage = self.stage()
        inputs = [('a', [1, 2, 3]), ('b', [5])]
        with ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(run, stage, *args) for args in inputs]
            actual = [f.result(timeout=30) for f in futures]
        for args, got in zip(inputs, actual):
            baseline = run(self.stage(), *args)
            self.assertTrue(torch.equal(got[0], baseline[0]))
            self.assertEqual(got[1], baseline[1])
            for layer in got[2]:
                self.assertTrue(torch.equal(got[2][layer], baseline[2][layer]))
        old = {k: v.clone() for k, v in actual[0][2].items()}
        stage.forward_frame('a', 1, input_ids=[4], frame_context=FrameContext('a', 1))
        for layer in old:
            self.assertTrue(torch.equal(actual[0][2][layer], old[layer]))

    def test_mid_frame_exception_restores_previous_kv(self):
        stage = self.stage()
        stage.expert_call = self.callback
        stage.forward_frame('a', 0, input_ids=[1, 2], frame_context=FrameContext('a', 0))
        before = dict(stage._kv['a'])
        def fail(*args):
            raise RuntimeError('injected')
        stage.expert_call = fail
        with self.assertRaisesRegex(RuntimeError, 'injected'):
            stage.forward_frame('a', 1, input_ids=[3], frame_context=FrameContext('a', 1))
        self.assertEqual(stage._kv['a']['pos'], before['pos'])
        for layer in (0, 1):
            self.assertIs(stage._kv['a'][layer], before[layer])


if __name__ == '__main__':
    torch.set_num_threads(1)
    unittest.main()
