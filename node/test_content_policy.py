from test_support import targeted_query
"""Signed profile -> executor receipt -> independent registered auditor evidence."""
import base64
import copy
import hashlib
import json
import queue
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import digest, node_id, sign, verify
from ember_pb2 import (AuditVerdict, ChainGenesis, ContentPolicy, ExecutionReceipt,
                       ModelManifest, ResourceProfile, TunnelFrame)
from agent import Agent
from auditor import audit
from auditor.numeric import UNION
from auditor.worker import execute
from content_policy import from_manifest, validate
from engine import EngineCapabilities, MockEngine
from evidence import opening


def anchors():
    key = Ed25519PrivateKey.generate()
    manifest = ModelManifest(chain_id='ember-content-policy-test')
    manifest.content_policy.CopyFrom(ContentPolicy(id='prompt-sha256-denylist', version='1',
        denied_prompt_sha256=[hashlib.sha256(b'policy test input').digest()],
        refusal_tokens=[73, 32, 100, 101, 99, 108, 105, 110, 101, 46], refusal_text='I decline.'))
    sign(manifest, key)
    genesis = ChainGenesis(chain_id=manifest.chain_id, model_manifest_root=digest(manifest),
                           execution_profile_ver='content-policy-test-v1')
    policy = genesis.numeric_audit_policy
    policy.metric, policy.top_k = UNION, 16
    policy.layers.extend(['0', '14', '27'])
    policy.calibration_ref = b'x' * 32
    policy.bands.add(producer_engine='mock/1|torch=unknown', producer_device='cpu',
        reference_engine='mock/1|torch=unknown', reference_device='cpu', tolerance=.001, mode='enforce')
    sign(genesis, key)
    return genesis, manifest


class Reference(MockEngine):
    device = 'cpu'
    def capabilities(self):
        return EngineCapabilities('mock', '1', 'cpu', 'synthetic', ('full',), 'raw-logits', 16, True)
    def replay(self, *args):
        raise AssertionError('policy refusal must not be treated as numerical deviation')


def produce(policy, prompt='policy test input', **extra):
    key = Ed25519PrivateKey.generate()
    producer = Agent(node_id(key.public_key().public_bytes_raw()), MockEngine(), key=key)
    producer.content_policy = policy
    job = dict(prompt=prompt, seed=7, max_tokens=20, **extra)
    outgoing = queue.Queue()
    producer._run(TunnelFrame(request_id='policy-request', payload=json.dumps(job).encode()), outgoing)
    frames = list(outgoing.queue)
    receipt = ExecutionReceipt.FromString(next(f.payload for f in frames if f.kind == 'receipt'))
    opened = [opening(s) for s in producer.sessions['policy-request'][1]]
    return producer, receipt, opened, frames


class ContentPolicyTests(unittest.TestCase):
    def test_registered_refusal_integration(self):
        genesis, manifest = anchors()
        policy = from_manifest(genesis, manifest)
        producer, receipt, opened, frames = produce(policy)
        self.assertEqual(b''.join(f.payload for f in frames if f.kind == 'token'), b'I decline.')
        self.assertEqual(receipt.output_category, 'policy-refusal')
        profile = ResourceProfile(node_id=producer.node_id, chain_id=genesis.chain_id,
            manifest_root=genesis.model_manifest_root, execution_profile_ver=genesis.execution_profile_ver,
            actual_device='cpu', torch_version='unknown', engine_capabilities=Reference().capabilities().to_proto())
        sign(profile, producer.key)
        profile_raw = profile.SerializeToString(deterministic=True)
        agent = Agent('unused', Reference())
        agent.node_id = node_id(agent.key.public_key().public_bytes_raw())
        agent.genesis_bytes = genesis.SerializeToString(deterministic=True)
        agent.chain_args = SimpleNamespace(audit_role=True, chain_rpc='isolated-test-chain',
            chain_cli='unused', ledger_chain_id='ember-content-policy-test-chain')
        enc = lambda raw: base64.b64encode(raw).decode()
        state = dict(parameters={}, holds={'policy-request': dict(auditor=agent.node_id, state='dispute',
            receipt_ref=digest(receipt).hex(), producer_profile=enc(profile_raw), audit_proof='ab' * 32)},
            models={genesis.chain_id: dict(signed_manifest=enc(manifest.SerializeToString()))},
            accounts={agent.node_id: dict(nonce=0)})
        job = dict(genesis=agent.genesis_bytes.hex(), producer_profile=profile_raw.hex(),
                   receipt=receipt.SerializeToString().hex(), prompt='policy test input', seed=7, openings=opened)
        with patch('agent.join._chain_json', side_effect=targeted_query(state)):
            result = json.loads(execute(agent, TunnelFrame(request_id='policy-request'), job))
        envelope = json.loads(base64.b64decode(result['envelope']))
        verdict = AuditVerdict.FromString(base64.b64decode(envelope['action']['verdict']))
        verify(verdict)
        self.assertEqual(verdict.verdict, 'accept')
        self.assertFalse(verdict.HasField('offense'))
        self.assertEqual(verdict.n_out_billed, 0)
        self.assertEqual(result['grade'], 'profile-policy-compliant-refusal')
        print('POLICY_EVIDENCE ' + json.dumps(dict(request='policy-request',
            policy_id=policy.id, policy_version=policy.version, manifest_root=digest(manifest).hex(),
            verdict=verdict.verdict, grade=result['grade'], distance=result['distance'],
            n_out_billed=verdict.n_out_billed, offense=verdict.HasField('offense'))))

    def test_anchors_and_versions_fail_closed(self):
        genesis, manifest = anchors()
        for field in ('id', 'version'):
            bad = copy.deepcopy(manifest.content_policy)
            setattr(bad, field, '')
            with self.assertRaises(ValueError): validate(bad)
        manifest.content_policy.version = '2'
        with self.assertRaises((ValueError, InvalidSignature)): from_manifest(genesis, manifest)
        sign(manifest, Ed25519PrivateKey.generate())
        with self.assertRaises((ValueError, InvalidSignature)): from_manifest(genesis, manifest)

    def test_undeclared_or_unwarranted_refusal_is_not_legal(self):
        genesis, manifest = anchors()
        policy = from_manifest(genesis, manifest)
        producer, receipt, opened, _ = produce(policy)
        for prompt, declaration in [('policy test input', None), ('other prompt', policy)]:
            self.assertEqual(audit(prompt, 7, receipt, opened, MockEngine(), 0,
                                   content_policy=declaration)[0], 'reject')
        # Even an authorized category cannot bypass committed evidence integrity.
        opened[0]['token'] += 1
        self.assertEqual(audit('policy test input', 7, receipt, opened, MockEngine(), 0,
                               content_policy=policy)[0], 'reject')

    def test_no_policy_preserves_normal_output_and_ignores_job_override(self):
        _, receipt, opened, _ = produce(None, content_filter=True, refuse=True)
        self.assertEqual(receipt.output_category, '')
        self.assertEqual(audit('policy test input', 7, receipt, opened, MockEngine(), 0)[0], 'accept')
        _, receipt, opened, _ = produce(anchors()[1].content_policy, prompt='ordinary prompt')
        self.assertEqual(receipt.output_category, '')
        self.assertEqual(audit('ordinary prompt', 7, receipt, opened, MockEngine(), 0)[0], 'accept')
