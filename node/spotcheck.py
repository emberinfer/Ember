"""v14 canonical commitments and durable opening service (no model execution)."""
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import struct
import time
import uuid

SPOT_OPEN_ROUND_BLOCKS = 120
SPOT_FIRST_VERDICT_BLOCKS = 60
SPOT_SUBSTITUTE_BLOCKS = 120
SPOT_ESCALATION_BLOCKS = 350
DTYPES = {'f32': (1, 4), 'f64': (2, 8), 'bf16': (3, 2), 'f16': (4, 2), 'i64': (5, 8), 'i32': (6, 4)}


def sha(data):
    return hashlib.sha256(data).digest()


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()


def tensor_header(dtype, shape):
    code, width = DTYPES[dtype]
    if len(shape) > 255 or any(type(n) is not int or n < 0 for n in shape):
        raise ValueError('invalid tensor shape')
    return b'ETV1' + bytes((code, len(shape))) + b''.join(struct.pack('>Q', n) for n in shape)


def tensor_hash(path, dtype, shape):
    h = hashlib.sha256(tensor_header(dtype, shape))
    size = DTYPES[dtype][1]
    for n in shape:
        size *= n
    seen = 0
    with open(path, 'rb') as source:
        for chunk in iter(lambda: source.read(65536), b''):
            h.update(chunk)
            seen += len(chunk)
    if seen != size:
        raise ValueError('tensor payload size mismatch')
    return h.hexdigest()


def token_hash(tokens):
    return sha(tensor_header('i64', [len(tokens)]) + b''.join(struct.pack('<q', t) for t in tokens)).hex()


def leaf_hash(leaf):
    fields = [bytes.fromhex(leaf[k]) for k in ('input_hash', 'output_hash', 'format_hash', 'profile_id_hash', 'request_id')]
    if any(len(v) != 32 for v in fields) or leaf['step_index'] < 1:
        raise ValueError('invalid leaf')
    return sha(b'\0EMBER-SPOTLEAF-V1' + struct.pack('>Q', leaf['step_index']) + b''.join(fields))


def tree(leaves):
    if not leaves:
        raise ValueError('empty commitment')
    levels = [[leaf_hash(v) for v in leaves]]
    while len(levels[-1]) > 1:
        row = levels[-1]
        levels.append([sha(b'\1' + row[i] + row[min(i+1, len(row)-1)]) for i in range(0, len(row), 2)])
    return levels


def proof(levels, index):
    result = []
    for row in levels[:-1]:
        result.append({'hash': row[min(index ^ 1, len(row)-1)].hex(), 'left': bool(index & 1)})
        index //= 2
    return result


def verify_proof(leaf, siblings, root):
    value = leaf_hash(leaf)
    for sibling in siblings:
        other = bytes.fromhex(sibling['hash'])
        if len(other) != 32:
            return False
        value = sha(b'\1' + (other + value if sibling['left'] else value + other))
    return value.hex() == root


def durable_json(path, value):
    path = Path(path)
    temporary = path.with_name(path.name + '.' + uuid.uuid4().hex + '.tmp')
    with temporary.open('wb') as out:
        out.write(canonical(value))
        out.flush()
        os.fsync(out.fileno())
    os.replace(temporary, path)
    fd = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


class Commitment:
    """Only leaf metadata stays resident; payloads remain in caller-owned spool."""
    def __init__(self, request_id, profile_id, input_tokens):
        if len(bytes.fromhex(request_id)) != 32:
            raise ValueError('request ID must be 32 bytes')
        self.request_id = request_id
        self.profile_id = sha(profile_id.encode()).hex()
        self.first = token_hash(input_tokens)
        self.leaves = []
        self.payloads = []

    def append(self, input_path, input_dtype, input_shape, output_path, output_dtype, output_shape):
        incoming = tensor_hash(input_path, input_dtype, input_shape)
        outgoing = tensor_hash(output_path, output_dtype, output_shape)
        expected = self.leaves[-1]['output_hash'] if self.leaves else self.first
        if incoming != expected:
            raise ValueError('boundary chain broken')
        leaf = dict(step_index=len(self.leaves)+1, input_hash=incoming, output_hash=outgoing,
                    format_hash=sha(f'{output_dtype}|{",".join(map(str, output_shape))}|raw-le-v1'.encode()).hex(),
                    profile_id_hash=self.profile_id, request_id=self.request_id)
        self.leaves.append(leaf)
        self.payloads.append(dict(input=dict(path=str(Path(input_path).resolve()), dtype=input_dtype, shape=input_shape),
                                  output=dict(path=str(Path(output_path).resolve()), dtype=output_dtype, shape=output_shape)))

    def finish(self, output_tokens, expected_steps=62):
        if len(self.leaves) != expected_steps or self.leaves[-1]['output_hash'] != token_hash(output_tokens):
            raise ValueError('step count or terminal anchor mismatch')
        return dict(request_id=self.request_id, commitment_root=tree(self.leaves)[-1][0].hex(),
                    input_token_hash=self.first, output_token_hash=token_hash(output_tokens),
                    leaves=self.leaves, payloads=self.payloads)


class OpeningService:
    """signer is the registered gateway Ed25519 key; deliver consumes paths streaming.

    All state transitions are serialized by a filesystem lock, including negative
    attestations, so a concurrent opening cannot race the absence query.
    """
    def __init__(self, root, signer, deliver):
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)
        self.signer, self.deliver = signer, deliver

    def signed(self, value):
        from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
        return dict(payload=value, signer=self.signer.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw).hex(),
                    signature=self.signer.sign(b'EMBER-SPOOL-V1\0' + canonical(value)).hex())

    def chain_attestation(self, request_id, challenge_height, steps, received, height):
        """A's exact Go struct order, JSON escaping, and base64 wire contract."""
        from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
        from ember_core import node_id
        public = self.signer.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
        value = dict(request_id=request_id, challenge_height=challenge_height,
                     step_set_hash=base64.b64encode(sha(b''.join(
                         struct.pack('>Q', i) for i in sorted(set(steps))))).decode(),
                     gateway=node_id(public), received=received, height=height)
        raw = json.dumps(value, separators=(',', ':'), ensure_ascii=False)
        for a, b in (('&', r'\u0026'), ('<', r'\u003c'), ('>', r'\u003e'),
                     ('\u2028', r'\u2028'), ('\u2029', r'\u2029')):
            raw = raw.replace(a, b)
        value['signature'] = base64.b64encode(self.signer.sign(
            b'EMBER-SPOT-SPOOL-V1\0' + raw.encode())).decode()
        return value

    def _locked(self):
        import contextlib
        import fcntl
        @contextlib.contextmanager
        def lock():
            with (self.root / '.lock').open('a') as handle:
                fcntl.flock(handle, fcntl.LOCK_EX)
                yield
        return lock()

    def _records(self):
        return [json.loads(p.read_text()) for p in self.root.glob('*.json')]

    def open(self, manifest, steps, round, challenge_height, height, escalation=False):
        with self._locked():
            rid = manifest['request_id']
            leaves = manifest['leaves']
            levels = tree(leaves)
            if levels[-1][0].hex() != manifest['commitment_root']:
                raise ValueError('manifest commitment mismatch')
            if (leaves[0]['input_hash'] != manifest['input_token_hash'] or
                    leaves[-1]['output_hash'] != manifest['output_token_hash'] or
                    any(leaf['request_id'] != rid or leaf['step_index'] != i+1
                        for i, leaf in enumerate(leaves)) or
                    any(a['output_hash'] != b['input_hash'] for a, b in zip(leaves, leaves[1:]))):
                raise ValueError('manifest boundary chain mismatch')
            steps = sorted(set(steps))
            if not steps or any(type(i) is not int or not 1 <= i <= len(leaves) for i in steps) or round not in (1, 2):
                raise ValueError('invalid challenge')
            prior = [r for r in self._records() if r.get('kind') == 'opening' and r['request_id'] == rid]
            if round == 2 and not escalation and not any(r['steps'] == steps and r['round'] == 1 for r in prior):
                raise ValueError('retry must reuse first challenge set')
            if height < challenge_height:
                raise ValueError('opening precedes challenge')
            if escalation:
                opened = {i for r in prior for i in r['steps']}
                steps = [i for i in range(1, len(leaves)+1) if i not in opened]
            record_id = uuid.uuid4().hex
            directory = self.root / record_id
            directory.mkdir()
            packages = []
            for i in steps:
                neighbor = i+1 if i < len(leaves) else max(1, i-1)
                package = dict(step_index=i, leaf=leaves[i-1], proof=proof(levels, i-1),
                               neighbor_leaf=leaves[neighbor-1], neighbor_proof=proof(levels, neighbor-1), leaf_count=len(leaves), tensors={})
                for name, tensor in manifest['payloads'][i-1].items():
                    if tensor_hash(tensor['path'], tensor['dtype'], tensor['shape']) != leaves[i-1][name+'_hash']:
                        raise ValueError('spool tensor changed')
                    target = directory / f'{i}-{name}.raw'
                    with open(tensor['path'], 'rb') as source, target.open('wb') as out:
                        shutil.copyfileobj(source, out, 65536)
                        out.flush()
                        os.fsync(out.fileno())
                    package['tensors'][name] = dict(tensor, path=str(target.resolve()))
                packages.append(package)
            fd = os.open(directory, os.O_RDONLY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
            receipt = self.signed(dict(kind='receipt', request_id=rid, challenge_height=challenge_height,
                step_set_hash=sha(b''.join(struct.pack('>Q', i) for i in steps)).hex(),
                spool_record_id=record_id, height=height, timestamp=time.time_ns(), round=round))
            receipt['chain_attestation'] = self.chain_attestation(rid, challenge_height, steps, True, height)
            record = dict(kind='opening', request_id=rid, challenge_height=challenge_height, height=height,
                          steps=steps, round=round, receipt=receipt, packages=packages, delivered=False)
            durable_json(self.root / (record_id+'.json'), record)
        # Persist before transport; failed deliveries remain available for retry.
        self.deliver(record)
        with self._locked():
            record['delivered'] = True
            durable_json(self.root / (record_id+'.json'), record)
        return receipt

    def attest_absence(self, request_id, challenge_height, steps, deadline, height):
        with self._locked():
            if height <= deadline or deadline < challenge_height:
                raise ValueError('deadline not finalized')
            requested = set(steps)
            if not requested or any(type(i) is not int or i < 1 for i in requested):
                raise ValueError('invalid challenge steps')
            covered = set()
            for r in self._records():
                if (r.get('kind') == 'opening' and r['request_id'] == request_id and
                        r['challenge_height'] == challenge_height and challenge_height <= r['height'] <= deadline):
                    covered.update(r['steps'])
            if requested & covered:
                raise ValueError('corresponding opening exists; cannot attest absence')
            value = self.signed(dict(kind='absence', request_id=request_id, challenge_height=challenge_height,
                                    steps=sorted(requested), deadline=deadline, height=height))
            value['chain_attestation'] = self.chain_attestation(request_id, challenge_height, sorted(requested), False, height)
            durable_json(self.root / (uuid.uuid4().hex+'.json'), value)
            return value

    def receipt(self, record_id):
        if len(record_id) != 32 or any(c not in '0123456789abcdef' for c in record_id):
            raise ValueError('invalid record ID')
        with self._locked():
            return json.loads((self.root / (record_id+'.json')).read_text())['receipt']

    def retry_delivery(self, record_id):
        self.receipt(record_id)  # Validate before constructing a path.
        with self._locked():
            record = json.loads((self.root / (record_id+'.json')).read_text())
        self.deliver(record)
        with self._locked():
            record['delivered'] = True
            durable_json(self.root / (record_id+'.json'), record)
        return record['receipt']

    def cleanup(self, manifest, lottery_final=False, selected=None, settled=False):
        if not (settled or lottery_final and selected is False):
            return False
        with self._locked():
            for pair in manifest['payloads']:
                for tensor in pair.values():
                    Path(tensor['path']).unlink(missing_ok=True)
            for record in self._records():
                if record.get('kind') == 'opening' and record['request_id'] == manifest['request_id']:
                    for package in record['packages']:
                        for tensor in package['tensors'].values():
                            Path(tensor['path']).unlink(missing_ok=True)
            durable_json(self.root / (uuid.uuid4().hex+'.json'),
                         dict(kind='deletion', request_id=manifest['request_id'], timestamp=time.time_ns()))
        return True


def rank_candidates(profiles, lookup, warn):
    ranked = []
    for index, profile in enumerate(profiles):
        try:
            level = lookup(profile.node_id)
            if level not in (0, 1, 2):
                raise ValueError('invalid availability level')
        except Exception as exc:
            warn(f'availability lookup failed for {profile.node_id}: {exc}')
            level = 0
        if level != 2:
            ranked.append((level, index, profile))
    return [p for _, _, p in sorted(ranked, key=lambda item: item[:2])]


class FailureJournal:
    """One durable row per task opportunity/outcome; offline tools aggregate by UTC day/node."""
    def __init__(self, root):
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)

    def record(self, node_id, request_id, outcome):
        if outcome not in ('opportunity', 'task_failed', 'node_offline', 'heartbeat_missing'):
            raise ValueError('unknown baseline event')
        durable_json(self.root / (uuid.uuid4().hex+'.json'), dict(node_id=node_id,
            request_id=request_id, outcome=outcome, day=time.strftime('%Y-%m-%d', time.gmtime()),
            timestamp=time.time_ns(), challenge=False))


def verify_signed(envelope, registered_public_key):
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    if envelope['signer'] != registered_public_key.hex():
        raise ValueError('unregistered gateway signer')
    Ed25519PublicKey.from_public_bytes(registered_public_key).verify(
        bytes.fromhex(envelope['signature']), b'EMBER-SPOOL-V1\0' + canonical(envelope['payload']))
    return envelope['payload']
