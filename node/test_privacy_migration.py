"""Migration API enforcement and synthetic two-executor evidence continuity.

These tests never claim revocation of a key already copied by an operator.
Consensus authorization and replay rejection also run in Go state-machine tests.
"""
import copy
import json
import tempfile
import unittest
from pathlib import Path
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey
from ember_pb2 import ExecutionCommitment, ChainGenesis
from ember_core import sign
from privacy.crypto import public, b64, unb64, digest
from privacy.envelope import create, verify_response
from privacy.evidence import BundleStore, decrypt_bundle, proofs, verify
from privacy.executor import Executor, audit
from privacy.migration import rewrap_request, bundle_request
from privacy.escrow import Custodian, identity, release_request, reconstruct
from test_privacy_flow import SyntheticEngine, RoleChain


class CountedEngine(SyntheticEngine):
    def count_input_tokens(self, prompt): return 33


class MigrationChain(RoleChain):
    def hold(self, rid): return self.fee_hold
    def model(self, mid): return dict(signed_profile=self.fee_hold['token_fee']['profile'])
    def submit(self, op, **fields):
        if op == 'migrate':
            if self.record['status'] not in ('requested','checkpoint'):
                raise ValueError('not migratable')
            self.record.setdefault('migrations',[]).append(dict(id=fields['migration_id'],
                to=fields['executor'],from_=self.record['executor'],finalized=False,
                wrap_key=self.operator(fields['executor'])['wrap_key']))
            m=self.record['migrations'][-1];m['from']=m.pop('from_')
        elif op == 'rewrap':
            m=self.record['migrations'][-1]
            if m['finalized'] or m['id']!=fields['migration_id'] or m['to']!=fields['executor']:
                raise ValueError('stale or wrong target')
            m.update(finalized=True,wrapped_key=fields['wrapped_key']);self.record['executor']=m['to']
        elif op == 'checkpoint':
            self.record.setdefault('segments',[]).append(dict(root=fields['root'],
                bundle_ref=fields['bundle_ref'],tokens=fields['output_tokens']))
            self.record.update(status='checkpoint',root=fields['root'],bundle_ref=fields['bundle_ref'])
        else:
            super().submit(op,**fields)


class MigrationTests(unittest.TestCase):
    def setup_flow(self, tmp):
        self.consumer,self.a,self.b,self.auditor=[Ed25519PrivateKey.generate() for _ in range(4)]
        self.aw,self.bw,self.uw=[X25519PrivateKey.generate() for _ in range(3)]
        self.aid,self.bid,self.uid=[identity(public(k)) for k in (self.a,self.b,self.auditor)]
        custodians={f'custodian-{i}':X25519PrivateKey.generate() for i in range(5)}
        self.e,self.raw,self.key=create('synthetic private prompt','model',self.aid,public(self.aw),
            'epoch',{i:public(k) for i,k in custodians.items()},self.consumer,max_tokens=4)
        rid=self.e['request_id']
        record=dict(id=rid,tier='P0',consumer=identity(public(self.consumer)),executor=self.aid,
            model_id='model',epoch='epoch',envelope_hash=digest(self.e),status='requested',auditor='')
        self.chain=MigrationChain(record,{self.aid:dict(wrap_key=b64(public(self.aw))),
            self.bid:dict(wrap_key=b64(public(self.bw))),self.uid:dict(wrap_key=b64(public(self.uw)))},list(custodians))
        profile=ChainGenesis(chain_id='model',execution_profile_ver='v',price_input=2,price_output=3,price_context=1)
        c=ExecutionCommitment(request_id=rid,chain_id='model',profile_ver='v',input_tokens=33,max_output_tokens=4,
            input_commitment=bytes.fromhex(self.e['ciphertext_blob_ref']))
        sign(c,self.a)
        self.chain.fee_hold=dict(token_fee=dict(profile=b64(profile.SerializeToString()),commitment=b64(c.SerializeToString())))
        self.ea=Executor(self.chain,self.a,self.aw,CountedEngine(),BundleStore(Path(tmp)/'a'),'model')
        self.eb=Executor(self.chain,self.b,self.bw,CountedEngine(),BundleStore(Path(tmp)/'b'),'model')
        self.custodians={i:Custodian(self.chain,'epoch',k,Path(tmp)/i) for i,k in custodians.items()}
        for i,custodian in self.custodians.items():custodian.receive(rid,unb64(self.e['wrapped_keys']['shares'][i]))
        return rid,record

    def handshake(self,rid):
        self.chain.submit('migrate',request_id=rid,executor=self.bid,migration_id='d'*64)
        request=rewrap_request(self.chain,rid,self.key,self.consumer)
        wrong=dict(request,executor=self.uid)
        with self.assertRaises(ValueError):self.chain.submit('rewrap',**wrong)
        self.chain.submit('rewrap',**request)
        with self.assertRaises(ValueError):self.chain.submit('rewrap',**request)
        return request

    def test_unopened_old_executor_cut_off_by_all_key_and_escrow_apis(self):
        with tempfile.TemporaryDirectory() as tmp:
            rid,record=self.setup_flow(tmp)
            # A has never executed or called unwrap_request before this migration.
            self.handshake(rid)
            with self.assertRaises(ValueError):self.ea.unwrap_request(self.raw)
            with self.assertRaises(ValueError):self.ea.execute(self.raw)
            with self.assertRaises(ValueError):self.ea.prepare_pricing(self.raw)
            self.assertEqual(self.eb.unwrap_request(self.raw),self.key)
            result=self.eb.execute(self.raw)
            self.assertEqual(result['receipt']['actual'],78)
            with self.assertRaises(ValueError):self.eb.bundle(bundle_request(rid,record['bundle_ref'],self.a))
            # Exercise actual HTTP dispatch without creating a socket.
            from privacy.server import handler
            from privacy.chain import Chain
            from io import BytesIO
            from types import SimpleNamespace
            self.chain.state=lambda:dict(privacy=dict(requests={rid:record}))
            self.chain.public_bundle=lambda ref:Chain.public_bundle(self.chain,ref)
            class LocalHandler(handler(executor=self.ea)):
                def send_response(self,status):self.status=status
                def send_header(self,*args):pass
                def end_headers(self):pass
            def dispatch(path,body=None):
                h=object.__new__(LocalHandler);h.path=path;h.wfile=BytesIO()
                h.connection=SimpleNamespace(settimeout=lambda _:None)
                if body is None:h.do_GET()
                else:
                    h.rfile=BytesIO(body);h.headers={'Content-Length':str(len(body))};h.do_POST()
                return h.status
            self.assertEqual(dispatch('/v2/requests',self.raw),400)
            self.assertEqual(dispatch('/v2/tokenize',self.raw),400)
            self.assertEqual(dispatch('/v2/resume',json.dumps(dict(transport=b64(self.raw))).encode()),400)
            self.assertEqual(dispatch('/bundles/access',json.dumps(bundle_request(rid,record['bundle_ref'],self.a)).encode()),400)
            self.assertEqual(dispatch('/bundles/'+record['bundle_ref']),404)
            record.update(status='assigned',auditor=self.uid)
            with self.assertRaises(ValueError):next(iter(self.custodians.values())).release(release_request(self.chain,rid,self.a))

    def test_checkpoint_resume_escrow_and_full_output_merkle_root(self):
        with tempfile.TemporaryDirectory() as tmp:
            rid,record=self.setup_flow(tmp)
            checkpoint=self.ea.execute(self.raw,checkpoint_tokens=2)
            old_ref=checkpoint['receipt']['bundle_ref']
            old_cipher=self.ea.store.get(old_ref)
            old=decrypt_bundle(self.key,rid,old_cipher)
            self.handshake(rid)
            # A previously held K_r: only API refusal is asserted, never offline revocation.
            with self.assertRaises(ValueError):self.ea.unwrap_request(self.raw)
            with self.assertRaises(ValueError):self.ea.bundle(bundle_request(rid,old_ref,self.a))
            allowed=self.ea.bundle(bundle_request(rid,old_ref,self.b))
            self.assertEqual(allowed,old_cipher)
            with self.assertRaises(ValueError):self.eb.execute(self.raw)
            result=self.eb.execute(self.raw,prior_ciphertexts=[allowed])
            output=verify_response(self.e,result,public(self.b),self.key,migration_record=record)
            self.assertEqual(output['text'],'synthetic output'*4)
            final_cipher=self.eb.bundle(bundle_request(rid,record['bundle_ref'],self.b))
            final=decrypt_bundle(self.key,rid,final_cipher)
            self.assertEqual(final['items'][:2],old['items'])
            self.assertTrue(all(verify(record['root'],p) for p in proofs(final['items'],0,4)))
            record.update(status='assigned',auditor=self.uid)
            releases=[(i,c.release(release_request(self.chain,rid,self.auditor))) for i,c in list(self.custodians.items())[:3]]
            # Both segments use the same escrowed key, reconstructed by the assigned auditor.
            from privacy.crypto import unwrap
            shares=[json.loads(unwrap(self.uw,r,(rid+'/auditor/epoch').encode())) for _,r in releases]
            escrow_key=reconstruct(shares,3)
            self.assertEqual(decrypt_bundle(escrow_key,rid,old_cipher)['root'],old['root'])
            self.assertEqual(decrypt_bundle(escrow_key,rid,final_cipher)['root'],record['root'])
            verdict,_=audit(self.chain,self.auditor,self.uw,self.e,releases,final_cipher,CountedEngine(),[old_cipher])
            self.assertEqual(verdict,'accept')
            self.ea.retire(rid)
            self.assertFalse((self.ea.store.directory/old_ref).exists())
            self.assertEqual(self.chain.events[-1]['kind'],'migration_deleted')

    def test_encrypted_tokenization_prepares_exact_pricing_commitment(self):
        with tempfile.TemporaryDirectory() as tmp:
            rid,_=self.setup_flow(tmp)
            def missing(_):raise KeyError(rid)
            self.chain.request=missing
            prepared=self.ea.prepare_pricing(self.raw)
            c=ExecutionCommitment.FromString(unb64(prepared['pricing_commitment']))
            from ember_core import verify as verify_signature
            verify_signature(c)
            self.assertEqual(c.request_id,rid)
            self.assertEqual((c.input_tokens,c.max_output_tokens,prepared['amount']),(33,4,78))
            self.assertEqual(c.input_commitment,bytes.fromhex(self.e['ciphertext_blob_ref']))

    def test_count_mismatch_refused_before_generation(self):
        with tempfile.TemporaryDirectory() as tmp:
            rid,_=self.setup_flow(tmp)
            c=ExecutionCommitment.FromString(unb64(self.chain.fee_hold['token_fee']['commitment']))
            c.input_tokens=32
            self.chain.fee_hold['token_fee']['commitment']=b64(c.SerializeToString())
            with self.assertRaises(ValueError):self.ea.execute(self.raw)
