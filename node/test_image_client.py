import copy
import json
from pathlib import Path
import struct

import pytest

from ember_core.t2i import encode_prompts, f32, sqrt32
from engine.adapters.diffusers_t2i import DiffusersT2I, Formula, sha
from image_client import ImageClient, canonical, tree, verify, prompt_anchor


class Components(DiffusersT2I):
    """Deterministic components to exercise transport, not stand in for SD1.5."""
    def __init__(self):
        import torch
        self.torch, self.device, self.ready = torch, 'cpu', True
        self.encodes = self.forwards = 0
        self.tokenizer = lambda s, **kw: {'input_ids': [ord(c) for c in s]}

    def embeddings(self, r):
        self.encodes += 1
        return (0, 1)

    def step(self, x, formula, i, embeddings):
        self.forwards += 1
        return formula.update(x, i, x*0.001, x*0.002)

    def decode(self, x):
        import numpy as np
        return np.full((512, 512, 3), 123, dtype='uint8')


@pytest.fixture
def generated(tmp_path):
    e = Components()
    c = ImageClient(e, tmp_path, 60)
    r = dict(prompt='red cube', negative_prompt='', seed=42, sampler='ddim', steps=3,
             guidance=7500, resolution=0)
    actions = []
    m = c.generate('aa'*32, 'model', r, lambda p, b: canonical({'binding': b}), actions.append,
                   hold_id='hold')
    ctx = dict(m, **{k: m['submission'][k] for k in
                  ('pixel_sha256', 'image_sha256', 'image_bytes', 'final_latent_hash')})
    return e, c, ctx, actions


def audit(c, ctx, opening=None, steps=None):
    steps = [0, 1, 3] if steps is None else steps
    return c.audit(ctx, c.opening(ctx['request_id'], steps) if opening is None else opening,
        steps, dict(base=0, step_coef=0, hard_multiplier=1),
        dict(mode='both', max_relative_l2=0, min_within_one_fraction=1), lambda p: b'signed')


def test_shared_generation_audit_and_retention(generated):
    e, c, ctx, actions = generated
    before = e.encodes
    v = audit(c, ctx)['result']
    assert v['distance'] == 0 and v['verdict'] == 'accept'
    assert v['pixels'] == dict(relative_l2=0, within_one_fraction=1, **{'pass': True})
    assert e.encodes == before+1
    assert actions[0]['type'] == 'submit_image' and actions[0]['hold_id'] == 'hold'
    assert len(actions[0]['image_submission']['leaves']) == 4
    assert not c.prune(ctx['request_id'], now=10000)
    c.mark_settled(ctx['request_id'], 100)
    assert not c.prune(ctx['request_id'], now=159)
    assert c.prune(ctx['request_id'], now=160)


@pytest.mark.parametrize('tamper', ['tokenizer', 'tensor', 'root', 'png', 'anchor', 'request'])
def test_binding_fails_before_inference(generated, tamper):
    e, c, ctx, _ = generated
    o = c.opening(ctx['request_id'], [0, 1, 3])
    if tamper == 'tokenizer':
        # Different vocabulary/version maps the same prompt to different IDs.
        e.tokenizer = lambda s, **kw: {'input_ids': [ord(ch)+1 for ch in s]}
    elif tamper == 'tensor':
        o['tensors']['1'] = o['tensors']['1'][:-1] + bytes([o['tensors']['1'][-1] ^ 1])
    elif tamper == 'root':
        ctx['root'] = '99'*32
    elif tamper == 'png':
        o['png'] += b'x'
    elif tamper == 'anchor':
        o['leaves']['0']['leaf']['anchor_hash'] = '12'*32
    else:
        ctx['request']['prompt'] += '!'
    before = (e.encodes, e.forwards)
    with pytest.raises(ValueError, match='binding failure'):
        audit(c, ctx, o)
    assert before == (e.encodes, e.forwards)


def test_first_step_requires_exact_init_even_with_rebuilt_root(generated):
    e, c, ctx, _ = generated
    o = c.opening(ctx['request_id'], [1])
    raw = bytearray(o['tensors']['0']); raw[40] ^= 1
    o['tensors']['0'] = bytes(raw)
    leaves = copy.deepcopy(ctx['leaves'])
    leaves[0]['input_hash'] = leaves[0]['output_hash'] = leaves[1]['input_hash'] = sha(raw)
    ctx['root'], proofs = tree(leaves)
    for i in (0, 1):
        o['leaves'][str(i)] = dict(leaf=leaves[i], proof=proofs[i])
    with pytest.raises(ValueError, match='init anchor'):
        audit(c, ctx, o, [1])


def test_gate_and_overlong(generated):
    e, c, ctx, _ = generated
    e.ready = False
    with pytest.raises(RuntimeError, match='JOIN'):
        c.generate('bb'*32, 'model', ctx['request'], None, None)
    with pytest.raises(RuntimeError, match='JOIN'):
        audit(c, ctx)
    r = dict(ctx['request'], prompt='x'*76)
    with pytest.raises(ValueError, match='sequence length'):
        e.tokens(r)
    assert len(e.tokens(dict(r, prompt='x'*75))[0]) == 77
    assert e.tokens(dict(r, prompt=''))[0] == [49406]+[49407]*76


@pytest.mark.parametrize('sampler', ['ddim', 'euler'])
@pytest.mark.parametrize('steps', [1, 3, 20, 100])
def test_formula_against_diffusers_reference(sampler, steps):
    import torch
    from diffusers import DDIMScheduler, EulerDiscreteScheduler
    cls = DDIMScheduler if sampler == 'ddim' else EulerDiscreteScheduler
    ref = cls(beta_start=.00085, beta_end=.012, beta_schedule='scaled_linear',
              steps_offset=1, **({'clip_sample': False, 'set_alpha_to_one': False}
                                 if sampler == 'ddim' else {}))
    ref.set_timesteps(steps)
    f = Formula(steps, sampler, 7500)
    assert f.ts == ref.timesteps.tolist()
    assert f.init_noise_sigma == float(ref.init_noise_sigma)
    x = torch.linspace(-2, 2, 120).reshape(1, 4, 5, 6)
    for i, t in enumerate(f.ts):
        u, c = x * .11, x * .17
        torch.testing.assert_close(f.model_input(x, i), ref.scale_model_input(x, t),
                                   rtol=2e-7, atol=2e-7)
        actual = f.update(x, i, u, c)
        expected = ref.step(u + 7.5 * (c - u), t, x).prev_sample
        torch.testing.assert_close(actual, expected, rtol=2e-6, atol=2e-6)
        x = actual


def test_merkle_odd_and_directions(generated):
    _,_,ctx,_ = generated
    leaves = ctx['leaves'][:3]
    root, proofs = tree(leaves)
    for leaf, proof in zip(leaves, proofs):
        verify(leaf, proof, root, 3)
    bad = copy.deepcopy(proofs[2]);bad[0]['hash']='12'*32
    with pytest.raises(ValueError, match='odd node'):
        verify(leaves[2],bad,root,3)


def test_manifest_pins_complete_tokenizer_asset_set():
    """Actual real-CLIP vocabulary mutation is tested by image_conformance."""
    from engine.adapters.diffusers_t2i import ASSETS
    assets = json.loads(ASSETS.read_text())
    assert len([n for n in assets['files'] if n.startswith('tokenizer/')]) == 4


def test_t1_go_fixture_bytes():
    from image_client import leaf_hash, wire_leaf
    fixture = json.loads((Path(__file__).parent/'testdata/image-t1.json').read_text())
    assert [leaf_hash(l).hex() for l in fixture['leaves']] == fixture['leaf_hashes']
    assert tree(fixture['leaves'])[0] == fixture['root']
    assert [wire_leaf(l) for l in fixture['leaves']] == fixture['wire_leaves']
    assert prompt_anchor((fixture['prompt_tokens'], fixture['negative_tokens'])) == fixture['leaves'][0]['anchor_hash']


def test_asset_manifest_enforced(tmp_path):
    e = object.__new__(DiffusersT2I)
    e.root = tmp_path
    (tmp_path/'tokenizer').mkdir()
    (tmp_path/'tokenizer/vocab.json').write_text('good')
    e.assets = dict(files={'tokenizer/vocab.json': sha(b'good')})
    e.verify_assets()
    (tmp_path/'tokenizer/vocab.json').write_text('wrong')
    with pytest.raises(ValueError, match='pinned asset mismatch'):
        e.verify_assets()
    (tmp_path/'tokenizer/extra.json').write_text('{}')
    with pytest.raises(ValueError, match='asset set'):
        e.verify_assets()


def test_gateway_receipt_signature_and_binding():
    import base64
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
    from ember_core import node_id
    from image_client import verify_spool_receipt
    key = Ed25519PrivateKey.generate()
    pub = key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    a = dict(request_id='hold', challenge_height=1, step_set_hash=base64.b64encode(bytes.fromhex('ab'*32)).decode(),
             gateway=node_id(pub), received=True, height=2)
    a['signature'] = base64.b64encode(key.sign(b'EMBER-SPOT-SPOOL-V1\0'+canonical(a))).decode()
    raw = canonical(a)
    assert verify_spool_receipt(raw,pub,'hold',1,'ab'*32,2) == raw
    with pytest.raises(ValueError, match='receipt binding'):
        verify_spool_receipt(raw,pub,'hold',1,'ac'*32,2)


def test_chain_submission_signature_matches_go_order(generated, tmp_path):
    import base64
    from types import SimpleNamespace as NS
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
    from auditor.binding import signed_envelope
    _, _, _, actions = generated
    key = Ed25519PrivateKey.generate()
    public = key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    agent = NS(key=key, chain_args=NS(ledger_chain_id='isolated-t2'))
    env = json.loads(base64.b64decode(signed_envelope(agent,actions[0],1,public)))
    signature = base64.b64decode(env['signature']); env['signature'] = None
    # Exact field ordering is the cross-language transaction signature contract.
    assert list(env['action']) == ['image_submission','commitment_root','type','hold_id']
    key.public_key().verify(signature, b'ember/chain-tx/v1\0'+canonical(env))


def test_calibration_gate_replays_and_fails_closed(tmp_path):
    from ember_core.t2i import init_latent, serialize_latent
    e = Components()
    e.verify_assets = lambda: None
    r = dict(prompt='x', negative_prompt='', seed=42, sampler='euler', steps=2,
             guidance=7500, resolution=0)
    paths, hashes = [], []
    for i, x in enumerate(e.trajectory(r)):
        p = tmp_path/f'{i}.etv'; raw = e.raw(x); p.write_bytes(raw)
        paths.append(str(p)); hashes.append(sha(raw))
    v = dict(seed='42', shape=[1,4,1,1], sha256=sha(serialize_latent(init_latent(42,[1,4,1,1]),[1,4,1,1])))
    vectors = tmp_path/'vectors.json'; vectors.write_text(json.dumps(dict(vectors=[v])))
    cal = dict(request=r, reference_paths=paths, latent_hashes=hashes, max_distances=[0,0,0])
    assert e.self_test(cal, vectors) == [0,0,0] and e.ready
    # A corrupted reference curve cannot preserve the successful admission flag.
    Path(paths[1]).write_bytes(b'corrupted')
    with pytest.raises(ValueError, match='reference hash mismatch'):
        e.self_test(cal, vectors)
    assert not e.ready


def test_final_pixel_both_tracks_are_reported(generated):
    e,c,ctx,_ = generated
    import numpy as np
    e.decode = lambda x: np.full((512,512,3),125,dtype='uint8')
    result = audit(c,ctx)['result']
    assert result['verdict'] == 'reject'
    assert result['pixels']['relative_l2'] > 0
    assert result['pixels']['within_one_fraction'] == 0
    assert result['per_step'][-1]['outcome'] == 'binding_fail'


def test_failed_spool_keeps_durable_trajectory(tmp_path):
    e = Components();c=ImageClient(e,tmp_path,60)
    r = dict(prompt='x',negative_prompt='',seed=42,sampler='ddim',steps=1,guidance=1000,resolution=0)
    def fail(*args):
        raise ConnectionError('isolated gateway offline')
    with pytest.raises(ConnectionError):
        c.generate('ef'*32,'model',r,fail,lambda _:None)
    assert (tmp_path/('ef'*32)/'1.etv').is_file()
    assert not c.prune('ef'*32, now=10**12)


def test_embedded_runtime_uses_finalized_request_and_signed_receipt(tmp_path, monkeypatch):
    import base64
    from types import SimpleNamespace as NS
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives.serialization import Encoding,PublicFormat
    from ember_core import node_id
    from agent.image_runtime import execute
    key,gateway=Ed25519PrivateKey.generate(),Ed25519PrivateKey.generate()
    pub=gateway.public_key().public_bytes(Encoding.Raw,PublicFormat.Raw)
    r=dict(prompt='x',negative_prompt='',seed=42,sampler='ddim',steps=1,guidance=7500,resolution=0)
    hold=dict(image=dict(request=r),model_id='model',request_id='aa'*32,node='executor',state='frozen',freeze_height=1)
    def query(cli,rpc,path):
        if path=='/hold/hold':return hold
        if path=='/state':return dict(height=2)
        if path=='/model/model':return dict(image_profile=dict(allowed_resolutions=[dict(height=h,width=w) for h,w in Components.resolutions]))
        raise AssertionError(path)
    monkeypatch.setattr('agent.join._chain_json',query)
    monkeypatch.setattr('auditor.worker._fresh_audit_nonce',lambda a:0)
    def spool(png,binding,hold_id,h):
        a=dict(request_id=hold_id,challenge_height=1,step_set_hash=base64.b64encode(bytes.fromhex(binding)).decode(),gateway=node_id(pub),received=True,height=2)
        a['signature']=base64.b64encode(gateway.sign(b'EMBER-SPOT-SPOOL-V1\0'+canonical(a))).decode()
        return canonical(a)
    ag=NS(key=key,node_id='executor',engine=Components(),chain_args=NS(chain_rpc='isolated',chain_cli='unused',chain='model',state_dir=str(tmp_path),ledger_chain_id='isolated'),image_transport=NS(spool=spool,gateway_public_key=pub))
    result=json.loads(execute(ag,NS(kind='run',request_id='hold'),dict(prompt='malicious ignored')))
    env=json.loads(base64.b64decode(result['envelope']))
    assert env['action']['type']=='submit_image' and env['action']['hold_id']=='hold'
    assert json.loads((tmp_path/'image-evidence'/('aa'*32)/'journal.json').read_text())['request']==r


def test_chain_audit_envelope_uses_signed_protobufs(generated):
    import base64
    from types import SimpleNamespace as NS
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives.serialization import Encoding,PublicFormat
    from ember_core import node_id,verify as verify_message
    from ember_pb2 import AuditChallenge
    from auditor.spot_wire import SpotVerdict
    from image_client import chain_verdict_action
    _,c,ctx,_=generated
    result=audit(c,ctx,steps=[1,3])['result']
    key=Ed25519PrivateKey.generate()
    pub=key.public_key().public_bytes(Encoding.Raw,PublicFormat.Raw)
    ag=NS(key=key,node_id=node_id(pub))
    hold=dict(receipt_ref=ctx['root'],audit_proof='cc'*32,pipeline=dict(
        commitment_root=base64.b64encode(bytes.fromhex(ctx['root'])).decode(),
        spot=dict(step_set=[1,3],round='2')))
    action=chain_verdict_action(ag,'hold',hold,result,'dd'*32)
    challenge=AuditChallenge.FromString(base64.b64decode(action['challenge']))
    verdict=SpotVerdict.FromString(base64.b64decode(action['verdict']))
    verify_message(challenge);verify_message(verdict)
    assert verdict.verdict=='accept' and [r.step for r in verdict.per_step]==[1,3]
    assert verdict.verdict_distance==0 and verdict.evidence_ref==bytes.fromhex('dd'*32)


@pytest.mark.parametrize('dtype,in_band,bound,expected', [
    ('F16', True, True, 'mismatch'),
    ('F16', True, False, 'binding_fail'),
    (None, True, True, 'binding_fail'),
    ('F32', True, True, 'binding_fail'),
    ('F16', False, True, 'binding_fail'),
])
def test_final_precision_pixel_binding(generated, dtype, in_band, bound, expected):
    import numpy as np
    e, c, ctx, _ = generated
    if dtype is not None:
        ctx['actual_dtype'] = dtype
    opened = c.opening(ctx['request_id'], [3])
    original_step = e.step
    # Audit recomputation drifts; the committed tensors and PNG remain unchanged.
    e.step = lambda *args: original_step(*args) + 1
    committed = e.load(opened['tensors']['3'], (1, 4, 64, 64))
    e.decode = lambda x: np.full((512, 512, 3),
        123 if bound and e.torch.equal(x, committed) else 130, dtype='uint8')
    result = c.audit(ctx, opened, [3],
        dict(base=0, step_coef=0, hard_multiplier=1,
             precision_min=0 if in_band else 100, precision_max=101),
        dict(mode='within_one', max_relative_l2=0, min_within_one_fraction=.999),
        lambda p: b'signed')['result']
    assert result['per_step'][0]['outcome'] == expected
    assert result['pixels']['pass'] == (expected == 'mismatch')


def test_precision_verdict_uses_reject_kind(generated):
    from image_client import chain_verdict_action
    from ember_core import node_id
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
    from types import SimpleNamespace
    from auditor.spot_wire import SpotVerdict
    import base64
    key = Ed25519PrivateKey.generate()
    pub = key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    _, _, ctx, _ = generated
    hold = dict(pipeline=dict(commitment_root=base64.b64encode(bytes.fromhex(ctx['root'])).decode(),
        spot=dict(step_set=[3], round='spot')), receipt_ref='11'*32, audit_proof='22'*32)
    result = dict(root=ctx['root'], challenge_steps=[3], distance=.02,
        precision_mismatch=True, per_step=[dict(step_index=3, outcome='mismatch', distance=.02)])
    action = chain_verdict_action(SimpleNamespace(key=key, node_id=node_id(pub)), 'hold', hold, result, '33'*32)
    v = SpotVerdict.FromString(base64.b64decode(action['verdict']))
    assert (v.verdict, v.verdict_kind) == ('reject', 'precision_mismatch')
