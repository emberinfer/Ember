"""Receipt-anchored derivation and its existing Merkle binding boundary."""
import base64
import hashlib
import json
from pathlib import Path
from types import SimpleNamespace as NS

import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import sign
from ember_pb2 import ChainGenesis, PipelineExecutionReceipt, ShardForwardRequest, ShardForwardResponse
from auditor.spot_context import ContextDerivationError
from auditor.spot_worker import context_for
from auditor.spot_pipeline import verify_pipeline_binding
from auditor.spot_binding import BindingFailure
from spotcheck import canonical, pipeline_commitment, pipeline_package


def setup(tmp_path, metric='legacy-ranked-v1'):
    job = json.loads((Path(__file__).parent/'auditor/testdata/pipeline-record-v1.json').read_text())['job']
    request_id = 'ab'*32
    # Retain golden openings, but bind this synthetic request to the real hold-ID convention.
    for item in job['invocations']:
        req = ShardForwardRequest.FromString(bytes.fromhex(item['request']))
        req.request_id = request_id
        item['request'] = req.SerializeToString().hex()
        res = ShardForwardResponse.FromString(bytes.fromhex(item['response']))
        res.receipt.request_id = request_id
        item['response'] = res.SerializeToString().hex()
    receipt = PipelineExecutionReceipt.FromString(bytes.fromhex(job['pipeline_receipt']))
    receipt.request_id = request_id
    raw = receipt.SerializeToString()
    job.update(pipeline_receipt=raw.hex(), first_invocation_request=job['invocations'][0]['request'],
               prompt='fixture', seed=0, metric=metric, tolerance=0.01)
    openings = [canonical(v) for v in job['openings']]
    manifest = pipeline_commitment(request_id, 'tiny', job['invocations'], openings, 2)
    packages = [pipeline_package(manifest, i, job['invocations'].__getitem__, openings.__getitem__) for i in (1, 2)]
    genesis = ChainGenesis(chain_id='model')
    genesis.numeric_audit_policy.metric = metric
    sign(genesis, Ed25519PrivateKey.generate())
    genesis_raw = genesis.SerializeToString()
    config = tmp_path/'profiles.json'
    config.write_text(json.dumps(dict(gateway_public='12'*32, profiles={'model':dict(
        profile_id='tiny', genesis=genesis_raw.hex(), tokenizer_version='pinned')})))
    def tokenize(prompt, **kwargs):
        return NS(input_ids=[NS(tolist=lambda: [1, 2] if prompt == 'fixture' else [3, 2])])
    agent = NS(genesis_bytes=genesis_raw, engine=NS(tokenizer=tokenize, tokenizer_version='pinned'),
        chain_args=NS(state_dir=str(tmp_path), spot_profile_config=str(config)))
    frame = NS(request_id=request_id)
    hold = dict(id=request_id, model_id='model',
        receipt_ref=hashlib.sha256(b'ember/pipeline-receipt/v1\0' + raw).hexdigest(),
        pipeline=dict(receipt=base64.b64encode(raw).decode(),
        spot_step_count=2, commitment_root=base64.b64encode(bytes.fromhex(manifest['commitment_root'])).decode(),
        spot=dict(request_id=request_id, step_set=[1], round='first', challenge_height=10, deadline=40)))
    def query(path):
        return {'/state': dict(version=14), '/model/model':dict(signed_profile=base64.b64encode(genesis_raw).decode()),
                '/parameters':dict(audit_equality=dict(enabled=True, epsilon=1.0))}[path]
    return agent, frame, job, hold, query, packages


def test_derivation_context_for_binding_and_manual_regression(tmp_path):
    agent, frame, job, hold, query, packages = setup(tmp_path)
    context = context_for(agent, frame, hold, job, query)
    assert context['request_tokens'] == [1, 2]
    assert context['claimed_tokens'] == [7, 8]
    assert context['epsilon'] == 1.0  # header tolerance cannot win
    assert verify_pipeline_binding(packages[0], context).token == 7
    assert verify_pipeline_binding(packages[1], context).token == 8
    # Existing archive path requires no new configuration, job, or chain calls.
    del agent.chain_args.spot_profile_config
    assert context_for(agent, frame, hold) == context


def test_claimed_token_tamper_fails_before_write(tmp_path, caplog):
    from auditor.spot_worker import execute
    agent, frame, job, hold, query, _ = setup(tmp_path)
    agent.node_id = 'auditor'
    hold['auditor'] = hold['pipeline']['spot']['auditor'] = agent.node_id
    job['openings'][0]['token'] ^= 1
    # Execute has no key or replay engine available: it must stop before verdict handling.
    with pytest.raises(ContextDerivationError, match='output commitment mismatch'):
        execute(agent, frame, job, hold, query)
    assert not (tmp_path/'spot-context').exists()
    assert not (tmp_path/'spot-evidence').exists()
    assert 'spot context derivation failed' in caplog.text


def test_prompt_tamper_caught_at_existing_binding_boundary(tmp_path):
    agent, frame, job, hold, query, packages = setup(tmp_path)
    job['prompt'] = 'tampered'
    context = context_for(agent, frame, hold, job, query)
    assert context['request_tokens'] == [3, 2]
    with pytest.raises(BindingFailure, match='trusted token prefix mismatch'):
        verify_pipeline_binding(packages[0], context)


@pytest.mark.parametrize('field', ['prompt', 'seed', 'metric', 'tolerance', 'pipeline_receipt',
                                  'openings', 'first_invocation_request'])
def test_missing_header_fails_without_context(tmp_path, field):
    agent, frame, job, hold, query, _ = setup(tmp_path)
    del job[field]
    with pytest.raises(ContextDerivationError):
        context_for(agent, frame, hold, job, query)
    assert not (tmp_path/'spot-context').exists()


@pytest.mark.parametrize('change', ['identity', 'receipt', 'tokenizer', 'model', 'count', 'epsilon'])
def test_other_anchor_mismatches_fail_closed(tmp_path, change):
    agent, frame, job, hold, query, _ = setup(tmp_path)
    if change == 'identity':
        req = ShardForwardRequest.FromString(bytes.fromhex(job['first_invocation_request']))
        req.request_id = 'other'
        job['first_invocation_request'] = req.SerializeToString().hex()
    elif change == 'receipt':
        job['pipeline_receipt'] += '00'
    elif change == 'tokenizer':
        agent.engine.tokenizer_version = 'changed'
    elif change == 'model':
        original = query
        query = lambda path: dict(signed_profile=base64.b64encode(b'other').decode()) if path.startswith('/model/') else original(path)
    elif change == 'count':
        hold['pipeline']['spot_step_count'] = 3
    else:
        original = query
        query = lambda path: dict(audit_equality=dict(enabled=True, epsilon=float('nan'))) if path == '/parameters' else original(path)
    with pytest.raises(ContextDerivationError):
        context_for(agent, frame, hold, job, query)
    assert not (tmp_path/'spot-context').exists()


def test_legacy_model_record_derives_via_publication_genesis(tmp_path):
    agent, frame, job, hold, query, packages = setup(tmp_path)
    def legacy_query(path):
        if path.startswith('/model/'):
            return {'publication': {'genesis': base64.b64encode(agent.genesis_bytes).decode()}}
        return query(path)
    context = context_for(agent, frame, hold, job, legacy_query)
    assert context['request_tokens'] == [1, 2]
    assert verify_pipeline_binding(packages[0], context).token == 7


def test_empty_signed_profile_falls_back_to_publication_genesis(tmp_path):
    agent, frame, job, hold, query, packages = setup(tmp_path)
    def legacy_query(path):
        if path.startswith('/model/'):
            return {'signed_profile': '',
                    'publication': {'genesis': base64.b64encode(agent.genesis_bytes).decode()}}
        return query(path)
    context = context_for(agent, frame, hold, job, legacy_query)
    assert verify_pipeline_binding(packages[1], context).token == 8


def test_legacy_publication_genesis_mismatch_fails_closed(tmp_path):
    agent, frame, job, hold, query, _ = setup(tmp_path)
    def bad_query(path):
        if path.startswith('/model/'):
            return {'publication': {'genesis': base64.b64encode(b'other').decode()}}
        return query(path)
    with pytest.raises(ContextDerivationError):
        context_for(agent, frame, hold, job, bad_query)
    assert not (tmp_path/'spot-context').exists()


def test_production_genesis_fixture_is_relative_l2():
    # The pinned production genesis (sha256 of the exact bytes the auditor
    # clients pin locally) registers relative-l2-v1 in both policy and band.
    raw = (Path(__file__).parent/'auditor/testdata/production-genesis.pb').read_bytes()
    assert hashlib.sha256(raw).hexdigest() == \
        '00973f5dc58bcbe2bad729d164f8f6171f20ad68c4535930799aa70f7495b668'
    genesis = ChainGenesis.FromString(raw)
    assert genesis.chain_id == 'ember-qwen2.5-1.5b-unified-v1'
    assert genesis.numeric_audit_policy.metric == 'relative-l2-v1'
    assert genesis.numeric_audit_policy.range_band.metric == 'relative-l2-v1'


def test_relative_l2_legacy_era_genesis_derives(tmp_path):
    # Old-era combination that production hit: no signed_profile on the model
    # record (publication.genesis fallback) AND the relative-l2-v1 metric.
    agent, frame, job, hold, query, packages = setup(tmp_path, metric='relative-l2-v1')
    def legacy_query(path):
        if path.startswith('/model/'):
            return {'publication': {'genesis': base64.b64encode(agent.genesis_bytes).decode()}}
        return query(path)
    context = context_for(agent, frame, hold, job, legacy_query)
    assert context['metric'] == 'relative-l2-v1'
    assert verify_pipeline_binding(packages[0], context).token == 7


def test_unknown_metric_fails_closed(tmp_path):
    agent, frame, job, hold, query, _ = setup(tmp_path, metric='unknown-metric-v9')
    with pytest.raises(ContextDerivationError, match='unsupported registered metric'):
        context_for(agent, frame, hold, job, query)
    assert not (tmp_path/'spot-context').exists()
