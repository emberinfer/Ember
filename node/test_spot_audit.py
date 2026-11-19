"""No model files: use the existing shard forward with tiny synthetic modules."""
import base64
import copy
import hashlib
import json
from pathlib import Path
import struct
import tempfile
from types import SimpleNamespace as NS
import unittest
from unittest.mock import patch

import numpy as np
import torch
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
from ember_core import verify
from engine.shard import Qwen2ShardEngine
from auditor.spot_binding import leaf_hash, tensor_hash, descriptor
from auditor.spot import (DirectorySpool, audit, audit_step, canonical, deadlines,
                          durable_json, reproduce_evidence, signed, unopened_claim)
from auditor.spot_worker import envelope_result, execute, unopened_action
from auditor.spot_wire import SpotVerdict


def sha(value):
    return hashlib.sha256(value).digest()


def tree(leaves):
    rows = [[leaf_hash(leaf) for leaf in leaves]]
    while len(rows[-1]) > 1:
        row = rows[-1]
        rows.append([sha(b'\1' + row[i] + row[min(i+1, len(row)-1)]) for i in range(0, len(row), 2)])
    return rows


def proof(rows, index):
    result = []
    for row in rows[:-1]:
        result.append(dict(hash=row[min(index ^ 1, len(row)-1)].hex(), left=bool(index & 1)))
        index //= 2
    return result


class Double(torch.nn.Module):
    def forward(self, value, **kwargs):
        return value * 2


def reference_engine():
    shard = Qwen2ShardEngine.__new__(Qwen2ShardEngine)
    shard.torch, shard.device, shard.dtype = torch, 'cpu', torch.bfloat16
    shard.start_layer, shard.end_layer = 0, 1
    shard.has_embedding = shard.has_lm_head = True
    shard.embedding = torch.nn.Embedding(4, 2, dtype=torch.bfloat16)
    shard.embedding.weight.data[:] = torch.tensor([[0, 0], [1, 2], [2, 1], [1, 1]])
    shard.layers = [Double()]
    shard.rotary = lambda *args: None
    shard.norm = torch.nn.Identity()
    shard.lm_head = torch.nn.Linear(2, 4, bias=False, dtype=torch.bfloat16)
    shard.lm_head.weight.data[:] = torch.tensor([[0, 0], [1, 0], [0, 1], [-1, -1]])
    shard.check_memory, shard.rss_bytes, shard.memory_budget = lambda *a: None, lambda: 0, (0, 0)
    return shard


class Fixture:
    def __init__(self, root, *, mismatch=False, broken_boundary=False):
        self.root = Path(root)
        self.key = Ed25519PrivateKey.generate()
        self.engine = reference_engine()
        self.rid = 'ab' * 32
        self.spool = self.root / 'spool'
        self.spool.mkdir()
        values = [(np.array([1], dtype='<i8'), 'i64'),
                  (np.array([[[1, 2]]], dtype='<f4'), 'f32'),
                  (np.array([[[2, 4]]], dtype='<f4'), 'f32'),
                  (np.array([2], dtype='<i8'), 'i64')]
        if mismatch:
            values[2][0][0, 0, 0] = 3
        self.tensors = []
        for i, (value, dtype) in enumerate(values):
            path = self.spool / f'tensor-{i}.raw'
            value.tofile(path)
            self.tensors.append(dict(path=str(path), dtype=dtype, shape=list(value.shape), encoding='raw-le-v1'))
        self.leaves = []
        for i in range(3):
            incoming, outgoing = self.tensors[i:i+2]
            self.leaves.append(dict(step_index=i+1, input_hash=tensor_hash(incoming['path'], incoming),
                output_hash=tensor_hash(outgoing['path'], outgoing),
                format_hash=sha(descriptor(outgoing['dtype'], outgoing['shape']).encode()).hex(),
                profile_id_hash=sha(b'tiny').hex(), request_id=self.rid))
        if broken_boundary:
            self.leaves[2]['input_hash'] = '11' * 32
        self.rows = tree(self.leaves)
        self.packages = []
        for i in range(3):
            neighbor = i+1 if i < 2 else i-1
            self.packages.append(dict(step_index=i+1, leaf=self.leaves[i], proof=proof(self.rows, i),
                neighbor_leaf=self.leaves[neighbor], neighbor_proof=proof(self.rows, neighbor),
                tensors=dict(input=self.tensors[i], output=self.tensors[i+1])))
        ops = {}
        for i, (start, end, emb, head) in enumerate(((0, 0, True, False), (0, 1, False, False), (1, 1, False, True))):
            ops[str(i+1)] = dict(input={k: self.tensors[i][k] for k in ('dtype', 'shape')},
                output={k: self.tensors[i+1][k] for k in ('dtype', 'shape')},
                operator=dict(start_layer=start, end_layer=end, has_embedding=emb, has_lm_head=head))
        ops['3']['output_kind'] = 'tokens'
        self.context = dict(request_id=self.rid, profile_id='tiny', root=self.rows[-1][0].hex(), total_steps=3,
            input_anchor=self.leaves[0]['input_hash'], output_anchor=self.leaves[-1]['output_hash'],
            claimed_tokens=[2], epsilon=.01, operations=ops, steps=[2], round='1',
            challenge_height=10, open_deadline=130, verdict_deadline=190, challenge_record={'height': 10})
        self.reader = DirectorySpool(self.spool)

    def deliver(self, steps, height=10, name='opening'):
        durable_json(self.spool / (name + '.json'), dict(kind='opening', request_id=self.rid,
                     challenge_height=height, packages=[self.packages[i-1] for i in steps], delivered=False))

    def run(self, mode='normal'):
        return audit(self.context, self.reader, self.engine, self.key, self.root / 'evidence', mode=mode)


class SpotAuditTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.f = Fixture(self.tmp.name)

    def test_partial_match_and_uses_existing_forward_only_once(self):
        self.f.deliver([2])
        with patch.object(self.f.engine, 'forward', wraps=self.f.engine.forward) as forward:
            result = self.f.run()
        self.assertEqual(result['verdict_kind'], 'accept')
        self.assertEqual(forward.call_count, 1)
        self.assertFalse(forward.call_args.kwargs['use_cache'])

    def test_tensor_tamper_fails_before_replay(self):
        Path(self.f.tensors[2]['path']).write_bytes(np.array([[[3, 4]]], dtype='<f4').tobytes())
        with patch.object(self.f.engine, 'forward') as replay:
            result = audit_step(self.f.packages[1], self.f.context, self.f.engine)
        self.assertEqual(result['outcome'], 'binding_fail')
        replay.assert_not_called()

    def test_wrong_merkle_proof_fails_before_replay(self):
        self.f.packages[1]['proof'][0]['hash'] = '00' * 32
        with patch.object(self.f.engine, 'forward') as replay:
            result = audit_step(self.f.packages[1], self.f.context, self.f.engine)
        self.assertEqual(result['outcome'], 'binding_fail')
        replay.assert_not_called()

    def test_broken_boundary_with_valid_proofs(self):
        with tempfile.TemporaryDirectory() as root:
            f = Fixture(root, broken_boundary=True)
            self.assertEqual(audit_step(f.packages[1], f.context, f.engine)['outcome'], 'binding_fail')

    def test_mismatch_self_sufficient_signed_evidence(self):
        with tempfile.TemporaryDirectory() as root:
            f = Fixture(root, mismatch=True)
            f.deliver([2])
            result = f.run()
            step = result['per_step'][0]
            self.assertEqual(step['outcome'], 'mismatch')
            self.assertEqual(result['verdict_kind'], 'reject_step')
            self.assertTrue(result['escalation_required'])
            import shutil
            shutil.rmtree(f.spool)
            reproduced = reproduce_evidence(step['evidence']['path'], reference_engine())
            self.assertEqual(reproduced['outcome'], step['outcome'])
            self.assertEqual(reproduced['first_divergence_distance'], step['first_divergence_distance'])

    def test_evidence_signature_tamper_rejected(self):
        self.f.context['operations']['2']['output']['dtype'] = 'f64'
        self.f.deliver([2])
        evidence = Path(self.f.run()['per_step'][0]['evidence']['path'])
        doc = json.loads(evidence.read_text())
        doc['payload']['context']['epsilon'] = 100
        evidence.write_text(json.dumps(doc))
        with self.assertRaises(Exception):
            reproduce_evidence(evidence, self.f.engine)

    def test_profile_dtype_shape_and_unparseable_payload(self):
        for mutation in ('dtype', 'shape', 'truncated'):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as root:
                f = Fixture(root)
                if mutation == 'truncated':
                    Path(f.tensors[2]['path']).write_bytes(b'x')
                else:
                    f.context['operations']['2']['output'][mutation] = 'f64' if mutation == 'dtype' else [8]
                with patch.object(f.engine, 'forward') as replay:
                    result = audit_step(f.packages[1], f.context, f.engine)
                self.assertEqual(result['outcome'], 'format_fail')
                replay.assert_not_called()

    def test_terminal_tokens_are_exact_even_with_huge_epsilon(self):
        self.f.context['epsilon'] = 100
        self.f.context['claimed_tokens'] = [1]
        self.assertEqual(audit_step(self.f.packages[2], self.f.context, self.f.engine)['outcome'], 'mismatch')

    def test_first_and_last_anchor_checks(self):
        for step, name in ((1, 'input_anchor'), (3, 'output_anchor')):
            context = dict(self.f.context, **{name: 'ff' * 32})
            self.assertEqual(audit_step(self.f.packages[step-1], context, self.f.engine)['outcome'], 'binding_fail')

    def test_substitute_ignores_previous_auditor_progress(self):
        self.f.deliver([2])
        durable_json(self.f.spool / 'progress.json', dict(auditor='disappeared', completed=[2], verdict='reject'))
        self.assertEqual(self.f.run('substitute')['verdict_kind'], 'accept')

    def test_escalation_reuses_spool_and_requires_supplement(self):
        self.f.deliver([2])
        with self.assertRaisesRegex(ValueError, 'step 1'):
            self.f.run('escalation')
        self.f.deliver([1, 3], height=20, name='supplement')
        self.f.context.update(challenge_height=20, history_heights=[10])
        with patch.object(self.f.reader, 'get', wraps=self.f.reader.get) as get:
            self.assertEqual(self.f.run('escalation')['verdict_kind'], 'escalation_full_accept')
        self.assertEqual([c.args[1] for c in get.call_args_list], [1, 2, 3])

    def test_escalation_reject(self):
        with tempfile.TemporaryDirectory() as root:
            f = Fixture(root, mismatch=True)
            f.deliver([1, 2, 3])
            self.assertEqual(f.run('escalation')['verdict_kind'], 'escalation_full_reject')

    def test_unopened_requires_gateway_attestation_and_finalized_deadline(self):
        gateway = Ed25519PrivateKey.generate()
        public = gateway.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
        with self.assertRaisesRegex(ValueError, 'deadline'):
            unopened_claim(self.f.context, self.f.reader, self.f.key, public, 130)
        with self.assertRaisesRegex(ValueError, 'attestation required'):
            unopened_claim(self.f.context, self.f.reader, self.f.key, public, 131)
        value = dict(kind='absence', request_id=self.f.rid, challenge_height=10, steps=[2], deadline=130, height=131)
        durable_json(self.f.spool / 'absence.json', signed(value, gateway, b'EMBER-SPOOL-V1\0'))
        claim = unopened_claim(self.f.context, self.f.reader, self.f.key, public, 131)
        self.assertEqual(claim['verdict_kind'], 'reject_unopened')
        self.f.deliver([2])
        with self.assertRaisesRegex(ValueError, 'contains opening'):
            unopened_claim(self.f.context, self.f.reader, self.f.key, public, 131)

    def test_round_two_claim_binds_new_challenge(self):
        gateway = Ed25519PrivateKey.generate()
        public = gateway.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
        self.f.context.update(round='2', challenge_height=150, open_deadline=270)
        value = dict(kind='absence', request_id=self.f.rid, challenge_height=150, steps=[2], deadline=270, height=271)
        durable_json(self.f.spool / 'absence.json', signed(value, gateway, b'EMBER-SPOOL-V1\0'))
        self.assertEqual(unopened_claim(self.f.context, self.f.reader, self.f.key, public, 271)['auditor_statement']['payload']['round'], '2')
        with self.assertRaises(Exception):
            unopened_claim(self.f.context, self.f.reader, self.f.key, bytes(32), 271)

    def test_deadline_warning_and_expiry(self):
        with self.assertLogs('auditor.spot', level='WARNING'):
            deadlines(self.f.context, 185)
        with self.assertRaisesRegex(ValueError, 'expired'):
            deadlines(self.f.context, 191)

    def test_gpu_reference_refused_without_cpu_fallback(self):
        self.f.engine.device = 'cuda'
        with self.assertRaisesRegex(ValueError, 'CPU reference'):
            audit_step(self.f.packages[1], self.f.context, self.f.engine)

    def test_nonce_refreshed_on_cached_resend(self):
        from auditor.worker import cache_verdict, resend_verdict
        agent = NS(key=self.f.key, chain_args=NS(ledger_chain_id='chain'))
        with patch('auditor.worker._fresh_audit_nonce', return_value=5):
            payload = envelope_result(agent, dict(type='audit', hold_id=self.f.rid), {'verdict_kind': 'accept'})
        with patch('auditor.worker._fresh_audit_nonce', return_value=19):
            resent = json.loads(resend_verdict(agent, cache_verdict(payload)))
        self.assertEqual(json.loads(base64.b64decode(resent['envelope']))['nonce'], 20)

    def test_per_step_engine_lock_is_released_before_chain_or_spool_io(self):
        import threading
        from agent.audit_engine import AuditEngine
        agent = NS(engine=self.f.engine, execution_lock=threading.Lock())
        view = AuditEngine(agent, self.f.rid)
        original = self.f.engine.forward
        def forward(**kwargs):
            self.assertTrue(agent.execution_lock.locked())
            return original(**kwargs)
        with patch.object(self.f.engine, 'forward', side_effect=forward):
            self.assertEqual(audit_step(self.f.packages[1], self.f.context, view)['outcome'], 'match')
        self.assertFalse(agent.execution_lock.locked())
        with patch.object(self.f.engine, 'forward', side_effect=RuntimeError('service failure')):
            with self.assertRaisesRegex(RuntimeError, 'service failure'):
                audit_step(self.f.packages[1], self.f.context, view)
        self.assertFalse(agent.execution_lock.locked())

    def test_tensor_hash_reads_only_bounded_blocks(self):
        from auditor.spot_binding import BLOCK, tensor_header
        path = self.f.spool / 'large.raw'
        with path.open('wb') as stream:
            for _ in range(3):
                stream.write(bytes(BLOCK))
        fmt = dict(dtype='i32', shape=[3 * BLOCK // 4], encoding='raw-le-v1')
        original = Path.open
        reads = []
        class Guard:
            def __init__(self, stream):
                self.stream = stream
            def __enter__(self):
                return self
            def __exit__(self, *args):
                self.stream.close()
            def read(self, size):
                self_outer.assertLessEqual(size, BLOCK)
                self_outer.assertGreater(size, 0)
                reads.append(size)
                return self.stream.read(size)
        self_outer = self
        def guarded(path, *args, **kwargs):
            return Guard(original(path, *args, **kwargs))
        with patch.object(Path, 'open', guarded):
            value = tensor_hash(path, fmt)
        expected = hashlib.sha256(tensor_header('i32', fmt['shape']))
        for _ in range(3):
            expected.update(bytes(BLOCK))
        self.assertEqual(value, expected.hexdigest())
        self.assertEqual(len(reads), 4)

    def test_verdict_is_not_returned_if_last_step_crosses_deadline(self):
        self.f.deliver([2])
        heights = iter([180, 191])
        with self.assertRaisesRegex(ValueError, 'expired'):
            audit(self.f.context, self.f.reader, self.f.engine, self.f.key,
                  self.f.root / 'evidence', height=lambda: next(heights))

    def test_checked_in_golden_vector(self):
        from auditor.spot_binding import verify_proof
        path = Path(__file__).parent / 'auditor/testdata/spot-v1-golden.json'
        vector = json.loads(path.read_text())
        self.assertEqual(vector['root'], '2710c505caa99d07321382067483c56a638a9c97a35fea9806b30af558c98f8f')
        self.assertEqual(self.f.context['root'], vector['root'])
        for i, leaf in enumerate(vector['leaves']):
            verify_proof(leaf, vector['proofs'][i], vector['root'], 3)
        proof_bad = copy.deepcopy(vector['proofs'][2])
        proof_bad[0]['hash'] = 'ff' * 32
        with self.assertRaises(ValueError):
            verify_proof(vector['leaves'][2], proof_bad, vector['root'], 3)

    def test_first_divergence_uses_existing_epsilon_distance(self):
        from auditor.spot_replay import compare_output
        from auditor.numeric import relative_distance
        path = self.f.spool / 'compare.raw'
        np.array([[1.001, 2], [3, 4], [8, 9]], dtype='<f4').tofile(path)
        opening = dict(tensors=dict(output=dict(path=str(path), dtype='f32', shape=[3, 2])))
        result = compare_output(torch.tensor([[1., 2], [2, 4], [2, 4]]), opening, .01)
        self.assertEqual(result[0], 'mismatch')
        self.assertEqual(result[2], 1)
        self.assertEqual(result[1], relative_distance([3, 4], [2, 4]))
        np.array([[1.001, 2]], dtype='<f4').tofile(path)
        opening['tensors']['output']['shape'] = [1, 2]
        self.assertEqual(compare_output(torch.tensor([[1., 2]]), opening, .01)[0], 'match')

    def test_registered_worker_builds_signed_contract_verdict(self):
        from auditor.worker import execute as worker_execute
        self.f.deliver([2])
        context_dir = Path(self.tmp.name) / 'contexts'
        durable_json(context_dir / (self.f.rid + '.json'), self.f.context)
        challenge = dict(request_id=self.f.rid, auditor='auditor', step_set=[2], round='1',
                         challenge_height=10, deadline=130, verdict_deadline=190)
        hold = dict(auditor='auditor', state='dispute', receipt_ref='cd' * 32, audit_proof='ef' * 32,
                    pipeline=dict(spot=challenge, total_layers=1,
                                  commitment_root=base64.b64encode(bytes.fromhex(self.f.context['root'])).decode()))
        agent = NS(node_id='auditor', key=self.f.key, genesis_bytes=b'genesis', engine=self.f.engine,
                   chain_args=NS(no_audit=False, chain_rpc='rpc', chain_cli='cli', ledger_chain_id='chain',
                                 state_dir=self.tmp.name, spot_context_dir=str(context_dir), spot_spool_dir=str(self.f.spool)))
        def query(cli, rpc, path):
            return hold if path.startswith('/hold/') else ({'nonce': 8} if path.startswith('/account/') else
                    {'audit_equality': {'enabled': True, 'epsilon': .01}} if path == '/parameters' else {'height': 11})
        with patch('agent.join._chain_json', side_effect=query):
            result = json.loads(worker_execute(agent, NS(request_id=self.f.rid), dict(kind='spotcheck', genesis=b'genesis'.hex())))
        envelope = json.loads(base64.b64decode(result['envelope']))
        verdict = SpotVerdict.FromString(base64.b64decode(envelope['action']['verdict']))
        verify(verdict)
        self.assertEqual(verdict.per_step[0].outcome, 'match')
        self.assertEqual(verdict.verdict_kind, 'accept')
        self.assertEqual(envelope['nonce'], 9)
        self.assertTrue((Path(self.tmp.name) / 'spot-evidence' / self.f.rid / 'verdict.json').exists())
        bad = copy.deepcopy(hold)
        bad['auditor'] = 'another'
        with patch('agent.join._chain_json', return_value=bad), self.assertRaisesRegex(ValueError, 'assigned auditor'):
            worker_execute(agent, NS(request_id=self.f.rid), dict(kind='spotcheck', genesis=b'genesis'.hex()))

    def test_chain_unopened_action_requires_second_gateway_signature(self):
        from ember_core import node_id
        gateway = Ed25519PrivateKey.generate()
        public = gateway.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
        value = dict(kind='absence', request_id=self.f.rid, challenge_height=10, steps=[2], deadline=130, height=131)
        durable_json(self.f.spool / 'absence.json', signed(value, gateway, b'EMBER-SPOOL-V1\0'))
        chain = dict(request_id=self.f.rid, challenge_height=10,
                     step_set_hash=base64.b64encode(sha(struct.pack('>Q', 2))).decode(),
                     gateway=node_id(public), received=False, height=131)
        chain['signature'] = base64.b64encode(gateway.sign(b'EMBER-SPOT-SPOOL-V1\0' + json.dumps(chain, separators=(',', ':')).encode())).decode()
        agent = NS(key=self.f.key, chain_args=NS(ledger_chain_id='chain', state_dir=self.tmp.name))
        with patch('auditor.worker._fresh_audit_nonce', return_value=8):
            result = json.loads(unopened_action(agent, self.f.context, self.f.reader, public, chain, 131))
        action = json.loads(base64.b64decode(result['envelope']))['action']
        self.assertEqual(action['type'], 'spot_unopened')
        self.assertEqual(list(action), ['spool', 'type', 'hold_id'])
        chain['signature'] = base64.b64encode(bytes(64)).decode()
        with self.assertRaises(Exception):
            unopened_action(agent, self.f.context, self.f.reader, public, chain, 131)

    def test_private_v14_schema_signatures_roundtrip(self):
        from ember_core import sign
        result = SpotVerdict(verdict='reject', verdict_distance=.2, verdict_kind='reject_step')
        result.per_step.add(step=2, outcome='mismatch', first_divergence_distance=.2)
        sign(result, self.f.key)
        verify(SpotVerdict.FromString(result.SerializeToString(deterministic=True)))


if __name__ == '__main__':
    unittest.main()
