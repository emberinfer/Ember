"""Domain-separated AEAD and X25519 key wrapping; no custom ciphers."""
import base64
import hashlib
import json
import secrets
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey, X25519PublicKey
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.hkdf import HKDF
from cryptography.hazmat.primitives import hashes


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()


def b64(raw):
    return base64.b64encode(raw).decode()


def unb64(value):
    return base64.b64decode(value, validate=True)


def public(key):
    return key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)


def derive(key, context):
    return HKDF(algorithm=hashes.SHA256(), length=32, salt=None,
                info=b'ember/privacy/v2/' + context).derive(key)


def seal(key, data, context):
    nonce = secrets.token_bytes(12)
    return nonce + AESGCM(derive(key, context)).encrypt(nonce, data, context)


def open_seal(key, blob, context):
    return AESGCM(derive(key, context)).decrypt(blob[:12], blob[12:], context)


def wrap(recipient, key, context):
    ephemeral = X25519PrivateKey.generate()
    shared = ephemeral.exchange(X25519PublicKey.from_public_bytes(recipient))
    return public(ephemeral) + seal(shared, key, context)


def unwrap(private, blob, context):
    shared = private.exchange(X25519PublicKey.from_public_bytes(blob[:32]))
    return open_seal(shared, blob[32:], context)


def digest(value):
    return hashlib.sha256(canonical(value)).hexdigest()
