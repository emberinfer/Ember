"""Consumer re-wrap handshake and signed, current-state bundle access."""
import secrets
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
from .crypto import canonical, public, b64, unb64, wrap, unwrap
from .escrow import identity


def migration_context(record, migration):
    return canonical(['ember/privacy/migration/v2', record['id'], record['envelope_hash'],
                      migration['id'], migration['from'], migration['to'], migration['wrap_key']])


def rewrap_request(chain, request_id, key, consumer_key):
    record = chain.request(request_id)
    migration = record['migrations'][-1]
    if record['consumer'] != identity(public(consumer_key)) or migration['finalized']:
        raise ValueError('not pending consumer migration')
    wrapped = wrap(unb64(migration['wrap_key']), key, migration_context(record, migration))
    return dict(request_id=request_id, migration_id=migration['id'], executor=migration['to'],
                wrapped_key=b64(wrapped))


def migrate(chain, request_id, target, key, consumer_key):
    chain.submit('migrate', request_id=request_id, executor=target, migration_id=secrets.token_hex(32))
    chain.submit('rewrap', **rewrap_request(chain, request_id, key, consumer_key))


def retired(record, actor):
    return any(m['finalized'] and m['from'] == actor for m in record.get('migrations', []))


def executor_key(chain, request_id, actor, wrapping_key, envelope):
    record = chain.request(request_id)
    if record['executor'] != actor or retired(record, actor):
        raise ValueError('executor access revoked')
    migrations = record.get('migrations', [])
    if migrations:
        m = migrations[-1]
        if not m['finalized'] or m['to'] != actor or b64(public(wrapping_key)) != m['wrap_key']:
            raise ValueError('migration not finalized for this key')
        return unwrap(wrapping_key, unb64(m['wrapped_key']), migration_context(record, m))
    return unwrap(wrapping_key, unb64(envelope['wrapped_keys']['key']),
                  (request_id+'/executor').encode())


def bundle_request(request_id, ref, signing_key):
    message = dict(request_id=request_id, ref=ref, nonce=secrets.token_hex(32))
    return dict(message=message, public_key=b64(public(signing_key)),
                signature=b64(signing_key.sign(b'ember/privacy-bundle/v2\0'+canonical(message))))


def authorize_bundle(chain, request):
    message = request['message']
    record = chain.request(message['request_id'])
    pub = unb64(request['public_key']); actor = identity(pub)
    Ed25519PublicKey.from_public_bytes(pub).verify(unb64(request['signature']),
        b'ember/privacy-bundle/v2\0'+canonical(message))
    refs = [record.get('bundle_ref')] + [s['bundle_ref'] for s in record.get('segments', [])]
    allowed = [record['consumer'], record['executor']]
    if record['status'] == 'assigned': allowed.append(record['auditor'])
    if (record['status'] == 'final' or message['ref'] not in refs or actor not in allowed
            or retired(record, actor)):
        raise ValueError('bundle access refused')
    return message['ref']
