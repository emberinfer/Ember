import hashlib
import contextlib
import io
import json
import math
from pathlib import Path
import sys
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "testdata"))
from profile_fixtures import profiles
FIXTURES = profiles()
import struct
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import node_id, sign
from ember_pb2 import ChainGenesis, ExecutionReceipt, ResourceProfile
from engine import EngineCapabilities, Step
from auditor.numeric import LEGACY, UNION, compare, union_distance
from auditor.policy import NUMERIC_EXECUTION_PROFILES, grade, load_job_policy, validate_policy

ROOT = Path(__file__).resolve().parents[1]


def signed_job(profile_name='ember-qwen2.5-1.5b-union-v1'):
    raw = (FIXTURES/profile_name/'genesis.pb').read_bytes()
    genesis = ChainGenesis.FromString(raw)
    key = Ed25519PrivateKey.generate()
    capabilities = EngineCapabilities('transformers', '4.45.2', 'cpu', 'bfloat16',
                                      ('full',), 'raw-logits', 16, True)
    profile = ResourceProfile(chain_id=genesis.chain_id,
        manifest_root=genesis.model_manifest_root,
        execution_profile_ver=genesis.execution_profile_ver,
        actual_device='cpu', torch_version='2.5.1',
        engine_capabilities=capabilities.to_proto())
    sign(profile, key)
    profile.node_id = node_id(profile.signer)
    sign(profile, key)
    receipt = ExecutionReceipt(node_id=profile.node_id)
    sign(receipt, key)
    return {'genesis': raw.hex(), 'producer_profile': profile.SerializeToString().hex(),
            'receipt': receipt.SerializeToString().hex(), 'engine': 'transformers',
            'prompt': 'synthetic unit fixture', 'seed': 7, 'openings': {}}


class NumericAuditTest(unittest.TestCase):
    def test_new_publisher_profile_uses_authenticated_policy(self):
        job = signed_job()
        genesis = ChainGenesis.FromString(bytes.fromhex(job['genesis']))
        publisher = Ed25519PrivateKey.generate()
        genesis.chain_id = 'ember-publisher-cpu'
        genesis.execution_profile_ver = 'publisher-cpu-v1'
        sign(genesis, publisher)
        node_key = Ed25519PrivateKey.generate()
        profile = ResourceProfile.FromString(bytes.fromhex(job['producer_profile']))
        profile.chain_id = genesis.chain_id
        profile.execution_profile_ver = genesis.execution_profile_ver
        sign(profile, node_key)
        profile.node_id = node_id(profile.signer)
        sign(profile, node_key)
        receipt = ExecutionReceipt(node_id=profile.node_id)
        sign(receipt, node_key)
        job.update(genesis=genesis.SerializeToString().hex(),
                   producer_profile=profile.SerializeToString().hex())
        policy, _, _ = load_job_policy(job, receipt)
        self.assertEqual(policy, genesis.numeric_audit_policy)
        profile.execution_profile_ver = 'different-profile'
        sign(profile, node_key)
        job['producer_profile'] = profile.SerializeToString().hex()
        with self.assertRaisesRegex(ValueError, 'binding mismatch'):
            load_job_policy(job, receipt)

    def test_union_is_id_aligned_and_ties_are_censored(self):
        tensor = struct.pack('<ff', 1, 2)
        left = [Step(1, '', [(1, 2.0), (2, 1.0)], {'0': tensor})]
        reordered = [Step(1, '', [(2, 1.0), (1, 2.0)], {'0': tensor})]
        tied = [Step(1, '', [(1, 2.0), (3, 1.0)], {'0': tensor})]
        self.assertTrue(math.isinf(compare(left, reordered, LEGACY)[0]))
        self.assertEqual(compare(left, reordered, UNION)[0], 0)
        distance, details = compare(left, tied, UNION)
        self.assertEqual(distance, 0)  # unknown tail values are intervals, not zero
        self.assertEqual(details['censored_union_entries'], 2)
        distance, missing, _ = union_distance([(1, 3.0), (2, 1.0)], [(3, 2.0), (2, 1.0)])
        self.assertTrue(math.isfinite(distance) and distance > 0)
        self.assertEqual(missing, 2)

    def test_activations_count_shapes_and_nonfinite_cannot_accept(self):
        raw = struct.pack('<ff', 1, 2)
        good = Step(1, '', [(1, 1.0)], {'0': raw})
        self.assertTrue(math.isinf(compare([good], [], UNION)[0]))
        for bad in [Step(1, '', [(1, math.nan)], {'0': raw}),
                    Step(1, '', [(1, 1e308)], {'0': raw}),
                    Step(1, '', [(1, 1), (1, 1)], {'0': raw}),
                    Step(1, '', [(1, 1)], {'0': b'\x00'}),
                    Step(1, '', [(1, 1)], {'0': struct.pack('<ff', math.nan, 2)}),
                    Step(1, '', [(1, 1)], {}, 'logprobs')]:
            self.assertTrue(math.isinf(compare([bad], [good], UNION)[0]))
        changed = Step(1, '', [(1, 1.0)], {'0': struct.pack('<ff', 2, 2)})
        distance, details = compare([changed], [good], UNION)
        self.assertGreater(distance, .001)
        self.assertEqual(details['logit_max'], 0)

    def test_observe_unknown_pairs_and_policy_validation(self):
        policy = ChainGenesis.FromString((FIXTURES/'ember-qwen2.5-1.5b-union-v1/genesis.pb').read_bytes()).numeric_audit_policy
        validate_policy(policy)
        cross = next(b for b in policy.bands if b.mode == 'observe')
        self.assertEqual(grade('accept', .25, cross), ('inconclusive', 'compatible-observation'))
        self.assertEqual(grade('dispute', .5, cross), ('inconclusive', 'outside-observation-band'))
        self.assertEqual(grade('dispute', .5, None), ('inconclusive', 'uncalibrated-pair'))
        cross.mode = 'enforce'
        with self.assertRaisesRegex(ValueError, 'cross-runtime'):
            validate_policy(policy)

    def test_cpu_strict_policy_enforces_only_same_runtime(self):
        policy = ChainGenesis.FromString((FIXTURES/'ember-qwen2.5-1.5b-cpu-strict-v1/genesis.pb').read_bytes()).numeric_audit_policy
        validate_policy(policy)
        enforced = [band for band in policy.bands if band.mode == 'enforce']
        self.assertEqual(len(enforced), 1)
        self.assertEqual(enforced[0].producer_engine, enforced[0].reference_engine)
        self.assertEqual(enforced[0].producer_device, enforced[0].reference_device)
        cross = next(band for band in policy.bands if band.mode == 'observe')
        cross.mode = 'enforce'
        with self.assertRaisesRegex(ValueError, 'cross-runtime'):
            validate_policy(policy)

    def test_cpu_profile_pins_both_cpu_runtime_families(self):
        job = signed_job('ember-qwen2.5-1.5b-cpu-v1')
        receipt = ExecutionReceipt.FromString(bytes.fromhex(job['receipt']))
        policy, producer, ref = load_job_policy(job, receipt)
        self.assertTrue(ref)
        self.assertEqual({b.producer_engine for b in policy.bands}, {
            'transformers/4.45.2|torch=2.5.1',
            'transformers/4.45.2|torch=2.5.1+cpu'})
        self.assertTrue(all(b.mode == 'enforce' and b.producer_device == 'cpu' for b in policy.bands))

    def test_cpu_preview_profile_is_an_authenticated_numeric_profile(self):
        raw = (FIXTURES/'ember-qwen2.5-1.5b-cpu-preview-v1/genesis.pb').read_bytes()
        genesis = ChainGenesis.FromString(raw)
        self.assertIn(genesis.execution_profile_ver, NUMERIC_EXECUTION_PROFILES)
        validate_policy(genesis.numeric_audit_policy)
        job = signed_job('ember-qwen2.5-1.5b-cpu-preview-v1')
        receipt = ExecutionReceipt.FromString(bytes.fromhex(job['receipt']))
        policy, _, policy_ref = load_job_policy(job, receipt)
        self.assertEqual(policy, genesis.numeric_audit_policy)
        self.assertEqual(policy_ref, hashlib.sha256(raw).hexdigest())

    def test_worker_does_not_slash_changed_runtime_or_unsigned_tolerance(self):
        from auditor.__main__ import main
        job = signed_job()
        job['tolerance'] = 1e20  # must not override the signed same-runtime band
        def fake(device, version, torch):
            return SimpleNamespace(device=device, close=lambda: None,
                capabilities=lambda: EngineCapabilities('transformers', version, device,
                    'bfloat16', ('full',), 'raw-logits', 16, True),
                resource_metadata=lambda: {'torch_version': torch})
        first = fake('cpu', '4.45.2', '2.5.1')
        changed = fake('cuda', '5.20.2', '2.14.0+cu130')
        output = io.StringIO()
        with patch('auditor.__main__.load_engine', side_effect=[first, changed]), \
             patch('auditor.__main__.audit', return_value=('dispute', .1, 'synthetic unit mismatch')) as run, \
             patch('sys.stdin', io.StringIO(json.dumps(job))), contextlib.redirect_stdout(output):
            main()
        result = json.loads(output.getvalue())
        self.assertEqual(result['verdict'], 'inconclusive')
        self.assertEqual(result['grade'], 'second-reference-runtime-mismatch')
        self.assertEqual(run.call_count, 1)
        self.assertEqual(run.call_args.args[-1], .001)

    def test_policy_requires_signed_producer_and_receipt_binding(self):
        original = ChainGenesis.FromString((FIXTURES/'ember-qwen2.5-1.5b-union-v1/genesis.pb').read_bytes())
        key = Ed25519PrivateKey.generate()
        capabilities = EngineCapabilities('transformers', '4.45.2', 'cpu', 'bfloat16', ('full',), 'raw-logits', 16, True)
        profile = ResourceProfile(chain_id=original.chain_id, manifest_root=original.model_manifest_root,
            execution_profile_ver=original.execution_profile_ver, actual_device='cpu', torch_version='2.5.1',
            engine_capabilities=capabilities.to_proto())
        sign(profile, key); profile.node_id = node_id(profile.signer); sign(profile, key)
        receipt = ExecutionReceipt(node_id=profile.node_id); sign(receipt, key)
        raw = original.SerializeToString(deterministic=True)
        job = {'genesis': raw.hex(), 'producer_profile': profile.SerializeToString().hex()}
        _, observed, policy_ref = load_job_policy(job, receipt)
        self.assertEqual(observed.node_id, profile.node_id)
        self.assertEqual(policy_ref, hashlib.sha256(raw).hexdigest())
        receipt.node_id = 'another-node'
        with self.assertRaisesRegex(ValueError, 'binding'):
            load_job_policy(job, receipt)
        receipt.node_id = profile.node_id
        profile.actual_device = 'cuda'  # changing signed metadata must be detected
        job['producer_profile'] = profile.SerializeToString().hex()
        with self.assertRaises(Exception):
            load_job_policy(job, receipt)


if __name__ == '__main__':
    unittest.main()
