"""Salted per-token Merkle commitments and encrypted evidence storage."""
import hashlib
import secrets
from pathlib import Path
from .crypto import canonical, seal, open_seal, digest, wrap, b64
from evidence import opening


def leaf(item):
    return hashlib.sha256(b'ember/token/v2\0' + canonical(item)).digest()


def parent(a, b):
    return hashlib.sha256(b'ember/merkle/v2\0' + a + b).digest()


def levels(items):
    if not items:
        raise ValueError('empty evidence')
    result = [[leaf(item) for item in items]]
    while len(result[-1]) > 1:
        row = result[-1]
        result.append([parent(row[i], row[min(i+1, len(row)-1)]) for i in range(0, len(row), 2)])
    return result


def commit(steps):
    items = []
    for index, step in enumerate(steps):
        if len(step.topk) != 16 or step.score_kind != 'raw-logits':
            raise ValueError('top-16 raw logits required')
        items.append(dict(index=index, salt=secrets.token_hex(32), step=opening(step)))
    return levels(items)[-1][0].hex(), items


def proofs(items, start, end):
    if not 0 <= start < end <= len(items):
        raise ValueError('invalid token window')
    tree = levels(items)
    result = []
    for i in range(start, end):
        index, path = i, []
        for row in tree[:-1]:
            path.append(row[min(index ^ 1, len(row)-1)].hex())
            index //= 2
        result.append(dict(item=items[i], path=path))
    return result


def verify(root, proof):
    value, index = leaf(proof['item']), proof['item']['index']
    for sibling in proof['path']:
        other = bytes.fromhex(sibling)
        value = parent(other, value) if index & 1 else parent(value, other)
        index //= 2
    return value.hex() == root


class BundleStore:
    """Only ciphertext on disk; deletion cannot erase copies held elsewhere."""
    def __init__(self, directory):
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True, mode=0o700)

    def put(self, key, request_id, bundle):
        blob = seal(key, canonical(bundle), (request_id + '/evidence').encode())
        ref = hashlib.sha256(blob).hexdigest()
        path = self.directory / ref
        path.write_bytes(blob)
        path.chmod(0o600)
        return ref

    def get(self, ref):
        if len(ref) != 64 or any(c not in '0123456789abcdef' for c in ref):
            raise ValueError('invalid bundle reference')
        return (self.directory / ref).read_bytes()

    def delete(self, ref, request_id, chain):
        if chain.request(request_id)['status'] != 'final':
            raise ValueError('not final')
        if chain.request(request_id)['bundle_ref'] != ref:
            raise ValueError('wrong bundle')
        self.get(ref)
        (self.directory / ref).unlink()
        chain.event('bundle_deleted', request_id=request_id, reference=ref)


def decrypt_bundle(key, request_id, ciphertext):
    import json
    return json.loads(open_seal(key, ciphertext, (request_id + '/evidence').encode()))


def dispute(bundle, envelope, start, end):
    if bundle.get('envelope_hash') != digest(envelope):
        raise ValueError('dispute envelope differs from committed bundle')
    if levels(bundle['items'])[-1][0].hex() != bundle['root']:
        raise ValueError('invalid bundle root')
    consent = envelope['dispute_consent']
    if not consent['enabled'] or not consent['arbitration_key']:
        raise ValueError('arbitration not pre-consented')
    from .crypto import unb64
    context = (envelope['request_id'] + '/dispute').encode()
    key = secrets.token_bytes(32)
    window = dict(request_id=envelope['request_id'], root=bundle['root'], start=start, end=end,
                  proofs=proofs(bundle['items'], start, end))
    return dict(wrapped_key=b64(wrap(unb64(consent['arbitration_key']), key, context)),
                ciphertext=b64(seal(key, canonical(window), context)))
