import json
import unittest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey
from privacy.crypto import public, canonical, b64, unb64, unwrap, digest, seal
from privacy.envelope import create, validate, verify_response, rewrap, encode_transport, BUCKETS
from privacy.escrow import identity
import hashlib


class EnvelopeTests(unittest.TestCase):
    def test_blind_padding_binding_and_migration(self):
        client, executor = Ed25519PrivateKey.generate(), Ed25519PrivateKey.generate()
        wrap_key = X25519PrivateKey.generate(); actor = identity(public(executor))
        custodians = {str(i):public(X25519PrivateKey.generate()) for i in range(5)}
        e, raw, key = create('PLANTED-PRIVATE-MARKER','ember-model',actor,public(wrap_key),'e1',custodians,client)
        self.assertIn(len(raw),BUCKETS); self.assertNotIn(b'PLANTED-PRIVATE-MARKER',raw)
        self.assertEqual(validate(raw)[0],e)
        self.assertEqual(unwrap(wrap_key,unb64(e['wrapped_keys']['key']),(e['request_id']+'/executor').encode()),key)
        other = X25519PrivateKey.generate()
        changed = rewrap(e,key,'receiving',public(other),client)
        with self.assertRaises(Exception):
            unwrap(wrap_key,unb64(changed['wrapped_keys']['key']),(e['request_id']+'/executor').encode())
        self.assertEqual(unwrap(other,unb64(changed['wrapped_keys']['key']),(e['request_id']+'/executor').encode()),key)
        output = seal(key,canonical(dict(text='private output')),(e['request_id']+'/output').encode())
        r=dict(request_id=e['request_id'],tier='P0',executor=actor,envelope_hash=digest(e),output_hash=hashlib.sha256(output).hexdigest())
        response=dict(receipt=r,output=b64(output),signature=b64(executor.sign(b'ember/privacy-receipt/v2\0'+canonical(r))))
        self.assertEqual(verify_response(e,response,public(executor),key)['text'],'private output')
        requested=dict(e,tier='P1')
        with self.assertRaisesRegex(ValueError,'honored tier'):
            verify_response(requested,response,public(executor),key)
        bad=json.loads(raw);bad['envelope']['tier']='P1'
        with self.assertRaises(Exception): validate(encode_transport(bad['envelope'],unb64(bad['blob'])))
