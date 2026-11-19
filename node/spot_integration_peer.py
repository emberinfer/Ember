"""Synthetic cross-language spool contract peer; no weights or production I/O."""
import json
import sys
import tempfile
from types import SimpleNamespace as NS
from unittest.mock import patch
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from spotcheck import OpeningService
from auditor.spot_worker import unopened_action


def main():
    job = json.load(sys.stdin)
    key = Ed25519PrivateKey.from_private_bytes(bytes.fromhex(job['seed']))
    from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
    public = key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    with tempfile.TemporaryDirectory() as root:
        service = OpeningService(root, key, lambda record: None)
        detailed = service.attest_absence(job['request_id'], job['challenge_height'],
            job['steps'], job['deadline'], job['height'])
        att = detailed['chain_attestation']
        context = dict(request_id=job['request_id'], challenge_height=job['challenge_height'],
            steps=job['steps'], open_deadline=job['deadline'], round='1', challenge_record=job)
        spool = NS(absence=lambda *_: att, get=lambda *_: None)
        agent = NS(key=key, chain_args=NS(state_dir=root))
        with patch('auditor.spot_worker.envelope_result', side_effect=lambda a, action, summary: action):
            action = unopened_action(agent, context, spool, public, att, job['height'])
        receipt = service.chain_attestation(job['request_id'], job['challenge_height'], job['steps'], True, job['deadline'])
        print(json.dumps(dict(action=action, receipt=receipt)))


if __name__ == '__main__':
    main()
