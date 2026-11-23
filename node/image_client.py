"""Image execution/audit transport boundary. Network I/O is injected by the host.

All roots, requests, thresholds and settlement heights must be authenticated by
that host from chain state. Never accept them from an evidence package.
"""
import base64
import hashlib
import io
import json
import math
import os
from pathlib import Path
import shutil
import struct
import time

from engine.adapters.diffusers_t2i import Formula, sha, validate_request

ZERO = '00'*32


def canonical(value):
    return json.dumps(value, separators=(',', ':'), ensure_ascii=False).encode()


def durable(path, raw):
    path = Path(path)
    temporary = path.with_name(path.name + '.tmp')
    with temporary.open('wb') as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
    fd = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def prompt_anchor(tokens):
    return sha(b'EMBER-IMAGEPROMPT-V1\0' + b''.join(struct.pack('<I', t) for seq in tokens for t in seq))


def leaf_hash(leaf):
    raw = b'\0EMBER-SPOTLEAF-V1' + struct.pack('>Q', leaf['step_index'])
    raw += b''.join(bytes.fromhex(leaf[k]) for k in ('input_hash', 'output_hash', 'format_hash', 'profile_id_hash', 'request_id'))
    return hashlib.sha256(b'EMBER-IMAGELEAF-V1\0' + hashlib.sha256(raw).digest()
                          + bytes.fromhex(leaf['anchor_hash'])).digest()


def tree(leaves):
    levels = [[leaf_hash(l) for l in leaves]]
    if not leaves:
        raise ValueError('empty tree')
    while len(levels[-1]) > 1:
        a = levels[-1]
        levels.append([hashlib.sha256(b'\1'+a[i]+a[min(i+1, len(a)-1)]).digest()
                       for i in range(0, len(a), 2)])
    proofs = []
    for i in range(len(leaves)):
        proof, j = [], i
        for level in levels[:-1]:
            proof.append({'hash': level[min(j ^ 1, len(level)-1)].hex(), 'left': bool(j % 2)})
            j //= 2
        proofs.append(proof)
    return levels[-1][0].hex(), proofs


def verify(leaf, proof, root, count):
    i, value = leaf['step_index'], leaf_hash(leaf)
    if type(i) is not int or not 0 <= i < count:
        raise ValueError('binding failure: leaf index')
    for node in proof:
        sibling = bytes.fromhex(node['hash'])
        if count <= 1 or type(node['left']) is not bool or node['left'] != bool(i % 2) or len(sibling) != 32:
            raise ValueError('binding failure: proof direction/length')
        if i == count-1 and count % 2 and sibling != value:
            raise ValueError('binding failure: odd node')
        value = hashlib.sha256(b'\1' + (sibling+value if i % 2 else value+sibling)).digest()
        count, i = (count+1)//2, i//2
    if count != 1 or value.hex() != root:
        raise ValueError('binding failure: Merkle root')


def relative_l2(actual, reference):
    import numpy as np
    a, b = np.asarray(actual, dtype='float64'), np.asarray(reference, dtype='float64')
    if a.shape != b.shape or not np.isfinite(a).all() or not np.isfinite(b).all():
        raise ValueError('invalid comparison')
    return float(np.linalg.norm((a-b).ravel()) / max(np.linalg.norm(b.ravel()), 1e-12))


def format_hash(shape):
    return sha(('f32|' + ','.join(map(str, shape)) + '|raw-le-v1').encode())


def wire_leaf(l):
    # Go SpotLeaf fields intentionally use exported names (no JSON tags).
    keys = ('Step', 'InputHash', 'OutputHash', 'FormatHash', 'ProfileIDHash', 'RequestID')
    vals = [l['step_index']] + [list(bytes.fromhex(l[k])) for k in
            ('input_hash', 'output_hash', 'format_hash', 'profile_id_hash', 'request_id')]
    return {'leaf': dict(zip(keys, vals)), 'anchor_hash': list(bytes.fromhex(l['anchor_hash']))}


class ImageClient:
    def __init__(self, engine, evidence_dir, safety_seconds):
        if not math.isfinite(safety_seconds) or safety_seconds < 0:
            raise ValueError('negative retention margin')
        self.engine, self.root, self.margin = engine, Path(evidence_dir), safety_seconds
        self.root.mkdir(parents=True, exist_ok=True)

    def directory(self, request_id):
        if (not isinstance(request_id, str) or len(request_id) != 64
                or any(c not in '0123456789abcdef' for c in request_id)):
            raise ValueError('invalid request ID')
        return self.root/request_id

    def require_ready(self):
        if not self.engine.ready:
            raise RuntimeError('JOIN self-test and calibration replay required before admission')

    def generate(self, request_id, model_id, request, spool, submit, *, hold_id=None):
        """spool(PNG bytes, delivery binding) -> canonical signed Go receipt bytes.

        Host verifies receipt signature/identity/height before returning it.
        submit(action) signs and submits through the existing chain transport.
        Evidence persists even if spool or submission fails.
        """
        self.require_ready()
        validate_request(request)
        if len(bytes.fromhex(request_id)) != 32 or request_id != bytes.fromhex(request_id).hex():
            raise ValueError('invalid request ID')
        directory = self.directory(request_id)
        directory.mkdir()  # never overwrite evidence, including retries
        hashes, shape, final = [], None, None
        for i, x in enumerate(self.engine.trajectory(request)):
            raw = self.engine.raw(x)
            durable(directory/f'{i}.etv', raw)
            hashes.append(sha(raw))
            shape, final = tuple(x.shape), x
        pixels = self.engine.decode(final)
        pixel_hash = sha(pixels.tobytes(order='C'))
        from PIL import Image
        encoded = io.BytesIO()
        Image.fromarray(pixels, 'RGB').save(encoded, format='PNG', compress_level=9, optimize=False)
        png = encoded.getvalue()
        durable(directory/'final.png', png)
        tokens = self.engine.tokens(request)
        leaves = []
        for i in range(request['steps']+1):
            leaves.append(dict(step_index=i, input_hash=hashes[max(i-1, 0)], output_hash=hashes[i],
                format_hash=format_hash(shape), profile_id_hash=sha(model_id.encode()), request_id=request_id,
                anchor_hash=prompt_anchor(tokens) if i == 0 else pixel_hash if i == request['steps'] else ZERO))
        root, proofs = tree(leaves)
        sub = dict(actual_dtype='F32', leaves=[wire_leaf(l) for l in leaves], image_sha256=sha(png),
                   pixel_sha256=pixel_hash, image_bytes=len(png), final_latent_hash=hashes[-1])
        meta = dict(request=request, request_id=request_id, model_id=model_id, root=root,
                    leaves=leaves, proofs=proofs, submission=sub, settled_at=None)
        durable(directory/'journal.json', canonical(meta))
        binding = sha(b'EMBER-IMAGE-DELIVERY-V1\0' + (sub['image_sha256']+pixel_hash+hashes[-1]).encode()
                      + struct.pack('>Q', len(png)))
        receipt = spool(png, binding)
        durable(directory/'receipt.json', receipt)
        sub['spool_receipt_hash'] = sha(receipt)
        action = dict(image_submission=sub, commitment_root=base64.b64encode(bytes.fromhex(root)).decode(),
                      type='submit_image', hold_id=hold_id or request_id)
        durable(directory/'journal.json', canonical(meta))
        submit(action)
        return meta

    def opening(self, request_id, steps):
        directory = self.directory(request_id)
        meta = json.loads((directory/'journal.json').read_text())
        # Always include sentinel and adjacent boundary leaves, with proofs.
        indices = {0}
        for i in steps:
            if type(i) is not int or not 0 <= i <= meta['request']['steps']:
                raise ValueError('challenge step outside request')
            indices.update((i, max(i-1, 0)))
        return dict(leaves={str(i): dict(leaf=meta['leaves'][i], proof=meta['proofs'][i]) for i in indices},
                    tensors={str(i): (directory/f'{i}.etv').read_bytes() for i in indices},
                    png=(directory/'final.png').read_bytes())

    def audit(self, context, opening, steps, band, pixels_policy, sign):
        """context is authenticated chain state; steps are chain challenge leaf IDs 0..N.

        sign(payload bytes) returns signature; caller routes envelope to chain's
        verdict transport. Binding failures raise before any model forward.
        """
        self.require_ready()
        r, n = context['request'], context['request']['steps']
        validate_request(r)
        if not steps or len(set(steps)) != len(steps) or any(type(i) is not int or not 0 <= i <= n for i in steps):
            raise ValueError('invalid challenge')
        h, w = self.engine.resolutions[r['resolution']]
        shape = (1, 4, h//8, w//8)
        tokens = self.engine.tokens(r)
        for key, seq in zip(('prompt_tokens', 'negative_tokens'), tokens):
            if key in r and r[key] != seq:
                raise ValueError('binding failure: tokenizer sequence')
        def bound(i):
            item = opening['leaves'][str(i)]
            l = item['leaf']
            if (l['step_index'], l['request_id'], l['profile_id_hash'], l['format_hash']) != (
                    i, context['request_id'], sha(context['model_id'].encode()), format_hash(shape)):
                raise ValueError('binding failure: identity/format')
            verify(l, item['proof'], context['root'], n+1)
            return l
        sentinel = bound(0)
        init = self.engine.raw(self.engine.initial(r))
        if (sentinel['anchor_hash'] != prompt_anchor(tokens) or sentinel['input_hash'] != sha(init)
                or sentinel['output_hash'] != sha(init) or opening['tensors']['0'] != init):
            raise ValueError('binding failure: tokenizer/init anchor')
        tensors = {}
        for i in steps:
            l = bound(i)
            prev = bound(max(i-1, 0))
            if l['input_hash'] != prev['output_hash']:
                raise ValueError('binding failure: adjacent boundary')
            for j, digest in ((max(i-1, 0), l['input_hash']), (i, l['output_hash'])):
                raw = opening['tensors'][str(j)]
                if sha(raw) != digest:
                    raise ValueError('binding failure: tensor hash')
                tensors[j] = self.engine.load(raw, shape)
            expected_anchor = prompt_anchor(tokens) if i == 0 else context['pixel_sha256'] if i == n else ZERO
            if l['anchor_hash'] != expected_anchor or (i == n and l['output_hash'] != context['final_latent_hash']):
                raise ValueError('binding failure: final/interior anchor')
        delivered = None
        if n in steps:
            png = opening['png']
            if sha(png) != context['image_sha256'] or len(png) != context['image_bytes']:
                raise ValueError('binding failure: delivered PNG')
            from PIL import Image
            import numpy as np
            im = Image.open(io.BytesIO(png))
            if im.format != 'PNG' or im.mode != 'RGB' or im.size != (w, h):
                raise ValueError('binding failure: pixel format')
            delivered = np.asarray(im)
            if sha(delivered.tobytes(order='C')) != context['pixel_sha256']:
                raise ValueError('binding failure: pixel anchor')
        # Encode exactly once per challenged request, after all binding checks.
        embeddings = self.engine.embeddings(r)
        formula, rows = Formula(n, r['sampler'], r['guidance']), []
        final_recomputed = None
        for i in steps:
            distance = 0.0
            if i:
                actual = self.engine.step(tensors[i-1], formula, i-1, embeddings)
                distance = relative_l2(actual.cpu().numpy(), tensors[i].cpu().numpy())
                if i == n:
                    final_recomputed = actual
            soft = band['base'] + band['step_coef']*i
            hard = soft * band['hard_multiplier']
            if not all(math.isfinite(v) for v in (soft, hard)) or soft < 0 or hard < soft:
                raise RuntimeError('invalid calibrated band')
            rows.append(dict(step_index=i, distance=distance,
                outcome='match' if distance <= soft else 'mismatch',
                band_region='soft' if distance <= soft else 'drift' if distance <= hard else 'hard'))
        metrics = None
        if delivered is not None:
            import numpy as np
            decoded = self.engine.decode(final_recomputed)
            delta = np.abs(decoded.astype('int16')-delivered.astype('int16'))
            metrics = dict(relative_l2=relative_l2(decoded, delivered),
                           within_one_fraction=float(np.mean(np.all(delta <= 1, axis=2))))
            limit, fraction = pixels_policy['max_relative_l2'], pixels_policy['min_within_one_fraction']
            if not math.isfinite(limit) or limit < 0 or not 0 <= fraction <= 1:
                raise RuntimeError('invalid pixel calibration')
            checks = [metrics['relative_l2'] <= limit, metrics['within_one_fraction'] >= fraction]
            mode = pixels_policy['mode']
            if mode not in ('relative_l2', 'within_one', 'either', 'both'):
                raise RuntimeError('unconfigured pixel policy')
            metrics['pass'] = checks[0] if mode == 'relative_l2' else checks[1] if mode == 'within_one' else any(checks) if mode == 'either' else all(checks)
        if metrics is not None and not metrics['pass']:
            next(row for row in rows if row['step_index'] == n)['outcome'] = 'binding_fail'
        result = dict(request_id=context['request_id'], root=context['root'], challenge_steps=steps,
                      distance=max(row['distance'] for row in rows), per_step=rows, pixels=metrics,
                      verdict='reject' if any(row['outcome'] != 'match' for row in rows) else 'accept')
        payload = b'EMBER-IMAGE-AUDIT-V1\0' + canonical(result)
        return dict(result=result, signature=base64.b64encode(sign(payload)).decode())

    def mark_settled(self, request_id, settled_at):
        if not math.isfinite(settled_at) or settled_at < 0:
            raise ValueError('invalid settlement time')
        p = self.directory(request_id)/'journal.json'
        meta = json.loads(p.read_text())
        meta['settled_at'] = settled_at
        durable(p, canonical(meta))

    def prune(self, request_id, now=None):
        p = self.directory(request_id)
        if p.parent.resolve() != self.root.resolve():
            raise ValueError('invalid evidence path')
        meta = json.loads((p/'journal.json').read_text())
        if meta['settled_at'] is None or (time.time() if now is None else now) < meta['settled_at']+self.margin:
            return False
        shutil.rmtree(p)
        return True


def chain_verdict_action(agent, hold_id, hold, result, evidence_hash):
    """Use the existing signed protobuf verdict and chain envelope format.

    Local dual pixel metrics stay in the evidence index bound by evidence_ref.
    Chain T1 currently has no separate pixel metric protobuf fields.
    """
    from ember_core import sign, digest
    from ember_pb2 import AuditChallenge
    from auditor.spot_wire import SpotVerdict
    from auditor.spot import verdict_kind
    challenge_record = hold['pipeline']['spot']
    steps = challenge_record['step_set']
    if result['challenge_steps'] != steps or result['root'] != base64.b64decode(hold['pipeline']['commitment_root']).hex():
        raise ValueError('result differs from finalized challenge')
    rows = [dict(step=r['step_index'], outcome=r['outcome'],
                 first_divergence_distance=r['distance']) for r in result['per_step']]
    ctx = dict(version=16)
    mode = 'escalation' if challenge_record['round'] == 'esc' else 'spot'
    if mode == 'escalation':
        first = SpotVerdict.FromString(base64.b64decode(challenge_record['first_verdict']))
        ctx['first_per_step'] = [dict(step=p.step, outcome=p.outcome) for p in first.per_step]
    kind = verdict_kind(ctx, rows, mode)
    challenge = AuditChallenge(receipt_ref=bytes.fromhex(hold['receipt_ref']), positions=steps,
                               vrf_proof=bytes.fromhex(hold['audit_proof']))
    sign(challenge, agent.key)
    v = SpotVerdict(challenge_ref=digest(challenge), auditor_id=agent.node_id,
                   verdict='reject' if 'reject' in kind else 'accept',
                   evidence_ref=bytes.fromhex(evidence_hash), distances=[result['distance']],
                   verdict_distance=result['distance'], verdict_kind=kind)
    for row in rows:
        v.per_step.add(**row)
    sign(v, agent.key)
    return dict(type='audit', hold_id=hold_id,
                challenge=base64.b64encode(challenge.SerializeToString(deterministic=True)).decode(),
                verdict=base64.b64encode(v.SerializeToString(deterministic=True)).decode())


def verify_spool_receipt(raw, gateway_key, hold_id, freeze_height, binding, height):
    """Verify exact T1 SpoolAttestation bytes before hashing them into submit_image."""
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    from ember_core import node_id
    a = json.loads(raw)
    fields = ('request_id','challenge_height','step_set_hash','gateway','received','height')
    if (a['request_id'] != hold_id or a['challenge_height'] != freeze_height
            or a['step_set_hash'] != base64.b64encode(bytes.fromhex(binding)).decode()
            or a['gateway'] != node_id(gateway_key) or a['received'] is not True
            or type(a['height']) is not int or not freeze_height <= a['height'] <= height):
        raise ValueError('gateway receipt binding mismatch')
    unsigned = {k: a[k] for k in fields}
    Ed25519PublicKey.from_public_bytes(gateway_key).verify(base64.b64decode(a['signature'], validate=True),
        b'EMBER-SPOT-SPOOL-V1\0' + canonical(unsigned))
    normalized = canonical(dict(unsigned, signature=a['signature']))
    if normalized != raw:
        raise ValueError('receipt must use canonical Go struct JSON bytes')
    return raw
