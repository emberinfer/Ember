import copy
import json
from pathlib import Path
import struct
from types import SimpleNamespace

import pytest
from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat

from spotcheck import (Commitment, OpeningService, FailureJournal, leaf_hash, tree, proof,
                       verify_proof, verify_signed, rank_candidates, token_hash)


@pytest.fixture
def manifest(tmp_path):
    paths = []
    for i in range(4):
        path = tmp_path / f'tensor-{i}'
        path.write_bytes(struct.pack('<q', i))
        paths.append(path)
    builder = Commitment('ab'*32, 'tiny', [0])
    for a, b in zip(paths, paths[1:]):
        builder.append(a, 'i64', [1], b, 'i64', [1])
    return builder.finish([3], expected_steps=3)


def test_golden(manifest):
    assert token_hash([0]) == 'e1809e1325db70c9d6577fd2ccee9c1acf49ebbecbc520d2643d42b83bed560c'
    assert manifest['commitment_root'] == '2ec568ee651b93e9ab89db6d0f15bf0f447362bc9eef77000941f332dc6fc63f'
    levels = tree(manifest['leaves'])
    for i, leaf in enumerate(manifest['leaves']):
        assert verify_proof(leaf, proof(levels, i), manifest['commitment_root'])
    altered = copy.deepcopy(manifest['leaves'][0])
    altered['request_id'] = 'ff'*32
    assert not verify_proof(altered, proof(levels, 0), manifest['commitment_root'])


def test_broken_chain_and_anchor(tmp_path):
    p = tmp_path / 'x'
    p.write_bytes(struct.pack('<q', 1))
    b = Commitment('ab'*32, 'tiny', [0])
    with pytest.raises(ValueError, match='boundary'):
        b.append(p, 'i64', [1], p, 'i64', [1])
    b = Commitment('ab'*32, 'tiny', [1])
    b.append(p, 'i64', [1], p, 'i64', [1])
    with pytest.raises(ValueError, match='anchor'):
        b.finish([2], 1)


@pytest.fixture
def service(tmp_path):
    key = Ed25519PrivateKey.generate()
    deliveries = []
    return OpeningService(tmp_path / 'spool', key, deliveries.append), deliveries, key


@pytest.mark.parametrize('step,neighbor', [(1, 2), (2, 3), (3, 2)])
def test_openings(manifest, service, step, neighbor):
    spool, deliveries, key = service
    receipt = spool.open(manifest, [step], 1, 10, 11)
    public = key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    assert verify_signed(receipt, public)['height'] == 11
    package = deliveries[0]['packages'][0]
    assert package['neighbor_leaf']['step_index'] == neighbor
    assert verify_proof(package['leaf'], package['proof'], manifest['commitment_root'])
    assert verify_proof(package['neighbor_leaf'], package['neighbor_proof'], manifest['commitment_root'])
    assert Path(package['tensors']['input']['path']).read_bytes() == struct.pack('<q', step-1)
    bad = copy.deepcopy(receipt)
    bad['payload']['height'] += 1
    with pytest.raises(InvalidSignature):
        verify_signed(bad, public)


def test_retry_escalation_restart_and_absence(manifest, service):
    spool, deliveries, key = service
    spool.open(manifest, [2], 1, 10, 11)
    with pytest.raises(ValueError, match='reuse'):
        spool.open(manifest, [1], 2, 10, 12)
    spool.open(manifest, [2], 2, 10, 12)
    assert len(deliveries) == 2
    spool = OpeningService(spool.root, key, deliveries.append)
    spool.open(manifest, [2], 1, 10, 13, escalation=True)
    assert deliveries[-1]['steps'] == [1, 3]
    with pytest.raises(ValueError, match='exists'):
        spool.attest_absence(manifest['request_id'], 10, [2], 130, 131)
    with pytest.raises(ValueError, match='finalized'):
        spool.attest_absence('cd'*32, 10, [2], 130, 130)
    assert spool.attest_absence('cd'*32, 10, [2], 130, 131)['payload']['kind'] == 'absence'


def test_retention(manifest, service):
    spool, _, _ = service
    for opts in ({}, dict(lottery_final=True, selected=True), dict(selected=False)):
        assert not spool.cleanup(manifest, **opts)
    assert spool.cleanup(manifest, lottery_final=True, selected=False)
    assert all(not Path(t['path']).exists() for pair in manifest['payloads'] for t in pair.values())


def test_settlement_retention(manifest, service):
    assert service[0].cleanup(manifest, settled=True)


def test_transport_failure_is_durable(manifest, service):
    spool, _, _ = service
    def fail(_):
        raise OSError('offline')
    spool.deliver = fail
    with pytest.raises(OSError):
        spool.open(manifest, [1], 1, 10, 11)
    records = spool._records()
    assert len(records) == 1 and not records[0]['delivered']
    with pytest.raises(ValueError, match='exists'):
        spool.attest_absence(manifest['request_id'], 10, [1], 130, 131)


def test_availability_and_failure_journal(tmp_path):
    profiles = [SimpleNamespace(node_id=i) for i in range(5)]
    def lookup(i):
        if i == 4:
            raise OSError('query down')
        return [1, 0, 2, 1][i]
    warnings = []
    assert [p.node_id for p in rank_candidates(profiles, lookup, warnings.append)] == [1, 4, 0, 3]
    assert len(warnings) == 1
    journal = FailureJournal(tmp_path)
    journal.record('node', 'request', 'opportunity')
    journal.record('node', 'request', 'task_failed')
    assert sorted(json.loads(p.read_text())['outcome'] for p in tmp_path.glob('*.json')) == ['opportunity', 'task_failed']


def test_streaming_hash_memory(tmp_path):
    import tracemalloc
    from spotcheck import tensor_hash
    path = tmp_path / 'large'
    with path.open('wb') as out:
        for _ in range(128):
            out.write(b'\x00' * 65536)
    tracemalloc.start()
    try:
        tensor_hash(path, 'i64', [1048576])
        assert tracemalloc.get_traced_memory()[1] < 512*1024
    finally:
        tracemalloc.stop()


def test_recovery_receipt_delivery(manifest, service):
    spool, deliveries, key = service
    receipt = spool.open(manifest, [1], 1, 10, 11)
    record_id = receipt['payload']['spool_record_id']
    recovered = OpeningService(spool.root, key, deliveries.append)
    assert recovered.receipt(record_id) == receipt
    assert recovered.retry_delivery(record_id) == receipt
    assert len(deliveries) == 2
    assert recovered.cleanup(manifest, settled=True)
    assert not Path(deliveries[0]['packages'][0]['tensors']['input']['path']).exists()
