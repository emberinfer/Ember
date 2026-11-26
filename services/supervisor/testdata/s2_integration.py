"""Loopback-only S2 acceptance: real publication, workers, JOIN, priced execution and settlement."""
import base64
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
import urllib.error
from types import SimpleNamespace

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import digest, sign, node_id
from ember_pb2 import ChainGenesis, ModelManifest
from agent.join import identity, _chain_tx, _chain_json
from agent.publish import prepare

ROOT = Path(__file__).resolve().parents[3]
OUT = Path(os.environ.get('S2_OUTPUT', str(ROOT / 'services/supervisor/testdata/s2-output/acceptance')))


def port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def main():
    OUT.mkdir(parents=True,exist_ok=True)
    children, logs = [], []
    chain, daemon = (str(ROOT / '.runtime/s1' / name) for name in ('emberchain', 'emberd'))
    with tempfile.TemporaryDirectory(prefix='s2-loopback-') as tmp:
        tmp = Path(tmp)
        def run(*args):
            try:
                return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT)
            except subprocess.CalledProcessError as error:
                print(error.output, flush=True)
                raise
        def start(name, *args):
            log = (OUT / (name + '.log')).open('w'); logs.append(log)
            process = subprocess.Popen(args, stdout=log, stderr=subprocess.STDOUT, cwd=ROOT)
            children.append(process)
            return process
        def wait_for(predicate, timeout=80):
            end = time.monotonic() + timeout
            while time.monotonic() < end:
                try:
                    result = predicate()
                    if result:
                        return result
                except (OSError, ValueError, KeyError, subprocess.CalledProcessError):
                    pass
                time.sleep(.3)
            raise RuntimeError('local acceptance timeout; inspect s2-output logs')
        def http(path, body=None):
            req = urllib.request.Request(base + path, None if body is None else json.dumps(body).encode(),
                    headers={'Content-Type':'application/json', 'Authorization':'Bearer '+token})
            try:
                with urllib.request.urlopen(req, timeout=150) as response:
                    raw = response.read()
                    return response.status, dict(response.headers), raw
            except urllib.error.HTTPError as error:
                return error.code, dict(error.headers), error.read()
        def catalog():
            code, _, raw = http('/v1/catalog'); assert code == 200, raw
            return {row['profile_id']:row for row in json.loads(raw)['profiles']}
        try:
            # Tiny local, random Qwen2: no downloads, no production models/funds.
            import torch
            from transformers import Qwen2Config, Qwen2ForCausalLM, PreTrainedTokenizerFast
            import transformers
            from tokenizers import Tokenizer, models, pre_tokenizers
            torch.manual_seed(7)
            weights = tmp / 'tiny'; weights.mkdir()
            model = Qwen2ForCausalLM(Qwen2Config(vocab_size=32, hidden_size=8, intermediate_size=16,
                num_hidden_layers=28, num_attention_heads=2, num_key_value_heads=1,
                max_position_embeddings=256, tie_word_embeddings=True, eos_token_id=None))
            model.save_pretrained(weights)
            tokenizer = Tokenizer(models.WordLevel({'[UNK]':0, 'hello':1, **{f't{i}':i for i in range(2,32)}}, unk_token='[UNK]'))
            tokenizer.pre_tokenizer = pre_tokenizers.Whitespace()
            PreTrainedTokenizerFast(tokenizer_object=tokenizer, unk_token='[UNK]').save_pretrained(weights)
            # The fixture publishes config/tokenizer artifacts (the chain's own
            # small publication fixture convention); engine weights remain local.
            def material(ident, price, key):
                manifest = ModelManifest(chain_id=ident)
                for name in ('config.json', 'tokenizer.json'):
                    raw = (weights/name).read_bytes()
                    manifest.artifacts.add(name=name, sha256=hashlib.sha256(raw).digest(), size=len(raw), source='git:'+name)
                manifest.config_hash = manifest.artifacts[0].sha256
                manifest.graph_hash = hashlib.sha256(b's2-tiny').digest()
                sign(manifest,key)
                genesis = ChainGenesis(chain_id=ident, execution_profile_ver='transformers-s2-'+str(price),
                    model_manifest_root=digest(manifest), tokenizer_hash=manifest.artifacts[1].sha256, price_output=price)
                policy=genesis.numeric_audit_policy
                policy.metric='union-interval-v1';policy.top_k=16;policy.layers.extend(['0','14','27'])
                policy.calibration_ref=hashlib.sha256(b's2-local-identical-runtime-fixture').digest()
                engine_id=f'transformers/{transformers.__version__}|torch={torch.__version__}'
                policy.bands.add(producer_engine=engine_id,producer_device='cpu',reference_engine=engine_id,
                    reference_device='cpu',tolerance=0.000001,mode='enforce')
                sign(genesis,key)
                gr,mr = genesis.SerializeToString(deterministic=True),manifest.SerializeToString(deterministic=True)
                return dict(id=ident,genesis_hash=hashlib.sha256(gr).hexdigest(),profile_hash=digest(genesis).hex(),
                    publication=dict(genesis=base64.b64encode(gr).decode(), manifest=base64.b64encode(mr).decode(),
                        publisher=node_id(genesis.signer),manifest_root=digest(manifest).hex())), gr, mr
            comp=dict(formula_ver=2,o_fixed=1,margin_bps=10000,audit_share_bps=1500,benchmark_ref='ab'*32)
            default, new = 'ember-s2-default', 'ember-s2-new-publication'
            ledger = 'ember-s2-isolated'
            publisher, publisher_id = identity(tmp/'publisher')
            authority_key, authority_id = identity(tmp/'authority')
            (tmp/'authority/identity.key').write_bytes((tmp/'authority/identity.key').read_bytes()+authority_key.public_key().public_bytes(serialization.Encoding.Raw,serialization.PublicFormat.Raw))
            payer_key,payer_id = identity(tmp/'payer')
            nodeids = [identity(tmp/('node'+str(i)))[1] for i in range(4)]
            fixture,_,_ = material(default,100,publisher)
            fixture['audit_comp']=comp
            _,gr,mr = material(new,200,publisher)
            token = 's2-local-only-'+'x'*40
            auth = tmp/'auth.json'; auth.write_text(json.dumps([dict(token=token,key=str(tmp/'payer/identity.key'))])); auth.chmod(0o600)
            home = tmp/'validator'; print(run(chain,'init-validator','--home',str(home)),flush=True)
            (home/'config/config.toml').write_text('[statesync]\nenable = false\n')
            sampling=dict(enabled=True)
            if os.environ.get('S2_FORCE_AUDIT'): sampling['tier_rates_ppm']=[1000000]*3
            app = tmp/'app.json'; app.write_text(json.dumps(dict(parameters=dict(chain_id=ledger,unbonding_blocks=3,
                slash_multiplier=20,dispute_blocks=10,registered_auditors=True,audit_sampling=sampling),models=[fixture],
                accounts=[dict(id=pid,balance=10000000,stake=100000) for pid in [publisher_id,authority_id,payer_id]+nodeids],
                coordinators=[authority_id],auditors=nodeids[2:],governors=[authority_id])))
            print(run(chain,'genesis','--out',str(home/'config/genesis.json'),'--chain-id',ledger,
                '--app-state',str(app),'--validators',str(home)),flush=True)
            rpcport,p2p,tunnel,web = [port() for _ in range(4)]
            rpc=f'http://127.0.0.1:{rpcport}'; base=f'http://127.0.0.1:{web}'
            start('integration-chain',chain,'start','--home',str(home),'--rpc',f'tcp://127.0.0.1:{rpcport}','--p2p',f'tcp://127.0.0.1:{p2p}')
            def query(path): return _chain_json(chain,rpc,path)
            wait_for(lambda:query('/state')['height']>0)
            common=tmp/'worker-args.json'; common.write_text(json.dumps(['--chain-coordinator-key',str(tmp/'authority/identity.key'),
                '--chain-auditor-key',str(tmp/'authority/identity.key'),'--chain-auth-file',str(auth),
                '--engine','transformers','--model-path',str(weights),'--auditor-python',sys.executable,
                '--audit-device','cpu','--chain-audit-routing=true','--liveness-enabled=false']))
            start('integration-supervisor',daemon,'supervisor','--chain-rpc',rpc,'--chain-id',ledger,
                '--default-profile',default,'--worker-args',str(common),'--worker-dir',str(tmp/'workers'),
                '--http',f'127.0.0.1:{web}','--grpc',f'127.0.0.1:{tunnel}','--insecure-localhost','--poll','1s')
            wait_for(lambda:default in catalog())
            # Publish only AFTER the supervisor has started. No new route/trust config.
            (tmp/'template-genesis.pb').write_bytes(gr)
            (tmp/'template-manifest.pb').write_bytes(mr)
            prepared = tmp/'prepared'
            prepare(SimpleNamespace(state_dir=tmp/'publisher', genesis=tmp/'template-genesis.pb',
                manifest=tmp/'template-manifest.pb', model_path=weights, chain_cli=chain,
                origin='https://example.org/s2-tiny', revision='1'*40, model_id=new,
                profile_version='transformers-s2-200', previous_profile_hash=None, output=prepared))
            action = json.loads((prepared/'registration.json').read_text())
            assert action['pricing'] == dict(target_executors=1, target_auditors=1, executor_weight=80,
                auditor_weight=15, consumer_weight=5)
            assert action['audit_comp'] == comp
            opts=SimpleNamespace(chain_cli=chain,chain_rpc=rpc,ledger_chain_id=ledger)
            _chain_tx(opts,tmp/'publisher/identity.key',query('/account/'+publisher_id)['nonce']+1,action)
            record = query('/model/'+new)
            assert record['publication']['genesis'] == action['genesis']
            assert record['pricing'] == action['pricing']
            assert record['audit_comp'] == dict(comp, r_in_per_ktok=0, r_out_per_ktok=0)
            wait_for(lambda:catalog().get(new,{}).get('worker_state')=='running')
            print('PASS 1: chain publication after supervisor startup automatically discovered and worker launched',flush=True)
            trust=tmp/'node-trust.json';trust.write_text(json.dumps(dict(profiles={pid:dict(publishers=[publisher_id]) for pid in (default,new)})))
            def join(i):
                pid=(default,new)[i%2]
                return start('integration-node'+str(i),sys.executable,'-u','-m','agent','join','--state-dir',str(tmp/('node'+str(i))),
                    '--chain',pid,'--coordinator',f'127.0.0.1:{tunnel}','--insecure-localhost','--engine','transformers',
                    '--model-path',str(weights),'--device','cpu','--chain-rpc',rpc,'--chain-cli',chain,'--ledger-chain-id',ledger,
                    '--trust-policy',str(trust),'--health-listen','','--registration-ttl','10000','--no-liveness-enabled',
                    *(['--audit-role','--no-exec'] if i>=2 else ['--no-audit']))
            nodes=[join(i) for i in range(4)]
            wait_for(lambda:all('JOIN READY' in (OUT/f'integration-node{i}.log').read_text() for i in range(4)))
            wait_for(lambda:all(catalog()[pid]['ready'] for pid in (default,new)))
            print('PASS 2: two real CPU nodes asserted profile JOIN READY through shared tunnel',flush=True)
            def order(pid):
                node_index=(default,new).index(pid)
                before=query('/account/'+nodeids[node_index])['balance']
                request=dict(model=pid,messages=[dict(role='user',content='hello')],max_tokens=2,seed=7,stream=False)
                code,_,raw=http('/v1/quote',request);assert code==200,(code,raw)
                quote=json.loads(raw);assert quote['model']==pid
                code,headers,raw=http('/v1/chat/completions',request);assert code==200,(code,raw)
                hid=headers.get('X-Request-Id') or headers.get('X-Request-ID') or json.loads(raw)['id']
                known=set()
                def settled():
                    # Terminal holds move into the chain archive. Capture their
                    # exact ID while live, then use the archive-aware query.
                    known.update(key for key in query('/state').get('holds',{}) if key.startswith(hid))
                    for key in known:
                        value=query('/hold/'+key)
                        if value and value.get('state')=='settled': return value
                hold=wait_for(settled,timeout=120)
                assert hold['model_id']==pid,(pid,hold)
                shares=hold['shares'];fee=hold['actual']
                assert shares['executor']==fee*80//100 and shares['auditor']==fee*15//100 and shares['consumer']==fee*5//100,shares
                assert sum(shares[k] for k in ('executor','auditor','consumer'))==fee
                assert query('/account/'+nodeids[node_index])['balance']-before==shares['executor']
                if hold.get('sampling',{}).get('sampled'): assert hold.get('audit_verdict')=='accept',hold
                print('READBACK executor balance delta=',shares['executor'],'fee split=',shares,'audit sampled=',hold.get('sampling',{}).get('sampled'),flush=True)
                print('SETTLEMENT',pid,json.dumps({key:hold.get(key) for key in ('id','model_id','node','actual','shares','sampling','unaudited','audit_verdict','audit_escrow_subunits','audit_receipts')},sort_keys=True),flush=True)
                return hold,quote
            hold,quote=order(new)
            print('PASS 3: quote, execution, chain settlement; sampling=',json.dumps(hold.get('sampling')), 'unaudited=',hold.get('unaudited',False),flush=True)
            with concurrent.futures.ThreadPoolExecutor(2) as executor:
                results=list(executor.map(order,(default,new)))
            assert results[0][1]['freeze'] != results[1][1]['freeze'],results
            assert results[0][0]['node']==nodeids[0] and results[1][0]['node']==nodeids[1]
            print('PASS 4: concurrent default/new orders retain distinct material, quotes, nodes and settlements',flush=True)
            nodes[1].terminate();nodes[1].wait(timeout=15)
            wait_for(lambda:not catalog()[new]['ready'],timeout=20)
            code,_,raw=http('/v1/chat/completions',dict(model=new,messages=[dict(role='user',content='hello')],max_tokens=2))
            assert code==503 and raw,(code,raw)
            nodes[1]=join(1);wait_for(lambda:catalog()[new]['ready'])
            print('PASS 5: disconnect flips not ready with reason, order rejected, reconnect restores ready',flush=True)
            # Website's standalone harness runs against the same live profile/catalog.
            env=dict(os.environ,EMBER_ROUTER_URL=base,S2_PROFILE_ID=new)
            result=subprocess.run([sys.executable,'/root/ember-web-push/scripts/test_dynamic_catalog.py'],env=env,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
            print(result.stdout,flush=True);assert result.returncode==0
            print('PASS 6: website build_models availability and order route match live router catalog',flush=True)
        finally:
            for child in reversed(children): child.terminate()
            for child in reversed(children):
                try: child.wait(timeout=15)
                except subprocess.TimeoutExpired: child.kill();child.wait()
            worker_dir=tmp/'workers'
            if worker_dir.exists():
                for path in worker_dir.glob('*/worker.log'):
                    (OUT/('worker-'+path.parent.name+'.log')).write_bytes(path.read_bytes())
            for log in logs: log.close()

if __name__=='__main__': main()
