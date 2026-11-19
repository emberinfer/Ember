"""Ruling 10 record preimages, streamed grouping and durable disclosure."""
import copy
import json
from tempfile import TemporaryDirectory

import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from spotcheck import (OpeningService, canonical, pipeline_commitment,
                       pipeline_package, sha)


def fixture():
    from test_pipeline_audit import PipelineAuditTest
    test = PipelineAuditTest()
    test.setUp()
    job = test.job
    raws = [canonical(value) for value in job['openings']]
    manifest = pipeline_commitment('ab'*32, 'tiny', iter(job['invocations']), iter(raws), 2)
    return job, raws, manifest


def test_original_record_hash_composition():
    job, raws, manifest = fixture()
    for index, leaf in enumerate(manifest['leaves']):
        items = job['invocations'][index*2:index*2+2]
        ih = sha(b''.join(sha(bytes.fromhex(v['request'])) for v in items)).hex()
        oh = sha(b''.join(sha(bytes.fromhex(v['response'])) for v in items)+sha(raws[index])).hex()
        assert (ih, oh) == (leaf['input_hash'], leaf['output_hash'])
        assert leaf['step_index'] == index+1
    assert manifest['spot_step_count'] == 2
    # Original JSON whitespace is committed, not a reserialization.
    changed = pipeline_commitment('ab'*32, 'tiny', job['invocations'],
                                  [b' '+raws[0], raws[1]], 2)
    assert changed['commitment_root'] != manifest['commitment_root']


def test_n_and_step_coverage_fail_closed():
    job, raws, _ = fixture()
    for n in (0, 1, 3):
        with pytest.raises(ValueError):
            pipeline_commitment('ab'*32, 'tiny', job['invocations'], raws, n)
    with pytest.raises(ValueError):
        pipeline_commitment('ab'*32, 'tiny', job['invocations'][2:], raws, 2)
    with pytest.raises(ValueError):
        pipeline_commitment('ab'*32, 'tiny', job['invocations'], job['openings'], 2)


def test_durable_pipeline_disclosure_retry_and_gap():
    job, raws, manifest = fixture()
    with TemporaryDirectory() as directory:
        records = []
        service = OpeningService(directory, Ed25519PrivateKey.generate(), records.append)
        readers = dict(read_invocation=job['invocations'].__getitem__, read_opening=raws.__getitem__)
        receipt = service.open(manifest, [1], 1, 10, 11, **readers)
        package = records[-1]['packages'][0]
        assert 'tensors' not in package
        assert package['first_request'] == job['invocations'][0]['request']
        assert package['last_response'] == job['invocations'][1]['response']
        assert package['opening'] == raws[0].hex()
        assert len(package['request_hashes']) == len(package['response_hashes']) == 2
        assert package['next']['first_request'] == job['invocations'][2]['request']
        assert len(package['next']['request_hashes']) == 2
        assert 'last_response' not in package['next']
        assert receipt['chain_attestation']['received']
        service.open(manifest, [1], 2, 12, 13, **readers)
        assert records[-1]['packages'] == records[0]['packages']
        service.open(manifest, [1, 2], 1, 14, 15, escalation=True, **readers)
        assert records[-1]['steps'] == [2]
        assert 'next' not in records[-1]['packages'][0]
        # Metadata survived delivery and reload.
        saved = [json.loads(p.read_text()) for p in service.root.glob('*.json')]
        assert sum(len(r['packages']) for r in saved) == 3


def test_modified_spool_records_rejected():
    job, raws, manifest = fixture()
    altered = copy.deepcopy(job['invocations'])
    altered[0]['response'] += '00'
    with pytest.raises(ValueError, match='response changed'):
        pipeline_package(manifest, 1, altered.__getitem__, raws.__getitem__)
    with pytest.raises(ValueError, match='opening changed'):
        pipeline_package(manifest, 1, job['invocations'].__getitem__, [b'{}', raws[1]].__getitem__)
