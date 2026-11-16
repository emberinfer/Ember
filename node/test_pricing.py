"""Tokenization must precede a hold and agree with independently audited work."""
import hashlib
import json
import queue
import unittest

from agent import Agent
from auditor import audit
from ember_core import sign, verify
from ember_pb2 import ExecutionCommitment, ExecutionReceipt, TunnelFrame
from engine import MockEngine


class TokenEngine(MockEngine):
    def count_input_tokens(self, prompt):
        # Deliberately differs from whitespace splitting.
        return 7


class PricingTests(unittest.TestCase):
    def setUp(self):
        self.agent = Agent('producer', TokenEngine())
        self.job = dict(prompt='two words', seed=1, max_tokens=2,
                        chain='ember-priced', profile='test', pricing=True)

    def frames(self, kind):
        outgoing = queue.Queue()
        frame = TunnelFrame(kind=kind, request_id='request', attempt_id='attempt',
                            payload=json.dumps(self.job).encode())
        self.agent._run(frame, outgoing)
        return list(outgoing.queue)

    def test_signed_tokenization_precedes_generation_and_binds_receipt(self):
        frames = self.frames('tokenize')
        self.assertEqual([f.kind for f in frames], ['tokenization'])
        self.assertEqual(self.agent.completed_requests, 0)
        c = ExecutionCommitment.FromString(frames[0].payload)
        verify(c)
        self.assertEqual((c.input_tokens, c.max_output_tokens), (7, 2))
        self.assertEqual(c.input_commitment, hashlib.sha256(b'two words').digest())
        frames = self.frames('run')
        r = ExecutionReceipt.FromString(frames[-1].payload)
        verify(r)
        self.assertEqual((r.input_tokens, r.output_tokens), (7, 2))
        self.assertEqual(r.commitment_ref, hashlib.sha256(c.SerializeToString(
            deterministic=True)).digest())

    def test_legacy_commitment_omits_new_fields(self):
        self.job.pop('pricing')
        frames = self.frames('run')
        r = ExecutionReceipt.FromString(frames[-1].payload)
        self.assertEqual(r.input_tokens, 2)
        c = self.agent.sessions['request'][2]
        self.assertEqual((c.input_tokens, c.max_output_tokens), (0, 0))
        legacy = ExecutionCommitment(request_id=c.request_id, chain_id=c.chain_id,
            input_commitment=c.input_commitment, profile_ver=c.profile_ver,
            sampling_params_hash=c.sampling_params_hash, seed=c.seed)
        sign(legacy, self.agent.key)
        self.assertEqual(c.SerializeToString(deterministic=True),
                         legacy.SerializeToString(deterministic=True))

    def test_reference_rejects_fabricated_input_count(self):
        r = ExecutionReceipt(request_id='request', input_tokens=8)
        sign(r, self.agent.key)
        result = audit('two words', 1, r, [], self.agent.engine, .001,
                       verify_input_tokens=True)
        self.assertEqual((result[0], result[2]), ('reject', 'input token count mismatch'))
