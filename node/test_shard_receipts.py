"""Invocation provenance regression, not physical pipeline acceptance."""
import unittest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import sign, node_id, digest
from ember_pb2 import (ChainGenesis, ModelManifest, ResourceProfile, ShardSpec,
                       ShardForwardRequest, ShardForwardResponse)
from shard_rpc import sign_stage_response, verify_stage_response


class StageReceipts(unittest.TestCase):
    def setUp(self):
        self.key = Ed25519PrivateKey.generate()
        self.manifest = ModelManifest(chain_id='ember-test')
        sign(self.manifest, self.key)
        self.genesis = ChainGenesis(chain_id='ember-test', execution_profile_ver='p')
        self.spec = ShardSpec(index=1, start_layer=14, end_layer=28,
                              parameters_hash=b'a' * 32)
        self.profile = ResourceProfile(chain_id='ember-test', execution_profile_ver='p',
            manifest_root=digest(self.manifest), shard_index=1, shard_start_layer=14,
            shard_end_layer=28, shard_parameters_hash=b'a' * 32)
        sign(self.profile, self.key)
        self.profile.node_id = node_id(self.profile.signer)
        sign(self.profile, self.key)
        self.request = ShardForwardRequest(request_id='attempt-1', step=3,
                                           hidden_bf16=b'ab', hidden_shape=[1, 1, 1])
        self.response = ShardForwardResponse(token_id=7, topk_ids=[7], topk_logits=[1.0],
            activations={'27': b'tensor'}, shard_index=1, shard_parameters_hash=b'a' * 32)
        sign_stage_response(self.request, self.response, self.key, self.genesis,
                            self.manifest, self.spec)

    def test_valid(self):
        receipt = verify_stage_response(self.request, self.response, self.profile)
        self.assertEqual(receipt.end_layer - receipt.start_layer, 14)

    def test_mutations(self):
        for target, attr, value in [
                ('request', 'request_id', 'replay'), ('request', 'step', 4),
                ('request', 'hidden_bf16', b'cd'), ('response', 'token_id', 8),
                ('profile', 'shard_end_layer', 27)]:
            with self.subTest(attr=attr):
                self.setUp()
                setattr(getattr(self, target), attr, value)
                if target == 'profile':
                    sign(self.profile, self.key)
                with self.assertRaises(Exception):
                    verify_stage_response(self.request, self.response, self.profile)

    def test_wrong_registered_identity(self):
        sign(self.profile, Ed25519PrivateKey.generate())
        self.profile.node_id = node_id(self.profile.signer)
        with self.assertRaises(Exception):
            verify_stage_response(self.request, self.response, self.profile)

    def test_unsigned_response_fails_closed(self):
        self.response.ClearField('receipt')
        with self.assertRaises(Exception):
            verify_stage_response(self.request, self.response, self.profile)
