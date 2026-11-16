#!/usr/bin/env python3
"""Local GPU-only staging acceptance; dedicated validator/roles, no existing services.

Requires socket permission and CUDA. Creates all secrets/logs under --work-dir;
never touches existing inference services. This is a single-host integration
exercise, not evidence of independent operators or secure deletion.
"""
import argparse
import copy
import hashlib
import importlib.util
import json
import socket
import subprocess
import threading
import time
from pathlib import Path
from http.server import ThreadingHTTPServer


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--work-dir',required=True)
    p.add_argument('--emberchain',required=True)
    p.add_argument('--emberd',required=True)
    p.add_argument('--model-path',required=True)
    a=p.parse_args()
    # Check GPU before creating processes or loading weights. Never substitute CPU.
    import torch
    from engine.device import available_bytes
    if not torch.cuda.is_available():raise RuntimeError('GPU_ACCEPTANCE_BLOCKED: CUDA unavailable')
    if available_bytes()<16*1024**3:raise RuntimeError('GPU_ACCEPTANCE_BLOCKED: available memory below 16 GiB')
    probe=socket.socket();probe.bind(('127.0.0.1',0));probe.close()
    root=Path(a.work_dir).resolve();root.mkdir(mode=0o700,parents=True,exist_ok=False)
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey
    from cryptography.hazmat.primitives import serialization
    from privacy.crypto import public,b64,unb64,digest,canonical,unwrap,open_seal
    from privacy.escrow import identity,Custodian,release_request,reconstruct
    from privacy.chain import Chain
    from privacy.envelope import create,verify_response,encode_transport
    from privacy.executor import Executor,audit
    from privacy.evidence import BundleStore,decrypt_bundle,dispute,verify
    from privacy.server import handler
    from engine import TransformersEngine
    from auditor.gpu import GPU_PROFILE,gpu_engine_id
    engine=TransformersEngine(a.model_path,'cuda')
    assert engine.device=='cuda'
    fingerprint=gpu_engine_id(engine.capabilities(),engine.resource_metadata())
    actors={};wrap_keys={};key_paths={}
    for name in ['consumer','executor','auditor','coordinator','governor']+[f'custodian-{i}' for i in range(5)]:
        key=Ed25519PrivateKey.generate();actors[name]=key;wrap_keys[name]=X25519PrivateKey.generate()
        path=root/(name+'.signing');path.write_bytes(key.private_bytes(serialization.Encoding.Raw,serialization.PrivateFormat.Raw,serialization.NoEncryption()));path.chmod(0o600);key_paths[name]=path
    ids={name:identity(public(key)) for name,key in actors.items()}
    chain_id='ember-privacy-staging';model_id='ember-qwen2.5-1.5b-gpu-privacy'
    model_files={}
    for path in sorted(Path(a.model_path).iterdir()):
        if path.is_file() and path.suffix in ('.json','.safetensors'):
            hasher=hashlib.sha256()
            with path.open('rb') as stream:
                for block in iter(lambda:stream.read(1024*1024),b''):hasher.update(block)
            model_files[path.name]=hasher.hexdigest()
    if not any(name.endswith('.safetensors') for name in model_files):raise RuntimeError('missing weights')
    from ember_pb2 import ChainGenesis, ModelManifest, ResourceProfile, ShardSpec
    from ember_core import sign, digest as proto_digest
    manifest=ModelManifest(chain_id=model_id,total_layers=28,
        shards=[ShardSpec(index=0,start_layer=0,end_layer=28,has_embedding=True,has_lm_head=True,
                          unit_kind='range',parameters_hash=bytes.fromhex(digest(model_files)))])
    sign(manifest,actors['governor']);manifest_root=proto_digest(manifest)
    pricing=ChainGenesis(chain_id=model_id,model_manifest_root=manifest_root,
        execution_profile_ver=GPU_PROFILE,price_input=2,price_output=3,price_context=1)
    sign(pricing,actors['governor'])
    pricing_raw=pricing.SerializeToString(deterministic=True);manifest_raw=manifest.SerializeToString(deterministic=True)
    app=dict(parameters=dict(chain_id=chain_id,registered_auditors=True,subsidy_cap=0,unbonding_blocks=1,slash_multiplier=20,dispute_blocks=0,hold_timeout_blocks=200,registration_ttl_blocks=10000),
        accounts=[dict(id=id,balance=1000000,stake=1000000) for id in ids.values()],
        models=[dict(id=model_id,genesis_hash=hashlib.sha256(pricing_raw).hexdigest(),
            profile_hash=hashlib.sha256(manifest_raw).hexdigest(),signed_profile=b64(pricing_raw),signed_manifest=b64(manifest_raw),
            pricing=dict(target_executors=1,target_auditors=1,executor_weight=80,auditor_weight=15,consumer_weight=5))],
        coordinators=[ids['coordinator']],auditors=[ids['auditor']],governors=[ids['governor']])
    app_path=root/'app.json';app_path.write_bytes(canonical(app));app_path.chmod(0o600)
    home=root/'validator';genesis=home/'config/genesis.json'
    def run(*args):return subprocess.run([a.emberchain,*map(str,args)],check=True,capture_output=True,text=True)
    run('init-validator','--home',home)
    run('genesis','--out',genesis,'--chain-id',chain_id,'--app-state',app_path,'--validators',home)
    def free_port():
        with socket.socket() as sock:sock.bind(('127.0.0.1',0));return sock.getsockname()[1]
    rpc_port,p2p_port,gateway_port=free_port(),free_port(),free_port()
    rpc=f'http://127.0.0.1:{rpc_port}'
    processes=[];servers=[];logs=[]
    def start(command,name):
        log=(root/name).open('wb');logs.append(log)
        proc=subprocess.Popen(list(map(str,command)),stdout=log,stderr=subprocess.STDOUT);processes.append(proc);return proc
    def serve(h):
        server=ThreadingHTTPServer(('127.0.0.1',0),h);server.daemon_threads=True;servers.append(server)
        threading.Thread(target=server.serve_forever,daemon=True).start();return server.server_address[1]
    from cryptography.exceptions import InvalidTag
    def refused(fn):
        try:fn()
        except (ValueError,KeyError,InvalidTag):return
        raise AssertionError('negative request unexpectedly accepted')
    try:
        start([a.emberchain,'start','--home',home,'--rpc',f'tcp://127.0.0.1:{rpc_port}','--p2p',f'tcp://127.0.0.1:{p2p_port}'],'chain.log')
        chains={name:Chain(a.emberchain,rpc,chain_id,genesis,key,key_paths[name]) for name,key in actors.items()}
        for attempt in range(60):
            try:chains['consumer'].state();break
            except subprocess.CalledProcessError:
                if attempt==59:raise
                time.sleep(.5)
        policy=dict(retention_seconds=60,deletion_practice='best-effort unlink after finality',evidence_handling='AEAD ciphertext')
        for name,chain in chains.items():chain.submit('operator',wrap_key=b64(public(wrap_keys[name])),policy=policy)
        for name,role in [('executor','model'),('auditor','auditor')]:
            profile=ResourceProfile(node_id=ids[name],chain_id=model_id,manifest_root=manifest_root,
                execution_profile_ver=GPU_PROFILE,actual_device='cuda',torch_version=engine.torch_version)
            profile.engine_capabilities.CopyFrom(engine.capabilities().to_proto())
            del profile.engine_capabilities.roles[:];profile.engine_capabilities.roles.append(role)
            sign(profile,actors[name])
            chains[name].submit_action(dict(type='register_node',profile=b64(profile.SerializeToString(deterministic=True)),expires_height=10000))
        custodian_names=[f'custodian-{i}' for i in range(5)]
        members=[ids[name] for name in custodian_names]
        chains['governor'].submit('epoch',epoch='epoch-1',threshold=3,members=members)
        store=BundleStore(root/'bundles')
        executor=Executor(chains['executor'],actors['executor'],wrap_keys['executor'],engine,store,model_id)
        executor_port=serve(handler(executor=executor))
        gateway=start([a.emberd,'privacy-gateway','--listen',f'127.0.0.1:{gateway_port}',
            '--executor',f'http://127.0.0.1:{executor_port}/v2/requests','--capture-dir',root/'gateway-captures'],'gateway.log')
        relay_path=Path(__file__).resolve().parents[1]/'deploy/public-relay.py'
        spec=importlib.util.spec_from_file_location('privacy_relay',relay_path);relay=importlib.util.module_from_spec(spec);spec.loader.exec_module(relay)
        relay_captures=root/'relay-captures';relay_captures.mkdir(mode=0o700)
        sequence=[0]
        def capture(direction,raw):
            sequence[0]+=1;path=relay_captures/f'{sequence[0]:06d}.{direction}';path.write_bytes(raw);path.chmod(0o600)
        relay_port=serve(relay.handler('gateway',gateway_port,capture))
        import http.client
        def request(raw):
            connection=http.client.HTTPConnection('127.0.0.1',relay_port,timeout=300)
            try:
                connection.request('POST','/v2/requests',raw,{'Content-Type':'application/json'})
                response=connection.getresponse();return response.status,response.read()
            finally:connection.close()
        for attempt in range(100):
            try:
                with socket.create_connection(('127.0.0.1',gateway_port),timeout=.2):break
            except OSError:
                if gateway.poll() is not None:raise RuntimeError('gateway exited')
                if attempt==99:raise
                time.sleep(.05)
        marker='EMBER_PRIVATE_MARKER_'+hashlib.sha256(public(actors['consumer'])).hexdigest()
        prompt='<|im_start|>user\nReply briefly: '+marker+'<|im_end|>\n<|im_start|>assistant\n'
        arb=X25519PrivateKey.generate()
        e,raw,key=create(prompt,model_id,ids['executor'],public(wrap_keys['executor']),'epoch-1',
            {ids[name]:public(wrap_keys[name]) for name in custodian_names},actors['consumer'],public(arb),max_tokens=4)
        rid=e['request_id'];chains['consumer'].submit('request',request_id=rid,executor=ids['executor'],tier='P0',
            model_id=model_id,epoch='epoch-1',envelope_hash=digest(e),**executor.prepare_pricing(raw))
        custodians={name:Custodian(chains[name],'epoch-1',wrap_keys[name],root/name) for name in custodian_names}
        for name,c in custodians.items():c.receive(rid,unb64(e['wrapped_keys']['shares'][ids[name]]))
        status,body=request(raw);assert status==200,(status,body)
        response=json.loads(body);verify_response(e,response,public(actors['executor']),key)
        record=chains['consumer'].request(rid);ciphertext=store.get(record['bundle_ref'])
        refused(lambda:decrypt_bundle(b'x'*32,rid,ciphertext))
        pre=release_request(chains['auditor'],rid,actors['auditor']);refused(lambda:custodians[custodian_names[0]].release(pre))
        chains['coordinator'].submit('assign',request_id=rid)
        release=release_request(chains['auditor'],rid,actors['auditor'])
        bad=copy.deepcopy(release);bad['message'].pop('proof');refused(lambda:custodians[custodian_names[0]].release(bad))
        bad=copy.deepcopy(release);bad['message']['request_id']='wrong';refused(lambda:custodians[custodian_names[0]].release(bad))
        bad=release_request(chains['consumer'],rid,actors['consumer']);refused(lambda:custodians[custodian_names[0]].release(bad))
        releases=[(ids[name],custodians[name].release(release)) for name in custodian_names[:3]]
        refused(lambda:audit(chains['auditor'],actors['auditor'],wrap_keys['auditor'],e,releases[:2],ciphertext,engine))
        verdict,stats=audit(chains['auditor'],actors['auditor'],wrap_keys['auditor'],e,releases,ciphertext,engine);assert verdict=='accept'
        bundle=decrypt_bundle(key,rid,ciphertext);window=dispute(bundle,e,0,1);ctx=(rid+'/dispute').encode()
        revealed=json.loads(open_seal(unwrap(arb,unb64(window['wrapped_key']),ctx),unb64(window['ciphertext']),ctx))
        assert len(revealed['proofs'])==1 and verify(record['root'],revealed['proofs'][0])
        refused(lambda:unwrap(X25519PrivateKey.generate(),unb64(window['wrapped_key']),ctx))
        chains['consumer'].submit('finalize',request_id=rid);store.delete(record['bundle_ref'],rid,chains['executor'])
        try:store.get(record['bundle_ref']);raise AssertionError('bundle survived deletion')
        except FileNotFoundError:pass
        p1=copy.deepcopy(e);p1.pop('signature');p1['tier']='P1'
        p1['signature']=b64(actors['consumer'].sign(b'ember/envelope/v2\0'+canonical(p1)))
        status,body=request(encode_transport(p1,unb64(json.loads(raw)['blob'])))
        assert status==503 and b'TIER_UNAVAILABLE' in body
        refused(lambda:verify_response(p1,response,public(actors['executor']),key))
        chains['governor'].submit('end_epoch',epoch='epoch-1')
        for c in custodians.values():c.destroy()
        for directory in [root/'gateway-captures',relay_captures]:
            assert list(directory.iterdir()),'empty captures'
            for path in directory.iterdir():assert marker.encode() not in path.read_bytes(),path
        for path in root.glob('*.log'):assert marker.encode() not in path.read_bytes(),path
        result=dict(status='PASS',profile=GPU_PROFILE,runtime=fingerprint,actual_device=engine.device,verdict=verdict,
            stats=stats,request_id=rid,root=record['root'],p1='TIER_UNAVAILABLE',transit_marker_absent=True,
            scope='single-host validator and role identities; one shared real engine reused sequentially for replay')
        (root/'result.json').write_bytes(canonical(result));print(json.dumps(result,indent=2))
    finally:
        for server in servers:server.shutdown();server.server_close()
        for process in reversed(processes):
            process.terminate()
            try:process.wait(timeout=10)
            except subprocess.TimeoutExpired:process.kill();process.wait()
        for log in logs:log.close()
        engine.close()


if __name__=='__main__':main()
