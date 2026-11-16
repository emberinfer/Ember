"""GPU stage policy must not widen any signed runtime pair's band."""
import hashlib
from types import SimpleNamespace
import unittest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import sign, node_id
from ember_pb2 import ChainGenesis, ResourceProfile
from auditor.pipeline import load_pipeline_policy
from auditor.gpu import gpu_engine_id
from engine import EngineCapabilities


class PipelineGPUPolicyTest(unittest.TestCase):
    def setUp(self):
        self.key = Ed25519PrivateKey.generate()
        self.caps = EngineCapabilities('transformers', '5.20.2', 'cuda',
            'bfloat16', ('shard-layer',), 'raw-logits', 16, False, roles=('shard-stage',))
        self.runtime = dict(actual_device='cuda', torch_version='2.14.0+cu130',
            cuda_version='13.0', accelerator_name='NVIDIA GB10')
        self.profile = ResourceProfile(chain_id='gpu-pipeline', manifest_root=b'm'*32,
            execution_profile_ver='transformers-qwen2.5-pipeline-gpu-v3', requested_device='cuda',
            engine_capabilities=self.caps.to_proto(), **self.runtime)
        sign(self.profile,self.key);self.profile.node_id=node_id(self.profile.signer)
        sign(self.profile,self.key)
        self.genesis = ChainGenesis(chain_id=self.profile.chain_id,
            execution_profile_ver=self.profile.execution_profile_ver,model_manifest_root=b'm'*32)
        policy=self.genesis.numeric_audit_policy
        policy.metric='rank-probability-v1';policy.top_k=16;policy.layers.extend(['0','14','27']);policy.calibration_ref=b'c'*32
        ident=gpu_engine_id(self.caps,self.runtime)
        policy.bands.add(producer_engine=ident,producer_device='cuda',reference_engine=ident,
            reference_device='cuda',tolerance=.001,mode='enforce')
        self.reference=SimpleNamespace(device='cuda',capabilities=lambda:self.caps,resource_metadata=lambda:self.runtime)

    def job(self,profiles=None):
        sign(self.genesis,self.key);raw=self.genesis.SerializeToString(deterministic=True)
        return dict(genesis=raw.hex(),policy_ref=hashlib.sha256(raw).hexdigest(),
            invocations=[dict(profile=p.SerializeToString().hex()) for p in (profiles or [self.profile])])

    def test_signed_gpu_pair_and_strict_device(self):
        metric,tolerance,_=load_pipeline_policy(self.job(),self.reference)
        self.assertEqual((metric,tolerance),('rank-probability-v1',.001))
        self.profile.requested_device='auto';sign(self.profile,self.key)
        with self.assertRaisesRegex(ValueError,'explicit CUDA'):load_pipeline_policy(self.job(),self.reference)

    def test_unknown_reference_and_observe_never_authorize(self):
        self.genesis.numeric_audit_policy.bands[0].mode='observe'
        with self.assertRaisesRegex(ValueError,'uncalibrated'):load_pipeline_policy(self.job(),self.reference)
        self.genesis.numeric_audit_policy.bands[0].mode='enforce'
        self.runtime['accelerator_name']='unknown GPU'
        with self.assertRaises(ValueError):load_pipeline_policy(self.job(),self.reference)

    def test_common_threshold_is_required_for_mixed_stages(self):
        other=ResourceProfile();other.CopyFrom(self.profile);other.accelerator_name='NVIDIA Thor';other_key=Ed25519PrivateKey.generate();sign(other,other_key);other.node_id=node_id(other.signer);sign(other,other_key)
        ident=gpu_engine_id(self.caps,dict(self.runtime,accelerator_name='NVIDIA Thor'))
        band=self.genesis.numeric_audit_policy.bands.add();band.CopyFrom(self.genesis.numeric_audit_policy.bands[0]);band.producer_engine=ident;band.tolerance=.5
        with self.assertRaisesRegex(ValueError,'common enforced'):load_pipeline_policy(self.job([self.profile,other]),self.reference)
        self.genesis.numeric_audit_policy.bands[0].tolerance=.5
        self.assertEqual(load_pipeline_policy(self.job([self.profile,other]),self.reference)[1],.5)
