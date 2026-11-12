"""Socketless synthetic role integration; never a GPU staging substitute."""
import copy
import json
from pathlib import Path
import struct
import tempfile
import unittest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey
from privacy.crypto import public,b64,unb64,digest,canonical,unwrap,open_seal
from privacy.envelope import create,verify_response
from privacy.escrow import Custodian,identity,release_request,reconstruct
from privacy.evidence import BundleStore,decrypt_bundle,dispute,verify
from privacy.executor import Executor,audit
from engine import Step


class SyntheticEngine:
    device='synthetic'
    def generate(self,prompt,seed,count):
        return self.replay(prompt,seed,[15]*count)
    def replay(self,prompt,seed,tokens):
        return [Step(t,'synthetic output',[(i,float(i)) for i in range(15,-1,-1)],
                     {k:struct.pack('<ff',1,2) for k in ('0','14','27')}) for t in tokens]


class RoleChain:
    """Explicit test double; consensus transition validation is tested in Go."""
    chain_id='ember-synthetic-privacy'
    def __init__(self,record,operators,members):
        self.record=record;self.operators=operators;self.events=[]
        self.config=dict(ended=False,threshold=3,members=members)
    def request(self,rid):
        if self.record['id']!=rid:raise KeyError(rid)
        return self.record
    def hold(self,rid):return {}
    def operator(self,actor):return self.operators[actor]
    def epoch(self,epoch):return self.config
    def event(self,kind,**fields):self.events.append(dict(kind=kind,**fields))
    def submit(self,op,**fields):
        fields.pop('request_id',None);self.record.update(fields)
        self.record['status']={'commit':'committed','verdict':'audited'}[op]


class FlowTests(unittest.TestCase):
    def test_roles_ciphertext_escrow_replay_dispute_deletion(self):
        consumer,producer,auditor=[Ed25519PrivateKey.generate() for _ in range(3)]
        producer_wrap,auditor_wrap,arb=[X25519PrivateKey.generate() for _ in range(3)]
        custodian_keys={f'custodian-{i}':X25519PrivateKey.generate() for i in range(5)}
        executor_id,auditor_id=identity(public(producer)),identity(public(auditor))
        marker='PRIVATE-PROMPT-MARKER'
        e,raw,key=create(marker,'ember-unit',executor_id,public(producer_wrap),'e1',
            {actor:public(k) for actor,k in custodian_keys.items()},consumer,public(arb),max_tokens=4)
        record=dict(id=e['request_id'],tier='P0',consumer=identity(public(consumer)),executor=executor_id,
                    model_id='ember-unit',epoch='e1',envelope_hash=digest(e),status='requested',auditor='')
        chain=RoleChain(record,{auditor_id:dict(wrap_key=b64(public(auditor_wrap)))},list(custodian_keys))
        with tempfile.TemporaryDirectory() as tmp:
            store=BundleStore(Path(tmp)/'bundles')
            executor=Executor(chain,producer,producer_wrap,SyntheticEngine(),store,'ember-unit')
            custodians={actor:Custodian(chain,'e1',k,Path(tmp)/actor) for actor,k in custodian_keys.items()}
            for actor,c in custodians.items():
                c.receive(e['request_id'],unb64(e['wrapped_keys']['shares'][actor]))
            response=executor.execute(raw)
            self.assertNotIn(marker.encode(),raw)
            self.assertNotIn(b'synthetic output',canonical(response))
            self.assertEqual(verify_response(e,response,public(producer),key)['text'],'synthetic output'*4)
            ciphertext=store.get(record['bundle_ref'])
            self.assertNotIn(marker.encode(),ciphertext)
            with self.assertRaises(Exception):decrypt_bundle(b'x'*32,e['request_id'],ciphertext)
            req=release_request(chain,e['request_id'],auditor)
            with self.assertRaises(ValueError):next(iter(custodians.values())).release(req)
            record.update(status='assigned',auditor=auditor_id)
            req=release_request(chain,e['request_id'],auditor)
            released=[(actor,c.release(req)) for actor,c in list(custodians.items())[:3]]
            with self.assertRaises(ValueError):audit(chain,auditor,auditor_wrap,e,released[:2],ciphertext,SyntheticEngine())
            verdict,stats=audit(chain,auditor,auditor_wrap,e,released,ciphertext,SyntheticEngine())
            self.assertEqual(verdict,'accept');self.assertEqual(stats['distance'],0)
            self.assertEqual(record['stats_hash'],digest(stats))
            bundle=decrypt_bundle(key,e['request_id'],ciphertext)
            window=dispute(bundle,e,1,3)
            context=(e['request_id']+'/dispute').encode()
            wk=unwrap(arb,unb64(window['wrapped_key']),context)
            opened=json.loads(open_seal(wk,unb64(window['ciphertext']),context))
            self.assertEqual([p['item']['index'] for p in opened['proofs']],[1,2])
            self.assertNotIn('prompt',opened)
            self.assertTrue(all(verify(record['root'],p) for p in opened['proofs']))
            with self.assertRaises(ValueError):store.delete(record['bundle_ref'],e['request_id'],chain)
            record['status']='final';store.delete(record['bundle_ref'],e['request_id'],chain)
            with self.assertRaises(FileNotFoundError):store.get(record['bundle_ref'])
            self.assertEqual(chain.events[-1]['kind'],'bundle_deleted')
            chain.config['ended']=True
            for c in custodians.values():c.destroy()
