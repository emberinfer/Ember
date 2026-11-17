import copy
import tempfile
import unittest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey
from privacy.crypto import public, wrap, unwrap, b64, canonical
from privacy.escrow import split, reconstruct, Custodian, release_request, identity
import json


class MemoryChain:
    chain_id = 'ember-unit'
    def __init__(self, auditor, wrap_key):
        self.record = dict(id='r', epoch='e1', auditor=identity(public(auditor)), root='a'*64,
                           envelope_hash='b'*64, status='committed')
        self.config = dict(threshold=3, ended=False)
        self.wrap_key = wrap_key
        self.events = []
    def request(self, request_id):
        if request_id != 'r': raise KeyError(request_id)
        return self.record
    def epoch(self, epoch): return self.config
    def operator(self, actor): return dict(wrap_key=b64(public(self.wrap_key)))
    def event(self, kind, **fields): self.events.append(dict(kind=kind, **fields))


class EscrowTests(unittest.TestCase):
    def test_threshold_and_authorization(self):
        key = b'k'*32; shares = split(key)
        auditor, auditor_wrap = Ed25519PrivateKey.generate(), X25519PrivateKey.generate()
        chain = MemoryChain(auditor, auditor_wrap)
        with tempfile.TemporaryDirectory() as tmp:
            nodes = []
            for index, share in enumerate(shares):
                private = X25519PrivateKey.generate()
                c = Custodian(chain, 'e1', private, tmp+'/'+str(index))
                c.receive('r', wrap(public(private), canonical(share), b'r/e1'))
                nodes.append(c)
            request = release_request(chain, 'r', auditor)
            with self.assertRaises(ValueError): nodes[0].release(request)  # pre-assignment
            chain.record['status'] = 'assigned'
            bad = copy.deepcopy(request); bad['message'].pop('proof')
            with self.assertRaises(ValueError): nodes[0].release(bad)
            bad = copy.deepcopy(request); bad['message']['request_id'] = 'wrong'
            with self.assertRaises(ValueError): nodes[0].release(bad)
            bad = release_request(chain, 'r', Ed25519PrivateKey.generate())
            with self.assertRaises(ValueError): nodes[0].release(bad)
            released = [json.loads(unwrap(auditor_wrap,c.release(request),b'r/auditor/e1')) for c in nodes[:3]]
            with self.assertRaises(ValueError): reconstruct(released[:2],3)
            self.assertEqual(reconstruct(released,3),key)
            self.assertEqual(reconstruct(shares[1:4],3),key)
            self.assertEqual(sum(e['kind']=='share_refusal' for e in chain.events),4)
            chain.config['ended'] = True
            for c in nodes: c.destroy()
            with self.assertRaises(ValueError): nodes[0].release(request)
