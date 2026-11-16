"""Signed pipeline reconstruction and anti-stitching regression."""
import hashlib
import struct
from types import SimpleNamespace
import unittest

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from auditor.pipeline import load_pipeline_policy, reconstruct
from ember_core import digest, node_id, sign
from ember_pb2 import (ChainGenesis, ModelManifest, NumericAuditPolicy,
                       PipelineExecutionReceipt, ResourceProfile,
                       ShardForwardRequest, ShardForwardResponse, ShardSpec,
                       ToleranceBand)
from engine import EngineCapabilities, Step
from evidence import canonical, encode_step, opening
from shard_rpc import sign_stage_response


class PipelineAuditTest(unittest.TestCase):
    def setUp(self):
        self.keys = [Ed25519PrivateKey.generate(), Ed25519PrivateKey.generate()]
        self.manifest = ModelManifest(chain_id='ember-p', total_layers=2)
        self.specs = [
            ShardSpec(index=0, start_layer=0, end_layer=1,
                      has_embedding=True, parameters_hash=b'a' * 32),
            ShardSpec(index=1, start_layer=1, end_layer=2,
                      has_lm_head=True, parameters_hash=b'b' * 32),
        ]
        self.manifest.shards.extend(self.specs)
        sign(self.manifest, self.keys[0])
        self.genesis = ChainGenesis(chain_id='ember-p',
            model_manifest_root=digest(self.manifest), execution_profile_ver='p')
        sign(self.genesis, self.keys[0])
        self.profiles = []
        for key, spec in zip(self.keys, self.specs):
            profile = ResourceProfile(chain_id='ember-p',
                manifest_root=self.genesis.model_manifest_root,
                execution_profile_ver='p', shard_index=spec.index,
                shard_start_layer=spec.start_layer,
                shard_end_layer=spec.end_layer, shard_total_layers=2,
                shard_has_embedding=spec.has_embedding,
                shard_has_lm_head=spec.has_lm_head,
                shard_parameters_hash=spec.parameters_hash)
            sign(profile, key)
            profile.node_id = node_id(profile.signer)
            sign(profile, key)
            self.profiles.append(profile)
        self.job = self._job()

    def _job(self):
        invocations = []
        receipts = []
        steps = []
        generated = [7, 8]
        for step, token in enumerate(generated):
            prefix = [1, 2, *generated[:step]]
            first_request = ShardForwardRequest(
                request_id='request', step=step, input_ids=prefix)
            hidden = bytes((step + 1, 2))
            first_response = ShardForwardResponse(hidden_bf16=hidden,
                hidden_shape=[1, 1, 1], activations={'0': struct.pack('<f', 1)},
                shard_index=0, shard_parameters_hash=b'a' * 32)
            second_request = ShardForwardRequest(request_id='request', step=step,
                hidden_bf16=hidden, hidden_shape=[1, 1, 1])
            second_response = ShardForwardResponse(token_id=token,
                topk_ids=[token], topk_logits=[1.0],
                activations={'1': struct.pack('<f', 2)}, shard_index=1,
                shard_parameters_hash=b'b' * 32)
            for index, (request, response) in enumerate(((first_request, first_response),
                                                         (second_request, second_response))):
                sign_stage_response(request, response, self.keys[index], self.genesis,
                                    self.manifest, self.specs[index])
                receipts.append(response.receipt)
                invocations.append({
                    'request': request.SerializeToString(deterministic=True).hex(),
                    'response': response.SerializeToString(deterministic=True).hex(),
                    'profile': self.profiles[index].SerializeToString(
                        deterministic=True).hex(),
                })
            steps.append(Step(token, '', [(token, 1.0)], {
                '0': struct.pack('<f', 1), '1': struct.pack('<f', 2)}))
        receipt = PipelineExecutionReceipt(request_id='request', input_tokens=2,
            output_tokens=2, elapsed_ms=10,
            output_commitment=hashlib.sha256(canonical(
                [encode_step(step) for step in steps])).digest())
        receipt.stage_receipts.extend(receipts)
        return {'pipeline_receipt': receipt.SerializeToString(
                    deterministic=True).hex(),
                'invocations': invocations,
                'openings': [opening(step) for step in steps]}

    def _resign_request(self, invocation_index, mutate):
        item = self.job['invocations'][invocation_index]
        request = ShardForwardRequest.FromString(bytes.fromhex(item['request']))
        response = ShardForwardResponse.FromString(bytes.fromhex(item['response']))
        mutate(request)
        stage_index = response.shard_index
        sign_stage_response(request, response, self.keys[stage_index], self.genesis,
                            self.manifest, self.specs[stage_index])
        item['request'] = request.SerializeToString(deterministic=True).hex()
        item['response'] = response.SerializeToString(deterministic=True).hex()
        receipt = PipelineExecutionReceipt.FromString(
            bytes.fromhex(self.job['pipeline_receipt']))
        receipt.stage_receipts[invocation_index].CopyFrom(response.receipt)
        self.job['pipeline_receipt'] = receipt.SerializeToString(
            deterministic=True).hex()

    def _signed_policy_job(self):
        producer = EngineCapabilities('transformers-shard', '4.45.2', 'cpu',
            'bfloat16', ('shard-layer',), 'raw-logits', 16, False,
            roles=('shard-stage',))
        profile_name = 'transformers-qwen2.5-pipeline2-v1'
        self.genesis.execution_profile_ver = profile_name
        for profile, key in zip(self.profiles, self.keys):
            profile.execution_profile_ver = profile_name
            profile.actual_device = 'cpu'
            profile.torch_version = '2.5.1'
            profile.engine_capabilities.CopyFrom(producer.to_proto())
            sign(profile, key)
        self.genesis.numeric_audit_policy.CopyFrom(NumericAuditPolicy(
            metric='union-interval-v1', layers=['0', '14', '27'], top_k=16,
            calibration_ref=b'c' * 32, bands=[ToleranceBand(
                producer_engine='pipeline[transformers-shard/4.45.2|torch=2.5.1]',
                producer_device='cpu',
                reference_engine='transformers/4.45.2|torch=2.5.1',
                reference_device='cpu', tolerance=.02, mode='enforce')]))
        sign(self.genesis, self.keys[0])
        self.job = self._job()
        raw = self.genesis.SerializeToString(deterministic=True)
        self.job.update(genesis=raw.hex(), policy_ref=hashlib.sha256(raw).hexdigest(),
                        tolerance=999, metric='legacy-ranked-v1')
        capabilities = EngineCapabilities('transformers', '4.45.2', 'cpu',
            'bfloat16', ('full',), 'raw-logits', 16, True)
        return SimpleNamespace(device='cpu', capabilities=lambda: capabilities,
            resource_metadata=lambda: {'torch_version': '2.5.1'})

    def test_valid_pipeline(self):
        receipt, steps = reconstruct(self.job)
        self.assertEqual(receipt.output_tokens, 2)
        self.assertEqual([step.token for step in steps], [7, 8])

    def test_resigned_hidden_substitution_is_rejected(self):
        self._resign_request(1, lambda request: setattr(
            request, 'hidden_bf16', b'xx'))
        with self.assertRaisesRegex(ValueError, 'hidden-state'):
            reconstruct(self.job)

    def test_resigned_token_prefix_substitution_is_rejected(self):
        def mutate(request):
            request.input_ids[2] = 99
        self._resign_request(2, mutate)
        with self.assertRaisesRegex(ValueError, 'token prefix'):
            reconstruct(self.job)

    def test_pipeline_policy_ignores_unsigned_overrides(self):
        reference = self._signed_policy_job()
        metric, tolerance, policy_ref = load_pipeline_policy(self.job, reference)
        self.assertEqual(metric, 'union-interval-v1')
        self.assertEqual(tolerance, .02)
        self.assertEqual(policy_ref, self.job['policy_ref'])
        item = self.job['invocations'][0]
        profile = ResourceProfile.FromString(bytes.fromhex(item['profile']))
        profile.torch_version = 'other'
        sign(profile, self.keys[0])
        item['profile'] = profile.SerializeToString(deterministic=True).hex()
        with self.assertRaisesRegex(ValueError, 'uncalibrated'):
            load_pipeline_policy(self.job, reference)


if __name__ == '__main__':
    unittest.main()
