"""Synthetic security/regression tests; hardware calibration is separate."""
import copy
import io
import contextlib
import json
import math
import struct
from types import SimpleNamespace
import unittest

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import node_id, sign
from ember_pb2 import ChainGenesis, ExecutionReceipt, ResourceProfile
from auditor.gpu import GPU_PROFILE, gpu_engine_id
from auditor.numeric import RANK_PROB, UNION, compare, topk_statistics
from auditor.policy import load_job_policy, select_band, validate_policy
from auditor.router import choose_route, route_job
from engine import EngineCapabilities, Step


def caps():
    return EngineCapabilities('transformers', '5.20.2', 'cuda', 'bfloat16',
                              ('full',), 'raw-logits', 16, True)


def runtime(name='NVIDIA GB10'):
    return dict(actual_device='cuda', torch_version='2.14.0+cu130',
                cuda_version='13.0', accelerator_name=name)


def fixture():
    key = Ed25519PrivateKey.generate()
    p = ResourceProfile(chain_id='ember-gpu-unit', execution_profile_ver=GPU_PROFILE,
        manifest_root=b'x'*32, requested_device='cuda', engine_capabilities=caps().to_proto(),
        **runtime())
    sign(p, key); p.node_id = node_id(p.signer); sign(p, key)
    g = ChainGenesis(chain_id=p.chain_id, execution_profile_ver=GPU_PROFILE,
                     model_manifest_root=p.manifest_root)
    policy = g.numeric_audit_policy
    policy.metric, policy.top_k, policy.calibration_ref = RANK_PROB, 16, b'y'*32
    policy.layers.extend(['0', '14', '27'])
    ident = gpu_engine_id(caps(), runtime())
    policy.bands.add(producer_engine=ident, producer_device='cuda',
        reference_engine=ident, reference_device='cuda', tolerance=.001, mode='enforce')
    sign(g, key)
    r = ExecutionReceipt(node_id=p.node_id); sign(r, key)
    return g, p, r, {'genesis': g.SerializeToString().hex(),
        'producer_profile': p.SerializeToString().hex(), 'receipt': r.SerializeToString().hex()}


class GPUAuditTest(unittest.TestCase):
    def test_second_model_keeps_signed_cuda_policy_and_routing(self):
        from auditor.gpu import SMOLLM_GPU_PROFILE
        genesis, producer, receipt, job = fixture()
        genesis.chain_id = producer.chain_id = 'ember-smollm2-360m-gpu-v1'
        genesis.execution_profile_ver = producer.execution_profile_ver = SMOLLM_GPU_PROFILE
        key = Ed25519PrivateKey.generate()
        sign(producer, key)
        producer.node_id = node_id(producer.signer)
        sign(producer, key)
        receipt.node_id = producer.node_id
        sign(receipt, key)
        sign(genesis, key)
        job.update(genesis=genesis.SerializeToString().hex(),
                   producer_profile=producer.SerializeToString().hex(),
                   receipt=receipt.SerializeToString().hex())
        policy, registered, _ = load_job_policy(job, receipt)
        self.assertEqual(registered.execution_profile_ver, SMOLLM_GPU_PROFILE)
        ident = gpu_engine_id(caps(), runtime())
        self.assertEqual(choose_route(policy, producer,
            [dict(node_id='auditor', engine_id=ident)])[0]['node_id'], 'auditor')
        producer.requested_device = 'auto'
        sign(producer, key)
        job['producer_profile'] = producer.SerializeToString().hex()
        with self.assertRaisesRegex(ValueError, 'explicit CUDA'):
            load_job_policy(job, receipt)

    def test_rank_ties_permutations_and_conditional_probability(self):
        left = [(1, 4.), (2, 2.), (3, 2.)]
        permuted = [(3, 2.), (1, 4.), (2, 2.)]
        stats = topk_statistics(left, permuted)
        self.assertAlmostEqual(stats['rank_distance'], 0)
        self.assertEqual(stats['probability_tv'], 0)
        shifted = [(t, v+100) for t, v in left]
        self.assertAlmostEqual(topk_statistics(left, shifted)['probability_tv'], 0)
        self.assertGreater(topk_statistics(left, [(1, 1.), (2, 2.), (3, 4.)])['rank_distance'], .8)
        self.assertGreater(topk_statistics(left, [(4, 4.), (5, 2.), (6, 2.)])['probability_tv'], .99)

    def test_hybrid_keeps_value_and_layer_checks(self):
        tensor = struct.pack('<ff', 1, 2)
        a = Step(1, '', [(1, 4.), (2, 3.), (3, 2.)], {'0': tensor})
        shifted = copy.deepcopy(a)
        shifted.topk = [(t, v+100) for t, v in a.topk]
        self.assertGreater(compare([a], [shifted], RANK_PROB)[0], .9)
        changed = copy.deepcopy(a); changed.activations['0'] = struct.pack('<ff', 4, 5)
        self.assertGreater(compare([a], [changed], RANK_PROB)[0], .6)
        bad = copy.deepcopy(a); bad.activations['0'] = b'x'
        self.assertTrue(math.isinf(compare([a], [bad], RANK_PROB)[0]))
        bad = copy.deepcopy(a); bad.topk[0] = (1, math.nan)
        self.assertTrue(math.isinf(compare([a], [bad], RANK_PROB)[0]))
        # Explicit metric selection retains the existing union result.
        self.assertEqual(compare([a], [a], UNION)[0], 0)

    def test_runtime_and_metric_are_signed_and_device_is_strict(self):
        g, p, r, job = fixture()
        policy, producer, _ = load_job_policy(job, r)
        reference = SimpleNamespace(device='cuda', capabilities=caps, resource_metadata=runtime)
        self.assertIsNotNone(select_band(policy, gpu_engine_id(caps(), runtime()), 'cuda', reference))
        reference.resource_metadata = lambda: runtime('NVIDIA Thor')
        self.assertIsNone(select_band(policy, gpu_engine_id(caps(), runtime()), 'cuda', reference))
        reference.device = 'cpu'
        self.assertIsNone(select_band(policy, gpu_engine_id(caps(), runtime()), 'cuda', reference))
        self.assertEqual(producer, p)
        g.numeric_audit_policy.bands[0].reference_engine = 'transformers/unpinned'
        with self.assertRaises(ValueError):
            validate_policy(g.numeric_audit_policy)

    def test_loose_layers_do_not_loosen_output_value_checks(self):
        reference = Step(1, '', [(1, 4.), (2, 3.), (3, 2.)],
                         {'0': struct.pack('<ff', 1, 0)})
        honest = copy.deepcopy(reference)
        honest.activations['0'] = struct.pack('<ff', 1, .8)
        self.assertLess(compare([honest], [reference], RANK_PROB)[0], .99)
        cheat = copy.deepcopy(reference)
        cheat.activations['0'] = struct.pack('<ff', 1, .5)
        cheat.topk = [(t, v+1) for t, v in reference.topk]
        # Rank and conditional probabilities are unchanged by this fabrication.
        self.assertLess(compare([cheat], [reference], UNION)[0], .99)
        self.assertGreater(compare([cheat], [reference], RANK_PROB)[0], .99)

    def test_router_excludes_self_observe_and_unknown_pairs(self):
        g, p, _, _ = fixture()
        ident = gpu_engine_id(caps(), runtime())
        routes = [{'node_id': p.node_id, 'engine_id': ident},
                  {'node_id': 'other-class', 'engine_id': gpu_engine_id(caps(), runtime('NVIDIA Thor'))},
                  {'node_id': 'auditor', 'engine_id': ident}]
        chosen, _ = choose_route(g.numeric_audit_policy, p, routes)
        self.assertEqual(chosen['node_id'], 'auditor')
        g.numeric_audit_policy.bands[0].mode = 'observe'
        self.assertEqual(choose_route(g.numeric_audit_policy, p, routes), (None, None))

    def test_router_cross_class_requires_explicit_band_and_worker_runtime(self):
        g, p, _, job = fixture()
        policy_ref = __import__('hashlib').sha256(bytes.fromhex(job['genesis'])).hexdigest()
        ident = gpu_engine_id(caps(), runtime())
        candidate = dict(node_id='auditor', engine_id=ident, command=['python', '-m', 'auditor'], model_path='model')
        def run(command, **kwargs):
            self.assertEqual(json.loads(kwargs['input'])['device'], 'cuda')
            return SimpleNamespace(stderr='', stdout=json.dumps(dict(verdict='accept', distance=0,
                device='cuda', policy_ref=policy_ref, metric=RANK_PROB, reference_engine=ident)))
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(route_job(job, [candidate], run)['audit_route'], 'auditor')
        def wrong(*args, **kwargs):
            return SimpleNamespace(stderr='', stdout=json.dumps(dict(verdict='accept', distance=0,
                device='cpu', policy_ref=policy_ref, metric=RANK_PROB, reference_engine=ident)))
        with self.assertRaisesRegex(ValueError, 'runtime'):
            route_job(job, [candidate], wrong)
        # Merely changing GPU class cannot inherit a same-class tolerance.
        candidate['engine_id'] = gpu_engine_id(caps(), runtime('NVIDIA Thor'))
        self.assertEqual(route_job(job, [candidate])['verdict'], 'inconclusive')
        cross = g.numeric_audit_policy.bands.add()
        cross.CopyFrom(g.numeric_audit_policy.bands[0])
        cross.reference_engine = candidate['engine_id']
        validate_policy(g.numeric_audit_policy)
        self.assertEqual(choose_route(g.numeric_audit_policy, p, [candidate])[0], candidate)


if __name__ == '__main__':
    unittest.main()
