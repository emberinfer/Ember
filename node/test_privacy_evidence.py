import tempfile
import unittest
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey
from privacy.crypto import public, b64, unb64, unwrap, open_seal, digest
from privacy.evidence import commit, proofs, verify, BundleStore, decrypt_bundle, dispute
from engine import Step
import json


class EvidenceTests(unittest.TestCase):
    def test_encryption_merkle_window(self):
        steps = [Step(i, '', [(j, float(j)) for j in range(16)], {}) for i in range(6)]
        root, items = commit(steps)
        self.assertNotEqual(root, commit(steps)[0])
        self.assertTrue(all(verify(root, p) for p in proofs(items, 0, 6)))
        tampered = proofs(items, 0, 1)[0]; tampered['item'] = dict(tampered['item'], salt='bad')
        self.assertFalse(verify(root, tampered))
        with tempfile.TemporaryDirectory() as tmp:
            store = BundleStore(tmp); key = b'k'*32
            bundle = dict(root=root, items=items)
            ref = store.put(key, 'r', bundle)
            self.assertNotIn(b'raw-logits', store.get(ref))
            self.assertEqual(decrypt_bundle(key, 'r', store.get(ref)), json.loads(json.dumps(bundle)))
            with self.assertRaises(Exception):
                decrypt_bundle(b'x'*32, 'r', store.get(ref))
            arb = X25519PrivateKey.generate()
            envelope = dict(request_id='r', dispute_consent=dict(enabled=True, arbitration_key=b64(public(arb))))
            bundle['envelope_hash']=digest(envelope)
            revealed = dispute(bundle, envelope, 2, 4)
            window_key = unwrap(arb, unb64(revealed['wrapped_key']), b'r/dispute')
            window = json.loads(open_seal(window_key, unb64(revealed['ciphertext']), b'r/dispute'))
            self.assertEqual([p['item']['index'] for p in window['proofs']], [2, 3])
            self.assertTrue(all(verify(root, p) for p in window['proofs']))
            with self.assertRaises(Exception):
                unwrap(X25519PrivateKey.generate(), unb64(revealed['wrapped_key']), b'r/dispute')
            envelope['dispute_consent']['enabled'] = False
            with self.assertRaises(ValueError):
                dispute(bundle, envelope, 2, 4)
