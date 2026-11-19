"""Actual B packages -> C binding and shared pipeline numeric replay."""
import copy
import json
from pathlib import Path
from types import SimpleNamespace

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
import pytest

from auditor.pipeline import reconstruct
from auditor.spot import audit, DirectorySpool, durable_json
from auditor.spot_pipeline import verify_pipeline_binding
from auditor.spot_binding import BindingFailure
from spotcheck import canonical, pipeline_package


def fixture():
    golden = json.loads((Path(__file__).parent/'auditor/testdata/pipeline-record-v1.json').read_text())
    job, manifest = golden['job'], golden['manifest']
    _, actual = reconstruct(job)
    context = dict(kind='pipeline', request_id='ab'*32, invocation_request_id='request',
        total_steps=2, profile_id='tiny', request_tokens=[1,2], claimed_tokens=[7,8],
        root=manifest['commitment_root'], steps=[1], challenge_height=10,
        prompt='fixture', seed=0, metric='legacy-ranked-v1', epsilon=0.01,
        verdict_deadline=100)
    raws = [canonical(v) for v in job['openings']]
    packages = [pipeline_package(manifest, i, job['invocations'].__getitem__, raws.__getitem__) for i in (1,2)]
    return context, packages, actual


def test_pipeline_spot_prefix_and_binding(tmp_path):
    context, packages, actual = fixture()
    durable_json(tmp_path/'opening.json', dict(kind='opening', request_id=context['request_id'],
        challenge_height=10, packages=packages))
    calls=[]
    def replay(prompt,seed,tokens):
        calls.append(list(tokens));return actual[:len(tokens)]
    engine=SimpleNamespace(replay=replay)
    key=Ed25519PrivateKey.generate()
    result=audit(context,DirectorySpool(tmp_path),engine,key,tmp_path/'evidence',height=20)
    assert result['verdict_kind']=='accept'
    assert calls==[[7]]
    context['steps']=[1,2]
    calls.clear()
    result=audit(context,DirectorySpool(tmp_path),engine,key,tmp_path/'evidence',height=20)
    assert result['verdict_kind']=='accept' and calls==[[7,8]]
    assert len(result['per_step'])==2
    # Ruling 11 supplies no next response table or next opening.
    assert set(packages[0]['next'])=={'first_request','request_hashes'}
    bad=copy.deepcopy(packages[0]);bad['next_leaf']['output_hash']='00'*32
    with pytest.raises(BindingFailure):verify_pipeline_binding(bad,context)


@pytest.mark.parametrize('field', ['last_response','opening','first_request'])
def test_pipeline_tamper_is_binding_fail(tmp_path,field):
    context, packages, actual=fixture()
    package=copy.deepcopy(packages[0]);package[field]+='00'
    durable_json(tmp_path/'opening.json',dict(kind='opening', request_id=context['request_id'],challenge_height=10,packages=[package]))
    def forbidden(*args):raise AssertionError('model must not run on unbound evidence')
    result=audit(context,DirectorySpool(tmp_path),SimpleNamespace(replay=forbidden),Ed25519PrivateKey.generate(),tmp_path/'evidence',height=20)
    assert result['per_step'][0]['outcome']=='binding_fail'
    assert Path(result['per_step'][0]['evidence']['path']).exists()


def test_pipeline_token_trusted_prefix_and_anchors():
    context,packages,_=fixture()
    for field,value in [('request_tokens',[9,2]),('claimed_tokens',[9,8]),('claimed_tokens',[7,9])]:
        changed=dict(context,**{field:value})
        with pytest.raises(BindingFailure):
            verify_pipeline_binding(packages[0] if value!=[7,9] else packages[1],changed)


def test_pipeline_escalation_reconstructs_and_replays_once(tmp_path):
    context, packages, actual = fixture()
    golden=json.loads((Path(__file__).parent/'auditor/testdata/pipeline-record-v1.json').read_text())
    context['steps']=[1,2]
    durable_json(tmp_path/'opening.json',dict(kind='opening',request_id=context['request_id'],challenge_height=10,packages=packages))
    spool=DirectorySpool(tmp_path)
    spool.pipeline_full_job=golden['job']
    calls=[]
    def replay(prompt,seed,tokens):
        calls.append(tokens);return actual
    result=audit(context,spool,SimpleNamespace(replay=replay),Ed25519PrivateKey.generate(),tmp_path/'evidence',mode='escalation',height=20)
    assert result['verdict_kind']=='escalation_full_accept' and calls==[[7,8]]


def test_pipeline_uses_existing_transformers_prefix_loop(tmp_path):
    import torch
    from engine import TransformersEngine
    context,packages,actual=fixture()
    durable_json(tmp_path/'opening.json',dict(kind='opening',request_id=context['request_id'],challenge_height=10,packages=packages))
    class Tokenizer:
        def __call__(self,prompt,return_tensors=None):
            return SimpleNamespace(input_ids=torch.tensor([[1,2]]))
        def decode(self,tokens,**kwargs):
            return ''.join(chr(65+t) for t in tokens)
    engine=TransformersEngine.__new__(TransformersEngine)
    engine.torch=torch;engine.device='cpu';engine.tokenizer=Tokenizer();engine.memory_budget=(0,0,1 << 60,0)
    # Independent synthetic reference computation; invoke the actual existing
    # transformers replay loop, including the forced-token prefix append.
    seen=[]
    def step(ids):
        seen.append(ids.tolist()[0])
        token=7+ids.shape[1]-2
        return [(token,1.0)],{'0':torch.tensor([1.0]).numpy().tobytes(), '1':torch.tensor([2.0]).numpy().tobytes()}
    engine._step=step
    context['steps']=[1,2]
    result=audit(context,DirectorySpool(tmp_path),engine,Ed25519PrivateKey.generate(),tmp_path/'evidence',height=20)
    assert result['verdict_kind']=='accept'
    assert seen==[[1,2],[1,2,7]]


def test_committed_token_change_breaks_chain_and_mismatch_evidence_reproduces(tmp_path):
    from spotcheck import pipeline_commitment
    from auditor.spot import reproduce_evidence
    context,packages,actual=fixture()
    golden=json.loads((Path(__file__).parent/'auditor/testdata/pipeline-record-v1.json').read_text())
    job=golden['job'];raws=[canonical(v) for v in job['openings']]
    altered=dict(job['openings'][0],token=9)
    raws[0]=canonical(altered)
    manifest=pipeline_commitment(context['request_id'],'tiny',job['invocations'],raws,2)
    package=pipeline_package(manifest,1,job['invocations'].__getitem__,raws.__getitem__)
    with pytest.raises(BindingFailure,match='token chain'):
        verify_pipeline_binding(package,dict(context,root=manifest['commitment_root']))
    durable_json(tmp_path/'opening.json',dict(kind='opening',request_id=context['request_id'],challenge_height=10,packages=packages))
    def replay(*args):
        return [type(actual[0])(7,'',[(7,2.0)],actual[0].activations)]
    engine=SimpleNamespace(replay=replay)
    result=audit(context,DirectorySpool(tmp_path),engine,Ed25519PrivateKey.generate(),tmp_path/'evidence',height=20)
    bad=result['per_step'][0]
    assert bad['outcome']=='mismatch' and bad['first_divergence_distance']>context['epsilon']
    reproduced=reproduce_evidence(bad['evidence']['path'],engine)
    assert reproduced['outcome']=='mismatch' and reproduced['first_divergence_distance']==bad['first_divergence_distance']
