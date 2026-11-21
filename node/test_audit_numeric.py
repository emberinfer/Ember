import hashlib
import contextlib
import io
import json
import math
from pathlib import Path
import struct
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import node_id, sign
from ember_pb2 import ChainGenesis, ExecutionReceipt, ResourceProfile
from engine import EngineCapabilities, Step
from auditor.numeric import LEGACY, RELATIVE_L2, UNION, compare, legacy_distance, peak_relative, union_distance
from auditor.policy import NUMERIC_EXECUTION_PROFILES, grade, load_job_policy, validate_policy

ROOT = Path(__file__).resolve().parents[1]


def signed_job(profile_name='ember-qwen2.5-1.5b-union-v1'):
    raw = (ROOT/'chain-genesis'/profile_name/'genesis.pb').read_bytes()
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

    def test_relative_l2_metric_uses_aligned_relative_distance(self):
        tensor = struct.pack('<ff', 1, 2)
        left = [Step(1, '', [(1, 2.0), (2, 1.0)], {'0': tensor})]
        same = [Step(1, '', [(1, 2.0), (2, 1.0)], {'0': tensor})]
        self.assertEqual(compare(left, same, RELATIVE_L2)[0], 0)
        shifted = [Step(1, '', [(1, 2.0), (2, 1.0)], {'0': struct.pack('<ff', 2, 2)})]
        distance, details = compare(shifted, left, RELATIVE_L2)
        self.assertTrue(math.isfinite(distance) and distance > 0)
        self.assertEqual(details['logit_max'], 0)
        reordered = [Step(1, '', [(2, 1.0), (1, 2.0)], {'0': tensor})]
        self.assertTrue(math.isinf(compare(left, reordered, RELATIVE_L2)[0]))
        with self.assertRaises(ValueError):
            compare(left, same, 'unknown-metric-v9')

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
        policy = ChainGenesis.FromString((ROOT/'chain-genesis/ember-qwen2.5-1.5b-union-v1/genesis.pb').read_bytes()).numeric_audit_policy
        validate_policy(policy)
        cross = next(b for b in policy.bands if b.mode == 'observe')
        self.assertEqual(grade('accept', .25, cross), ('inconclusive', 'compatible-observation'))
        self.assertEqual(grade('dispute', .5, cross), ('inconclusive', 'outside-observation-band'))
        self.assertEqual(grade('dispute', .5, None), ('inconclusive', 'uncalibrated-pair'))
        cross.mode = 'enforce'
        with self.assertRaisesRegex(ValueError, 'cross-runtime'):
            validate_policy(policy)

    def test_s19_policy_enforces_only_same_runtime(self):
        policy = ChainGenesis.FromString((ROOT/'chain-genesis/ember-qwen2.5-1.5b-s19-x86-v1/genesis.pb').read_bytes()).numeric_audit_policy
        validate_policy(policy)
        enforced = [band for band in policy.bands if band.mode == 'enforce']
        self.assertEqual(len(enforced), 1)
        self.assertEqual(enforced[0].producer_engine, enforced[0].reference_engine)
        self.assertEqual(enforced[0].producer_device, enforced[0].reference_device)
        cross = next(band for band in policy.bands if band.mode == 'observe')
        cross.mode = 'enforce'
        with self.assertRaisesRegex(ValueError, 'cross-runtime'):
            validate_policy(policy)

    def test_s22_profile_pins_both_cpu_runtime_families(self):
        job = signed_job('ember-qwen2.5-1.5b-s22-cpu-v1')
        receipt = ExecutionReceipt.FromString(bytes.fromhex(job['receipt']))
        policy, producer, ref = load_job_policy(job, receipt)
        self.assertTrue(ref)
        self.assertEqual({b.producer_engine for b in policy.bands}, {
            'transformers/4.45.2|torch=2.5.1',
            'transformers/4.45.2|torch=2.5.1+cpu'})
        self.assertTrue(all(b.mode == 'enforce' and b.producer_device == 'cpu' for b in policy.bands))

    def test_s21_profile_is_an_authenticated_numeric_profile(self):
        raw = (ROOT/'chain-genesis/ember-qwen2.5-1.5b-s21-prep-v1/genesis.pb').read_bytes()
        genesis = ChainGenesis.FromString(raw)
        self.assertIn(genesis.execution_profile_ver, NUMERIC_EXECUTION_PROFILES)
        validate_policy(genesis.numeric_audit_policy)
        job = signed_job('ember-qwen2.5-1.5b-s21-prep-v1')
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
        original = ChainGenesis.FromString((ROOT/'chain-genesis/ember-qwen2.5-1.5b-union-v1/genesis.pb').read_bytes())
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


class ScoreKindRepresentationTest(unittest.TestCase):
    """logprobs (dynamic-range segmented executors) vs raw-logits references.

    The production fixture is order ddcfb1f7 step 9: the segmented executor
    committed logprobs, the CPU replay reference is raw-logits, and the old
    comparator failed closed with 'score kind' before any numeric comparison.
    """
    FIXTURE = json.loads((Path(__file__).parent/'auditor'/'testdata'/
                          'spot-step9-ddcfb1f7.json').read_text())

    def step(self, side):
        raw = self.FIXTURE[side]
        return Step(raw['token'], '', [tuple(v) for v in raw['topk']],
                    {k: bytes.fromhex(v) for k, v in raw['activations'].items()},
                    raw['score_kind'])

    def test_production_step9_logprobs_unify_with_raw_reference(self):
        actual, reference = self.step('executor'), self.step('reference')
        self.assertEqual(actual.token, self.FIXTURE['claimed_token'])
        self.assertEqual(reference.token, actual.token)
        distance, details = compare([actual], [reference], RELATIVE_L2)
        self.assertEqual(details['reason'], 'numerical comparison')
        self.assertTrue(math.isfinite(distance))
        # Measured 0.0197 (bf16 cross-device noise; activations dominate),
        # far below the chain spot epsilon recorded in the fixture.
        self.assertLess(distance, 0.1)
        self.assertLess(distance, self.FIXTURE['epsilon'])

    def test_pure_representation_shift_measures_zero(self):
        actual = self.step('executor')
        shifted = Step(actual.token, '', [(t, v + 27.6883) for t, v in actual.topk],
                       dict(actual.activations), 'raw-logits')
        distance, details = compare([actual], [shifted], RELATIVE_L2)
        self.assertEqual(details['reason'], 'numerical comparison')
        self.assertLess(distance, 1e-9)

    def test_anchor_scales_are_not_washed_out(self):
        # Rehearsal anchors in the comparator's old both-raw form: bf16
        # cross-device noise (0.0193) must stay accept-scale after
        # normalization; int8-scale divergence (0.674, 1.332) must stay
        # escalation-scale, not collapse toward zero.
        values = [v for _, v in self.step('executor').topk]
        base = [v + (26.875 - values[0]) for v in values]
        pattern = [0.3, -0.7, 1.0, -0.2, 0.5, -1.0, 0.8, -0.4,
                   0.1, -0.9, 0.6, -0.5, 0.9, -0.1, 0.4, -0.8]

        def scaled(target):
            def old(s):
                return legacy_distance(base, [b + s * p for b, p in zip(base, pattern)])
            lo, hi = 0.0, 1.0
            while old(hi) < target:
                hi *= 2
            for _ in range(60):
                mid = (lo + hi) / 2
                if old(mid) < target:
                    lo = mid
                else:
                    hi = mid
            s = (lo + hi) / 2
            return [b + s * p for b, p in zip(base, pattern)]

        for target, low, high in ((0.0193, 0.03, 0.3), (0.674, 0.5, 1.5)):
            perturbed = scaled(target)
            self.assertAlmostEqual(legacy_distance(base, perturbed), target, delta=0.01)
            new = legacy_distance(peak_relative(values), peak_relative(perturbed))
            self.assertTrue(low < new < high, (target, new))
        shrink = 1.332 / 2.332
        perturbed = [b * (1 - shrink) for b in base]
        self.assertAlmostEqual(legacy_distance(base, perturbed), 1.332, delta=0.01)
        self.assertAlmostEqual(
            legacy_distance(peak_relative(values), peak_relative(perturbed)),
            1.332, delta=0.01)

    def test_unknown_and_mixed_kinds_stay_fail_closed(self):
        tensor = struct.pack('<ff', 1, 2)
        good = Step(1, '', [(1, 2.0), (2, 1.0)], {'0': tensor})
        for kind in ('fp16-scores', 'logits', ''):
            bad = Step(1, '', [(1, 2.0), (2, 1.0)], {'0': tensor}, kind)
            distance, details = compare([bad], [good], RELATIVE_L2)
            self.assertTrue(math.isinf(distance))
            self.assertEqual(details['reason'], 'score kind')
        probs = Step(1, '', [(1, -0.5), (2, -1.5)], {'0': tensor}, 'logprobs')
        distance, details = compare([probs], [good], UNION)
        self.assertTrue(math.isinf(distance))
        self.assertEqual(details['reason'], 'score kind')
        twin = Step(1, '', [(1, -0.5), (2, -1.5)], {'0': tensor}, 'logprobs')
        self.assertEqual(compare([probs], [twin], RELATIVE_L2)[0], 0)

    def test_normalized_branch_pairs_by_token_and_checks_layers(self):
        tensor = struct.pack('<ff', 1, 2)
        probs = Step(1, '', [(1, -0.5), (2, -1.5)], {'0': tensor}, 'logprobs')
        # The other runtime's tie order still pairs by token, not position.
        raw_reordered = Step(1, '', [(2, 11.0), (1, 12.0)], {'0': tensor})
        distance, details = compare([probs], [raw_reordered], RELATIVE_L2)
        self.assertEqual(details['reason'], 'numerical comparison')
        self.assertLess(distance, 0.2)
        raw_other = Step(1, '', [(1, 12.0), (3, 11.0)], {'0': tensor})
        distance, details = compare([probs], [raw_other], RELATIVE_L2)
        self.assertTrue(math.isinf(distance))
        self.assertEqual(details['reason'], 'topk/layer mismatch')
        raw_extra = Step(1, '', [(1, 12.0), (2, 11.0)], {'0': tensor, '7': tensor})
        distance, details = compare([probs], [raw_extra], RELATIVE_L2)
        self.assertTrue(math.isinf(distance))
        self.assertEqual(details['reason'], 'layer mismatch')


if __name__ == '__main__':
    unittest.main()
