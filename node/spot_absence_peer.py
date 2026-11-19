"""Consume a real Go-signed absence through C's ordinary unopened claim path."""
import json
import sys
import tempfile
from types import SimpleNamespace as NS
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from auditor.spot import unopened_claim

job=json.load(sys.stdin)
att=job['attestation']
context=dict(request_id=att['request_id'],challenge_height=att['challenge_height'],steps=job['steps'],
             open_deadline=job['deadline'],round='1',challenge_record=job['challenge'])
spool=NS(absence=lambda *_:att,get=lambda *_:None)
claim=unopened_claim(context,spool,Ed25519PrivateKey.generate(),bytes.fromhex(job['gateway_public']),att['height'])
print(json.dumps(dict(type='spot_unopened',hold_id=context['request_id'],spool=claim['attestation'])))
