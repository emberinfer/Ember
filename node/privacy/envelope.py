"""Consumer-side envelope v2. Keys and redaction maps stay local."""
import hashlib
import json
import secrets
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
from .crypto import canonical, digest, b64, unb64, seal, open_seal, wrap, public
from .escrow import split, identity

BUCKETS = (4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576)
PADDING_PROFILE = 'buckets-v1'
TIERS = ('P0', 'P1')


def context(envelope, purpose):
    return (envelope['request_id'] + '/' + purpose).encode()


def encode_transport(envelope, blob):
    value = dict(envelope=envelope, blob=b64(blob), padding='')
    size = len(canonical(value))
    bucket = next((b for b in BUCKETS if b >= size), None)
    if bucket is None:
        raise ValueError('request exceeds padding buckets')
    value['padding'] = '0' * (bucket-size)
    return canonical(value)


def create(prompt, model_id, executor, executor_wrap_key, epoch, custodian_keys,
           consumer_key, arbitration_key=None, tier='P0', threshold=3, max_tokens=3, seed=7):
    if tier not in TIERS:
        raise ValueError('invalid tier')
    key, request_id = secrets.token_bytes(32), secrets.token_hex(32)
    body = dict(prompt=prompt, max_tokens=max_tokens, seed=seed)
    blob = seal(key, canonical(body), (request_id + '/prompt').encode())
    shares = split(key, threshold, len(custodian_keys))
    e = dict(version=2, request_id=request_id, tier=tier, model_id=model_id,
        ciphertext_blob_ref=hashlib.sha256(blob).hexdigest(),
        wrapped_keys=dict(executor=executor, key=b64(wrap(executor_wrap_key, key, (request_id+'/executor').encode())),
            epoch=epoch, shares={actor:b64(wrap(pub,canonical(share),(request_id+'/'+epoch).encode()))
                for (actor,pub),share in zip(custodian_keys.items(),shares)}),
        session_nonce=secrets.token_hex(32),
        dispute_consent=dict(enabled=arbitration_key is not None,
                             arbitration_key=b64(arbitration_key) if arbitration_key else ''),
        evidence_policy=dict(top_k=16, metric='rank-probability-v1', tolerance=.001,
                             layers=['0','14','27'], delete_after_finality=True),
        padding_profile=PADDING_PROFILE, consumer_key=b64(public(consumer_key)))
    e['signature'] = b64(consumer_key.sign(b'ember/envelope/v2\0' + canonical(e)))
    return e, encode_transport(e,blob), key


def validate(raw):
    if len(raw) not in BUCKETS:
        raise ValueError('invalid padding bucket')
    value = json.loads(raw)
    if set(value) != {'envelope','blob','padding'} or set(value['padding'])-{'0'}:
        raise ValueError('invalid transport')
    e, blob = value['envelope'], unb64(value['blob'])
    required = {'version','request_id','tier','model_id','ciphertext_blob_ref','wrapped_keys',
        'session_nonce','dispute_consent','evidence_policy','padding_profile','consumer_key','signature'}
    if set(e) != required or e['version'] != 2 or e['tier'] not in TIERS or e['padding_profile'] != PADDING_PROFILE:
        raise ValueError('invalid envelope')
    for name in ('request_id','session_nonce','ciphertext_blob_ref'):
        if len(bytes.fromhex(e[name])) != 32:
            raise ValueError('invalid envelope identifier')
    if hashlib.sha256(blob).hexdigest() != e['ciphertext_blob_ref']:
        raise ValueError('blob reference mismatch')
    unsigned = {k:v for k,v in e.items() if k != 'signature'}
    Ed25519PublicKey.from_public_bytes(unb64(e['consumer_key'])).verify(unb64(e['signature']),
        b'ember/envelope/v2\0' + canonical(unsigned))
    return e, blob


def verify_response(envelope, response, executor_signer, key, migration_record=None):
    r = response['receipt']
    Ed25519PublicKey.from_public_bytes(executor_signer).verify(unb64(response['signature']),
        b'ember/privacy-receipt/v2\0' + canonical(r))
    expected_executor = envelope['wrapped_keys']['executor']
    if migration_record is not None:
        if (migration_record['id'] != envelope['request_id'] or migration_record['envelope_hash'] != digest(envelope)
                or not migration_record.get('migrations') or not migration_record['migrations'][-1]['finalized']):
            raise ValueError('unfinalized migration receipt')
        expected_executor = migration_record['executor']
        if r['root'] != migration_record['root'] or r['bundle_ref'] != migration_record['bundle_ref']:
            raise ValueError('receipt differs from migrated commitment')
    ciphertext = unb64(response['output'])
    if (r['request_id'] != envelope['request_id'] or r['tier'] != envelope['tier']
            or r['envelope_hash'] != digest(envelope) or r['executor'] != identity(executor_signer)
            or r['executor'] != expected_executor
            or r['output_hash'] != hashlib.sha256(ciphertext).hexdigest()):
        raise ValueError('receipt binding or honored tier mismatch')
    return json.loads(open_seal(key,ciphertext,context(envelope,'output')))


def rewrap(envelope, key, receiving_id, receiving_key, consumer_key):
    """Before registration, rewrap for another executor; prior holders cannot be revoked."""
    import copy
    e = copy.deepcopy(envelope)
    e.pop('signature')
    e['wrapped_keys']['executor'] = receiving_id
    e['wrapped_keys']['key'] = b64(wrap(receiving_key,key,context(e,'executor')))
    e['signature'] = b64(consumer_key.sign(b'ember/envelope/v2\0'+canonical(e)))
    return e
