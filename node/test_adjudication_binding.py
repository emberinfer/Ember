from test_support import targeted_query
import base64
import hashlib
import json
import math
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import patch

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import digest, sign, verify
from ember_pb2 import AuditVerdict, ExecutionReceipt, ChainGenesis, ToleranceBand, OffenseGrade, TunnelFrame, ModelManifest
from auditor.binding import recompute_ref, third_commitment, tolerance_identifier, audit_envelopes
from auditor.equality import compare, compare_detailed, compare_teacher_forced, compare_teacher_forced_detailed
from auditor import audit, audit_detailed
from auditor.numeric import compare as numeric_compare, LEGACY, UNION, RANK_PROB
from engine import Step
from evidence import canonical, encode_step

GOLDEN_RECOMPUTE = '88e96e597a68286610b962ace54195d153dd36be3486644fdd1417a330ab020d'
GOLDEN_THIRD = 'f0c07b43f9118a55a410d697d9a8bed5cca2e9f9e0e5e00a6f470340f8f45bf6'


def golden_verdict():
    return AuditVerdict(auditor_id='auditor-α', challenge_ref=bytes(range(32)),
                        evidence_ref=bytes(range(32, 64)), verdict='reject',
                        first_divergence=7, tolerance='token-equality-v1',
                        n_out_billed=16, distances=[0.0, 1.25, -0.0])


def golden_evidence():
    return dict(auditor='auditor-α', result_commitment='ab'*32,
                replay_record=GOLDEN_RECOMPUTE, first_divergence=7,
                tolerance='token-equality-v1', cause='token::token-equality-v1')


def decode_envelope(encoded):
    return json.loads(base64.b64decode(encoded))


class BindingTest(unittest.TestCase):
    def test_golden_vectors(self):
        v = golden_verdict()
        self.assertEqual(recompute_ref(v).hex(), GOLDEN_RECOMPUTE)
        self.assertEqual(third_commitment('reject', golden_evidence()), GOLDEN_THIRD)
        v.ClearField('first_divergence')
        self.assertNotEqual(recompute_ref(v).hex(), GOLDEN_RECOMPUTE)
        for invalid in (math.nan, math.inf, -math.inf):
            v.distances[:] = [invalid]
            with self.assertRaises(ValueError):
                recompute_ref(v)

    def test_tolerance_identifiers(self):
        for kind, band, expected in (('token', 'token-equality-v1', 'token-equality-v1'),
                                     ('numerical', 'band-hash', 'band-hash'),
                                     ('objective', '', 'exact')):
            g = OffenseGrade(**{'class': kind, 'band': band})
            self.assertEqual(tolerance_identifier(g), expected)
        band = ToleranceBand(mode='enforce', tolerance=.1)
        self.assertEqual(tolerance_identifier(band=band), hashlib.sha256(band.SerializeToString(deterministic=True)).hexdigest())
        self.assertEqual(tolerance_identifier(equality=True), 'token-equality-v1')

    def test_round_three_envelopes_match_go_golden(self):
        key = Ed25519PrivateKey.from_private_bytes(bytes(range(32)))
        agent = NS(node_id='auditor-α', key=key, chain_args=NS(ledger_chain_id='test'))
        v = golden_verdict()
        v.recompute_ref = recompute_ref(v)
        sign(v, key)
        hold = dict(audit_round=3, receipt_ref='ab'*32,
                    first_grade={'class': 'token', 'band': 'token-equality-v1'})
        action = dict(type='audit', hold_id='hold', verdict=base64.b64encode(v.SerializeToString()).decode())
        result = audit_envelopes(agent, hold, v, action, 40)
        fixture = Path(__file__).resolve().parents[1] / 'services/chain/testdata/adjudication_envelopes.json'
        self.assertEqual(result, json.loads(fixture.read_text()))
        pre, main = decode_envelope(result['pre_envelope']), decode_envelope(result['envelope'])
        self.assertEqual(pre['nonce'], 41)
        self.assertEqual(main['nonce'], 42)
        self.assertEqual(pre['action'], dict(type='commit_third_adjudication', hold_id='hold', adjudication_commitment=GOLDEN_THIRD))
        self.assertEqual(main['action'], action)
        for env in (pre, main):
            signature = base64.b64decode(env['signature'])
            env['signature'] = None
            key.public_key().verify(signature, b'ember/chain-tx/v1\x00' + json.dumps(env, separators=(',', ':'), ensure_ascii=False).encode())
        hold['audit_round'] = 2
        normal = audit_envelopes(agent, hold, v, action, 40)
        self.assertNotIn('pre_envelope', normal)
        self.assertEqual(decode_envelope(normal['envelope'])['nonce'], 41)

    def test_execute_round_three_bound_reject_and_honest_accept(self):
        from auditor.worker import execute
        key = Ed25519PrivateKey.from_private_bytes(bytes(range(32)))
        genesis = ChainGenesis(chain_id='test').SerializeToString()
        receipt = ExecutionReceipt(request_id='hold', input_tokens=2, output_tokens=4)
        sign(receipt, key)
        scores = [(i, -i*2.0) for i in range(16)]
        steps = [Step(0, '', scores, {}) for _ in range(4)]
        def after_replay(*args, **kwargs):
            state['accounts']['auditor']['nonce'] = 40
            return steps
        engine = NS(count_input_tokens=lambda p: 2, generate=lambda *a, **kw: iter(after_replay()),
                    score_forced=after_replay)
        agent = NS(node_id='auditor', key=key, engine=engine, genesis_bytes=genesis,
                   chain_args=NS(audit_role=True, chain_rpc='stub', chain_cli='stub', ledger_chain_id='test'))
        hold = dict(auditor=agent.node_id, state='dispute', audit_round=3,
                    receipt_ref=digest(receipt).hex(), node='producer',
                    producer_profile=base64.b64encode(b'profile').decode(), audit_proof='aa'*32,
                    first_grade={'class': 'token', 'fault': '', 'band': 'token-equality-v1'})
        state = dict(holds={'hold': hold}, accounts={agent.node_id: dict(nonce=9)},
                     parameters=dict(audit_equality=dict(enabled=True, epsilon=1)))
        job = dict(genesis=genesis.hex(), receipt=receipt.SerializeToString().hex(),
                   producer_profile=b'profile'.hex(), prompt='prompt', seed=0)
        for teacher_forcing in ('0', '1'):
            for mismatch in (True, False):
                job['openings'] = [{'token': int(mismatch and i == 2)} for i in range(4)]
                state['accounts'][agent.node_id]['nonce'] = 9
                with patch('agent.join._chain_json', side_effect=targeted_query(state)), patch.dict('os.environ', {'EMBER_AUDIT_TEACHER_FORCING': teacher_forcing}):
                    result = json.loads(execute(agent, TunnelFrame(request_id='hold'), dict(job)))
                pre, main = decode_envelope(result['pre_envelope']), decode_envelope(result['envelope'])
                v = AuditVerdict.FromString(base64.b64decode(main['action']['verdict']))
                verify(v)
                self.assertEqual(v.verdict, 'reject' if mismatch else 'accept')
                self.assertEqual(v.HasField('first_divergence'), mismatch)
                if mismatch:
                    self.assertEqual(v.first_divergence, 2)
                self.assertEqual(v.recompute_ref, recompute_ref(v))
                self.assertEqual(v.tolerance, 'token-equality-v1')
                evidence = dict(auditor=agent.node_id, result_commitment=hold['receipt_ref'], replay_record=v.recompute_ref.hex(), first_divergence=v.first_divergence if mismatch else -1, tolerance=v.tolerance, cause='token::token-equality-v1')
                self.assertEqual(pre['action']['adjudication_commitment'], third_commitment(v.verdict, evidence))
                self.assertEqual((pre['nonce'], main['nonce']), (41, 42))

    def test_execute_numeric_preserves_first_over_tolerance(self):
        from auditor.worker import execute
        actual, receipt_stub, openings = DivergenceTest().numeric_fixture()
        reference = list(actual)
        reference[1] = Step(0, '', [(0, 2.2), (1, 1.0)], {})
        reference[3] = Step(0, '', [(0, 7.0), (1, 1.0)], {})
        key = Ed25519PrivateKey.from_private_bytes(bytes(range(32)))
        genesis = ChainGenesis(chain_id='test').SerializeToString()
        receipt = ExecutionReceipt(request_id='hold', input_tokens=2, output_tokens=4,
                                   output_commitment=receipt_stub.output_commitment)
        sign(receipt, key)
        agent = NS(node_id='auditor', key=key, engine=NS(replay=lambda *args: reference),
                   genesis_bytes=genesis, chain_args=NS(audit_role=True, chain_rpc='stub',
                                                     chain_cli='stub', ledger_chain_id='test'))
        band = ToleranceBand(mode='enforce', tolerance=.01)
        band_id = hashlib.sha256(band.SerializeToString(deterministic=True)).hexdigest()
        manifest = ModelManifest(chain_id='test').SerializeToString()
        hold = dict(auditor='auditor', state='dispute', receipt_ref=digest(receipt).hex(),
                    node='producer', producer_profile=base64.b64encode(b'profile').decode(),
                    audit_proof='aa'*32, model_id='test', audit_output_tokens=4,
                    misjudge_thresholds=[dict(model='test', band=band_id, min_output=1,
                                             max_output=100, length_class='short')])
        state = dict(holds={'hold': hold}, accounts={'auditor': dict(nonce=0)}, parameters={},
                     models={'test': dict(signed_manifest=base64.b64encode(manifest).decode())})
        job = dict(genesis=genesis.hex(), receipt=receipt.SerializeToString().hex(),
                   producer_profile=b'profile'.hex(), prompt='prompt', seed=0, openings=openings)
        policy = NS(metric=LEGACY, layers=[], top_k=2)
        with patch('agent.join._chain_json', side_effect=targeted_query(state)), \
             patch('auditor.worker.load_job_policy', return_value=(policy, NS(actual_device='cpu'), 'bb'*32)), \
             patch('auditor.worker.profile_engine_id', return_value='test'), \
             patch('auditor.worker.select_band', return_value=band), \
             patch('content_policy.from_manifest', return_value=None), patch('auditor.verify_chunks'):
            result = json.loads(execute(agent, TunnelFrame(request_id='hold'), job))
        self.assertNotIn('pre_envelope', result)
        main = decode_envelope(result['envelope'])
        v = AuditVerdict.FromString(base64.b64decode(main['action']['verdict']))
        verify(v)
        self.assertEqual(v.verdict, 'reject')
        self.assertEqual(v.first_divergence, 1)
        self.assertEqual(v.tolerance, band_id)
        self.assertEqual(v.n_out_billed, 4)
        self.assertEqual(v.recompute_ref, recompute_ref(v))


class DivergenceTest(unittest.TestCase):
    def test_equality_paths_same_first_mismatch(self):
        scores = [(i, -i*2.0) for i in range(16)]
        steps = [Step(0, '', scores, {}) for _ in range(5)]
        engine = NS(count_input_tokens=lambda p: 2, generate=lambda *a: iter(steps), score_forced=lambda *a: steps)
        receipt = NS(input_tokens=2, output_tokens=5)
        openings = [{'token': 0} for _ in steps]
        openings[3] = {'token': 1}
        args = (engine, 'p', 0, receipt, openings, 1)
        left, right = compare_detailed(*args), compare_teacher_forced_detailed(*args)
        self.assertEqual(left, right)
        self.assertEqual((left[0], left[4]), ('reject', 3))
        self.assertEqual(compare(*args), left[:4])
        self.assertEqual(compare_teacher_forced(*args), right[:4])
        near = compare_detailed(engine, 'p', 0, receipt, openings, 3)
        self.assertEqual(near[0], 'accept')
        self.assertIsNone(near[4])
        openings[3] = {'token': 0}
        self.assertIsNone(compare_detailed(*args)[4])
        self.assertEqual(compare_detailed(engine, 'p', 0, receipt, openings[:2], 1)[4], 2)
        engine.count_input_tokens = lambda p: 3
        self.assertEqual(compare_detailed(*args)[4], 0)

    def numeric_fixture(self):
        steps = [Step(0, '', [(0, 2.0), (1, 1.0)], {}) for _ in range(4)]
        receipt = NS(input_tokens=2, output_tokens=4, chunk_roots=[], request_id='hold', output_category='',
                     output_commitment=hashlib.sha256(canonical([encode_step(s) for s in steps])).digest())
        return steps, receipt, [encode_step(s) for s in steps]

    def test_numeric_audit_first_over_tolerance(self):
        actual, receipt, openings = self.numeric_fixture()
        reference = list(actual)
        reference[1] = Step(0, '', [(0, 2.2), (1, 1.0)], {})
        reference[3] = Step(0, '', [(0, 7.0), (1, 1.0)], {})
        engine = NS(replay=lambda *a: reference)
        with patch('auditor.verify'), patch('auditor.verify_chunks'):
            result = audit_detailed('p', 0, receipt, openings, engine, .01)
            self.assertEqual((result[0], result[3]), ('dispute', 1))
            self.assertEqual(audit('p', 0, receipt, openings, engine, .01), result[:3])
        for metric in (LEGACY, UNION, RANK_PROB):
            distance, details = numeric_compare(actual, reference, metric)
            self.assertEqual(distance, max(details['position_values']))
            self.assertAlmostEqual(details['position_values'][0], 0)
        reference[2] = Step(0, '', [(0, 2.0), (1, 1.0)], {'layer': b'x'})
        _, details = numeric_compare(actual, reference)
        self.assertEqual(details['failure_index'], 2)
        self.assertTrue(math.isinf(details['position_values'][2]))

    def test_structural_positions_and_nonpositional_faults(self):
        actual, receipt, openings = self.numeric_fixture()
        engine = NS(replay=lambda *a: actual)
        with patch('auditor.verify'), patch('auditor.verify_chunks'):
            broken = list(openings)
            broken[2] = {'token': 0}
            self.assertEqual(audit_detailed('p', 0, receipt, broken, engine, .01)[2:], ('invalid opening', 2))
            receipt.output_commitment = b'wrong'
            self.assertEqual(audit_detailed('p', 0, receipt, openings, engine, .01)[2:], ('output commitment mismatch', None))
