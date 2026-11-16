"""Shamir escrow over the Mersenne prime 2**521-1 (default 3-of-5).

Custodians store only encrypted shares and release encrypted shares to the
registered auditor after checking finalized chain state and Ed25519 possession.
"""
import hashlib
import json
import secrets
from pathlib import Path
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
from .crypto import canonical, public, wrap, unwrap, b64, unb64

PRIME = 2**521 - 1


def split(key, threshold=3, count=5):
    if len(key) != 32 or not 2 <= threshold <= count <= 255:
        raise ValueError('invalid Shamir parameters')
    coefficients = [int.from_bytes(key, 'big')] + [secrets.randbelow(PRIME) for _ in range(threshold-1)]
    return [dict(x=x, y=str(sum(c * pow(x, i, PRIME) for i, c in enumerate(coefficients)) % PRIME),
                 threshold=threshold) for x in range(1, count+1)]


def reconstruct(shares, threshold):
    if len(shares) < threshold or threshold < 2:
        raise ValueError('below threshold')
    if any(s['threshold'] != threshold for s in shares):
        raise ValueError('threshold mismatch')
    xs = [s['x'] for s in shares]
    if len(set(xs)) != len(xs) or any(type(x) is not int or not 1 <= x <= 255 for x in xs):
        raise ValueError('invalid share indices')
    result = 0
    for s in shares:
        x, y, weight = s['x'], int(s['y']), 1
        if not 0 <= y < PRIME:
            raise ValueError('invalid field element')
        for other in xs:
            if x != other:
                weight = weight * (-other) * pow(x-other, -1, PRIME) % PRIME
        result = (result + y * weight) % PRIME
    if result >= 2**256:
        raise ValueError('inconsistent shares')
    return result.to_bytes(32, 'big')


def assignment_proof(chain_id, record):
    return hashlib.sha256(canonical(['ember/privacy/assignment/v2', chain_id, record['id'],
        record['epoch'], record['auditor'], record['root'], record['envelope_hash']])).hexdigest()


def identity(public_key):
    # Mirrors ember core NodeID.
    return hashlib.sha256(public_key).hexdigest()


def release_request(chain, request_id, auditor_key):
    record = chain.request(request_id)
    message = dict(request_id=request_id, epoch=record['epoch'],
        proof=assignment_proof(chain.chain_id, record), nonce=secrets.token_hex(32))
    return dict(message=message, public_key=b64(public(auditor_key)),
                signature=b64(auditor_key.sign(b'ember/escrow-release/v2\0' + canonical(message))))


class Custodian:
    def __init__(self, chain, epoch, key, directory):
        self.chain, self.epoch, self.key = chain, epoch, key
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True, mode=0o700)

    def path(self, request_id):
        return self.directory / hashlib.sha256((self.epoch + '/' + request_id).encode()).hexdigest()

    def receive(self, request_id, ciphertext):
        record = self.chain.request(request_id)
        if record['epoch'] != self.epoch or self.chain.epoch(self.epoch)['ended']:
            raise ValueError('inactive epoch')
        share = json.loads(unwrap(self.key, ciphertext, (request_id + '/' + self.epoch).encode()))
        if share['threshold'] != self.chain.epoch(self.epoch)['threshold']:
            raise ValueError('wrong threshold')
        path = self.path(request_id)
        if path.exists():
            raise ValueError('share already deposited')
        path.write_bytes(ciphertext); path.chmod(0o600)

    def release(self, request):
        message = request.get('message', {}) if isinstance(request,dict) else {}
        message = message if isinstance(message,dict) else {}
        request_id = message.get('request_id', '')
        request_id = request_id if isinstance(request_id,str) else ''
        try:
            record = self.chain.request(request_id)
            epoch = self.chain.epoch(self.epoch)
            if epoch['ended'] or message.get('epoch') != self.epoch or record['epoch'] != self.epoch:
                raise ValueError('inactive or wrong epoch')
            if record['status'] != 'assigned' or not record['auditor']:
                raise ValueError('no finalized assignment')
            if message.get('proof') != assignment_proof(self.chain.chain_id, record):
                raise ValueError('invalid assignment proof')
            pub = unb64(request['public_key'])
            if identity(pub) != record['auditor']:
                raise ValueError('not assigned auditor')
            Ed25519PublicKey.from_public_bytes(pub).verify(unb64(request['signature']),
                b'ember/escrow-release/v2\0' + canonical(message))
            share = unwrap(self.key, self.path(request_id).read_bytes(), (request_id + '/' + self.epoch).encode())
            target = unb64(self.chain.operator(record['auditor'])['wrap_key'])
        except Exception as error:
            # Log a bounded reason, never exception strings containing caller data.
            self.chain.event('share_refusal', request_id=request_id, epoch=self.epoch, reason='unauthorized_or_unavailable')
            raise ValueError('share release refused') from error
        # Fail closed if the on-chain release log cannot be committed.
        self.chain.event('share_release', request_id=request_id, epoch=self.epoch)
        return wrap(target, share, (request_id + '/auditor/' + self.epoch).encode())

    def destroy(self):
        if not self.chain.epoch(self.epoch)['ended']:
            raise ValueError('epoch still active')
        for path in self.directory.iterdir():
            if path.is_file() and len(path.name)==64 and all(c in '0123456789abcdef' for c in path.name):
                path.unlink()
        self.chain.event('shares_destroyed', epoch=self.epoch)
