"""Publication preflight and readback must fail closed before spending."""
import base64
import hashlib
import json
from pathlib import Path
import struct
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import sign, digest, node_id
from ember_pb2 import ChainGenesis, ModelManifest, NumericAuditPolicy
from agent.publish import main, readback, _published_record
from auditor.policy import validate_policy
from engine.spi import EngineCapabilities, Step


def test_verify_bundled_unified_uses_legacy_exact_ids(tmp_path, capsys):
    profile = Path(__file__).resolve().parents[1]/'chain-genesis'/'ember-qwen2.5-1.5b-unified-v1'
    args = ['verify', '--conformance', '--genesis', str(profile/'genesis.pb'),
            '--manifest', str(profile/'manifest.pb'), '--model-path', str(tmp_path), '--device', 'cpu']
    caps = EngineCapabilities('transformers', 'test', 'cpu', 'bfloat16', ('full',), 'raw-logits', 16, True)
    activations = {layer: struct.pack('<f', 1) for layer in ('0', '14', '27')}
    step = Step(1, 'hello', [(1, 2.), (2, 1.)], activations)
    with patch('agent.publish.verify_local') as artifacts, \
            patch('engine.adapters.transformers.TransformersEngine') as engine:
        adapter = engine.return_value
        adapter.capabilities.return_value = caps
        adapter.generate.return_value = [step]
        adapter.replay.return_value = [step]
        main(args)
        assert artifacts.call_count == 7
        engine.assert_called_once_with(str(tmp_path), device='cpu', layers=(0, 14, 27))
        result = json.loads(capsys.readouterr().out)
        assert result['result'] == 'PASS'
        assert result['conformance_policy'] == 'legacy exact-ID (relative-l2-v1)'
        assert [row['distance'] for row in result['conformance']] == [0, 0]
        adapter.replay.return_value = [Step(1, 'hello', [(1, 2.), (3, 1.)], activations)]
        with pytest.raises(ValueError, match='exceeds signed numeric tolerance'):
            main(args)


@pytest.mark.parametrize('defect', ['missing bands', 'wrong top_k', 'unordered layers'])
def test_verify_rejects_malformed_new_policy(tmp_path, defect):
    key = Ed25519PrivateKey.generate()
    manifest = ModelManifest(chain_id='ember-test')
    sign(manifest, key)
    genesis = ChainGenesis(chain_id=manifest.chain_id, model_manifest_root=digest(manifest),
                           execution_profile_ver='transformers-test-v1')
    policy = genesis.numeric_audit_policy
    policy.metric, policy.top_k, policy.calibration_ref = 'union-interval-v1', 16, b'x'*32
    policy.layers.extend(['0', '11', '23'] if defect != 'unordered layers' else ['0', '23', '11'])
    if defect != 'missing bands':
        policy.bands.add(producer_engine='engine', reference_engine='engine', producer_device='cpu',
                         reference_device='cpu', mode='enforce', tolerance=.01)
    if defect == 'wrong top_k':
        policy.top_k = 15
    sign(genesis, key)
    genesis_path, manifest_path = tmp_path/'genesis.pb', tmp_path/'manifest.pb'
    genesis_path.write_bytes(genesis.SerializeToString())
    manifest_path.write_bytes(manifest.SerializeToString())
    with patch('engine.adapters.transformers.TransformersEngine') as engine:
        with pytest.raises(ValueError, match='unknown/incomplete numeric policy'):
            main(['verify', '--conformance', '--genesis', str(genesis_path),
                  '--manifest', str(manifest_path), '--model-path', str(tmp_path)])
        engine.assert_not_called()


def test_policy_accepts_real_small_model_layers_and_rejects_ambiguous_indices():
    p = NumericAuditPolicy(metric='union-interval-v1', top_k=16, layers=['0','12','23'], calibration_ref=b'x'*32)
    p.bands.add(producer_engine='a', reference_engine='a', producer_device='cpu', reference_device='cpu', mode='observe', tolerance=.01)
    validate_policy(p)
    for layers in (['0','0'], ['23','12'], ['-1'], ['01'], ['x'], []):
        del p.layers[:]; p.layers.extend(layers)
        with pytest.raises(ValueError): validate_policy(p)


def test_missing_policy_cannot_build_or_submit():
    with patch('agent.publish.publish') as run:
        with pytest.raises(SystemExit): main(['publish'])
        run.assert_not_called()


def test_query_failure_is_not_absence():
    opts = SimpleNamespace(chain_cli='emberchain', chain_rpc='http://localhost', model_id='test')
    for message in ('connection refused', 'query code=2 log=unavailable'):
        with patch('agent.publish.subprocess.run', return_value=SimpleNamespace(returncode=1, stderr=message)):
            with pytest.raises(ValueError, match='cannot establish'): _published_record(opts)
    with patch('agent.publish.subprocess.run', return_value=SimpleNamespace(returncode=1, stderr='2026/11/25 query code=1 log=not found\n')):
        assert _published_record(opts) is None


def test_readback_requires_exact_bytes_hashes_and_publisher(tmp_path):
    key = Ed25519PrivateKey.generate()
    m = ModelManifest(chain_id='test'); sign(m,key)
    g = ChainGenesis(chain_id='test', model_manifest_root=digest(m)); sign(g,key)
    ident = node_id(g.signer)
    gb,mb=g.SerializeToString(deterministic=True),m.SerializeToString(deterministic=True)
    action=dict(genesis=base64.b64encode(gb).decode(),manifest=base64.b64encode(mb).decode(),origin=dict(repository='https://example.org/m',revision='1'*40))
    (tmp_path/'genesis.pb').write_bytes(gb);(tmp_path/'manifest.pb').write_bytes(mb)
    (tmp_path/'registration.json').write_text(json.dumps(action))
    record=dict(genesis_hash=hashlib.sha256(gb).hexdigest(),profile_hash=digest(g).hex(),publication=dict(action,publisher=ident,registered_height=3,manifest_root=digest(m).hex()))
    opts=SimpleNamespace(output=str(tmp_path),model_id='test')
    assert readback(opts,record,ident)['registered_height']==3
    for field,value in [('publisher','wrong'),('registered_height',0),('manifest',base64.b64encode(b'wrong').decode()),('manifest_root','0'*64),('invalid',True)]:
        bad=json.loads(json.dumps(record));bad['publication'][field]=value
        with pytest.raises(ValueError): readback(opts,bad,ident)
