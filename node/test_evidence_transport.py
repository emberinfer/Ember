"""F5 wire compatibility, chunk integrity and fail-closed worker coverage."""
import base64
import hashlib
import json
import os
import queue
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import digest, node_id, sign, verify
from ember_pb2 import ChainGenesis, ExecutionReceipt, TunnelFrame, EvidenceChunk, AuditVerdict
from agent import Agent
from engine import MockEngine
from evidence import canonical, manifest_chunks, fetch_openings, opening, MAX_FRAME_BYTES


class EvidenceTransportTest(unittest.TestCase):
    def frames(self, count, limit):
        key = Ed25519PrivateKey.generate()
        agent = Agent('producer', MockEngine(), key=key)
        outgoing = queue.Queue()
        job = dict(prompt='hello', seed=22, max_tokens=count, evidence_max_frame_tokens=limit)
        agent._run(TunnelFrame(kind='run', request_id='hold', attempt_id='attempt',
                              payload=json.dumps(job).encode()), outgoing)
        frames = list(outgoing.queue)
        self.assertEqual(frames[-1].kind, 'receipt', frames[-1])
        return agent, frames, ExecutionReceipt.FromString(frames[-1].payload)

    def test_single_frame_is_byte_compatible(self):
        agent, frames, receipt = self.frames(20, 512)
        self.assertFalse(receipt.HasField('evidence_manifest'))
        self.assertEqual([f.kind for f in frames if f.kind.startswith('evidence')], ['evidence'])
        expected = canonical([opening(s) for s in agent.sessions['hold'][1]])
        self.assertEqual(next(f.payload for f in frames if f.kind == 'evidence'), expected)
        verify(receipt)

    def test_multi_frame_roundtrip_and_go_merkle_vector(self):
        m, data = manifest_chunks([1, 2, 3, 4, 5], 2)
        self.assertEqual(m.merkle_root.hex(),
            'd8910d1608eb864fb08c37a2903e98f55970c11702948e10247be90e1553f81a')
        _, frames, receipt = self.frames(33, 7)
        chunks = [EvidenceChunk.FromString(f.payload) for f in frames if f.kind == 'evidence_chunk']
        self.assertEqual(len(chunks), 5)
        fetch = lambda root, index: chunks[index].data
        openings = fetch_openings(receipt, fetch, 7)
        self.assertEqual(len(openings), 33)
        chunks[1].data = b'[]'
        with self.assertRaisesRegex(ValueError, 'hash mismatch'):
            fetch_openings(receipt, fetch, 7)
        chunks.pop()
        with self.assertRaises((ValueError, IndexError)):
            fetch_openings(receipt, fetch, 7)

    def test_byte_bound_can_split_below_token_parameter(self):
        m, data = manifest_chunks(['x' * (MAX_FRAME_BYTES // 2)] * 3, 512)
        self.assertEqual(m.chunk_count, 3)
        self.assertTrue(all(len(chunk) <= MAX_FRAME_BYTES for chunk in data))

    def test_equality_missing_and_tampered_chunks(self):
        self.check_missing_and_tampered_chunks(True)

    def test_tf_default_disable_and_unsupported_worker_paths(self):
        from engine.spi import UnsupportedCapability
        for setting, unsupported, expected in ((None, False, 'teacher-forcing'),
                                               ('0', False, 'regeneration'),
                                               (None, True, 'regeneration')):
            env = {} if setting is None else {'EMBER_AUDIT_TEACHER_FORCING': setting}
            original = MockEngine.score_forced
            calls = []
            def score(engine, *args):
                calls.append('forced')
                if unsupported:
                    raise UnsupportedCapability('test adapter')
                return original(engine, *args)
            with patch.dict(os.environ, env, clear=True), \
                 patch.object(MockEngine, 'score_forced', score), \
                 self.assertLogs('auditor.worker', level='INFO') as logs:
                self.check_missing_and_tampered_chunks(True)
            self.assertEqual(bool(calls), setting != '0')
            self.assertTrue(any('path='+expected in line for line in logs.output))

    def test_missing_and_tampered_chunks_produce_signed_service_failure(self):
        self.check_missing_and_tampered_chunks(False)

    def check_missing_and_tampered_chunks(self, equality):
        from auditor.worker import execute
        _, frames, receipt = self.frames(33, 7)
        from ember_pb2 import ModelManifest
        key = Ed25519PrivateKey.generate()
        manifest = ModelManifest(chain_id='test')
        sign(manifest, key)
        genesis = ChainGenesis(chain_id='test', model_manifest_root=digest(manifest))
        sign(genesis, key)
        genesis_bytes = genesis.SerializeToString()
        auditor = Agent('temporary', MockEngine(), key=key)
        sign(receipt, key)
        auditor.node_id = node_id(receipt.signer)
        auditor.chain_args = SimpleNamespace(audit_role=True, chain_rpc='unused', chain_cli='unused',
                                            ledger_chain_id='test')
        auditor.genesis_bytes = genesis_bytes
        state = dict(models={'test': dict(signed_manifest=base64.b64encode(manifest.SerializeToString()).decode())},
            parameters=dict(evidence=dict(max_frame_tokens=7)),
            holds={'hold': dict(auditor=auditor.node_id, state='dispute', receipt_ref=digest(receipt).hex(),
                node='producer', producer_profile=base64.b64encode(b'profile').decode(), audit_proof='aa'*32,
                evidence_manifest=dict(merkle_root=base64.b64encode(receipt.evidence_manifest.merkle_root).decode()))},
            accounts={auditor.node_id:dict(nonce=0)})
        if equality:
            state['parameters']['audit_equality'] = dict(enabled=True, epsilon=1, max_rounds=3)
        job = dict(genesis=genesis_bytes.hex(), receipt=receipt.SerializeToString().hex(),
                   producer_profile=b'profile'.hex(), prompt='hello', seed=22)
        policy = SimpleNamespace(metric='test', layers=[], top_k=16)
        band = SimpleNamespace(mode='enforce', tolerance=.001)
        chunks = [EvidenceChunk.FromString(f.payload) for f in frames if f.kind == 'evidence_chunk']
        for failure in (('missing', 'tampered', 'success') if equality else ('missing', 'tampered')):
            def fetch(request, timeout):
                if failure == 'missing':
                    raise OSError('chunk missing')
                query = EvidenceChunk.FromString(request.payload)
                if failure == 'success':
                    return chunks[query.index]
                return EvidenceChunk(manifest_root=query.manifest_root, index=query.index, data=b'[]')
            auditor.evidence_stub = SimpleNamespace(FetchEvidence=fetch)
            with patch('agent.join._chain_json', return_value=state), \
                 patch('auditor.worker.load_job_policy', return_value=(policy, SimpleNamespace(actual_device='cuda'), 'bb'*32)), \
                 patch('auditor.worker.profile_engine_id', return_value='test'), \
                 patch('auditor.worker.select_band', return_value=band), \
                 patch('auditor.worker.audit') as replay:
                result = json.loads(execute(auditor, TunnelFrame(request_id='hold'), dict(job)),
                                    parse_constant=lambda value: self.fail('nonfinite wire number: '+value))
                envelope = json.loads(base64.b64decode(result['envelope']))
                verdict = AuditVerdict.FromString(base64.b64decode(envelope['action']['verdict']))
                verify(verdict)
                self.assertEqual(verdict.verdict, 'accept' if failure == 'success' else 'inconclusive')
                if failure == 'success':
                    from ember_pb2 import AuditChallenge
                    challenge = AuditChallenge.FromString(base64.b64decode(envelope['action']['challenge']))
                    verify(challenge)
                    self.assertEqual(list(challenge.positions), list(range(33)))
                    self.assertEqual(verdict.n_out_billed, 33)
                    self.assertEqual(list(verdict.distances), [0])
                self.assertEqual(verdict.evidence_ref, receipt.evidence_manifest.merkle_root)
                self.assertEqual(result['grade'], 'token-equality-v1' if failure == 'success' else 'evidence-unavailable')
                replay.assert_not_called()
                if equality:
                    from auditor.worker import load_job_policy, select_band
                    load_job_policy.assert_not_called()
                    select_band.assert_not_called()


if __name__ == '__main__':
    unittest.main()
