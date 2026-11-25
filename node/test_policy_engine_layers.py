import base64
from contextlib import nullcontext
import hashlib
import io
import json
from pathlib import Path
import sys
from types import SimpleNamespace
from unittest.mock import Mock

import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from agent import Agent
from agent.join import join
from auditor import __main__ as auditor_cli
from ember_core import digest, node_id, sign
from ember_pb2 import ChainGenesis, ExecutionReceipt, ModelManifest, ResourceProfile, TunnelFrame
from engine import EngineCapabilities


@pytest.fixture
def mock_transformers(monkeypatch):
    def load(total):
        model = Mock()
        model.to.return_value.eval.return_value = model
        model.model.layers = [Mock() for _ in range(total)]
        monkeypatch.setitem(sys.modules, 'torch', SimpleNamespace(
            __version__='2.5.1', version=SimpleNamespace(cuda=None), bfloat16=object()))
        monkeypatch.setitem(sys.modules, 'transformers', SimpleNamespace(
            __version__='4.45.2', AutoModelForCausalLM=Mock(**{'from_pretrained.return_value': model}),
            AutoTokenizer=Mock(**{'from_pretrained.return_value': SimpleNamespace(eos_token_id=0)})))
        monkeypatch.setattr('tokenization.artifact_version', lambda _: 'test-tokenizer')
        return model

    return load


def unified_genesis():
    root = Path(__file__).resolve().parents[1]
    return ChainGenesis.FromString(
        (root / 'chain-genesis/ember-qwen2.5-1.5b-union-v1/genesis.pb').read_bytes())


@pytest.mark.parametrize('path', ['join', 'audit'])
@pytest.mark.parametrize('total, policy_layers, expected', [
    pytest.param(24, ['0', '11', '23'], (0, 11, 23), id='g3c'),
    pytest.param(28, ['0', '14', '27'], (0, 14, 27), id='unified-1.5b'),
    pytest.param(24, None, (0, 12, 23), id='legacy'),
    pytest.param(24, ['0', '11', '24'], None, id='out-of-range'),
])
def test_transformers_layers_from_signed_policy(monkeypatch, tmp_path, mock_transformers,
                                                 path, total, policy_layers, expected):
    model = mock_transformers(total)

    publisher = Ed25519PrivateKey.generate()
    manifest = ModelManifest(chain_id='ember-test')
    sign(manifest, publisher)
    genesis = ChainGenesis(chain_id=manifest.chain_id, model_manifest_root=digest(manifest),
                           execution_profile_ver='v1')
    if policy_layers is not None:
        template = unified_genesis()
        genesis.execution_profile_ver = template.execution_profile_ver
        genesis.numeric_audit_policy.CopyFrom(template.numeric_audit_policy)
        if total == 28:
            assert list(genesis.numeric_audit_policy.layers) == policy_layers
        else:
            genesis.numeric_audit_policy.layers[:] = policy_layers
    sign(genesis, publisher)

    observed = []
    with (pytest.raises(ValueError, match='sampled layer is not in model')
          if expected is None else nullcontext()):
        if path == 'join':
            def serve(agent, address, **kwargs):
                payload = dict(genesis=base64.b64encode(genesis.SerializeToString()).decode(),
                               manifest=base64.b64encode(manifest.SerializeToString()).decode(),
                               nonce=base64.b64encode(b'nonce').decode(), questions=['hello'])
                kwargs['authenticate'](TunnelFrame(payload=json.dumps(payload).encode()))
                observed.append(agent.engine.layers)

            monkeypatch.setattr(Agent, 'serve', serve)
            join(SimpleNamespace(state_dir=str(tmp_path), coordinator='127.0.0.1:19191',
                                 tls_fingerprint=None, insecure_localhost=True, chain=genesis.chain_id,
                                 engine='transformers', model_path=str(tmp_path), device='cpu'))
        else:
            producer = Ed25519PrivateKey.generate()
            capabilities = EngineCapabilities('transformers', '4.45.2', 'cpu', 'bfloat16',
                                              ('full',), 'raw-logits', 16, True)
            profile = ResourceProfile(chain_id=genesis.chain_id, manifest_root=genesis.model_manifest_root,
                execution_profile_ver=genesis.execution_profile_ver, actual_device='cpu',
                torch_version='2.5.1', engine_capabilities=capabilities.to_proto())
            sign(profile, producer)
            profile.node_id = node_id(profile.signer)
            sign(profile, producer)
            receipt = ExecutionReceipt(node_id=profile.node_id)
            sign(receipt, producer)
            job = dict(genesis=genesis.SerializeToString().hex(),
                       producer_profile=profile.SerializeToString().hex(),
                       receipt=receipt.SerializeToString().hex(), engine='transformers',
                       model_path=str(tmp_path), device='cpu', tolerance=.001,
                       prompt='hello', seed=7, openings={})

            def audit(prompt, seed, receipt, openings, engine, tolerance, **comparison):
                observed.append(engine.layers)
                assert tuple(map(int, comparison.get('required_layers', ()))) == tuple(
                    map(int, policy_layers or ()))
                return 'accept', 0.0, 'match'

            monkeypatch.setattr(auditor_cli, 'audit', audit)
            monkeypatch.setattr(sys, 'stdin', io.StringIO(json.dumps(job)))
            auditor_cli.main()
    if expected is not None:
        assert observed == [expected]
        assert tuple(index for index, layer in enumerate(model.model.layers)
                     if layer.register_forward_hook.called) == expected


def test_full_model_pipeline_reference_uses_signed_layers(monkeypatch, tmp_path, mock_transformers):
    mock_transformers(24)
    publisher = Ed25519PrivateKey.generate()
    genesis = unified_genesis()
    genesis.execution_profile_ver = 'transformers-qwen2.5-pipeline-test'
    policy = genesis.numeric_audit_policy
    policy.layers[:] = ['0', '11', '23']
    del policy.bands[:]
    runtime = 'transformers/4.45.2|torch=2.5.1'
    policy.bands.add(producer_engine=f'pipeline[{runtime}]', producer_device='cpu',
                     reference_engine=runtime, reference_device='cpu', mode='enforce', tolerance=.001)
    sign(genesis, publisher)
    raw = genesis.SerializeToString()
    profile = ResourceProfile(chain_id=genesis.chain_id, manifest_root=genesis.model_manifest_root,
        execution_profile_ver=genesis.execution_profile_ver, actual_device='cpu', torch_version='2.5.1')
    profile.engine_capabilities.engine_name = 'transformers'
    profile.engine_capabilities.engine_version = '4.45.2'
    profile.engine_capabilities.evidence_levels.append('shard-layer')
    profile.engine_capabilities.roles.append('shard-stage')
    sign(profile, publisher)
    profile.node_id = node_id(profile.signer)
    sign(profile, publisher)
    job = dict(kind='pipeline', engine='transformers', model_path=str(tmp_path), device='cpu',
               genesis=raw.hex(), policy_ref=hashlib.sha256(raw).hexdigest(),
               invocations=[dict(profile=profile.SerializeToString().hex())])
    replay = Mock(return_value=('accept', 0.0, 'match', []))
    monkeypatch.setattr(auditor_cli, 'audit_pipeline', replay)
    monkeypatch.setattr(sys, 'stdin', io.StringIO(json.dumps(job)))
    auditor_cli.main()
    replay.assert_called_once()
    assert replay.call_args.args[1].layers == (0, 11, 23)
