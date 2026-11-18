"""One fixed ACT-2e MoE service, with pinned TLS and persistent RPC nonces."""
import argparse
from concurrent import futures
import json
import os
from pathlib import Path
import sys
ROOT=Path(os.environ.get('EMBER_MOE_TREE','/root/work/ODSI-act2a'))
sys.path[:0]=[str(ROOT/p) for p in ('proto','core-py','node')]
import grpc
import torch
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives import serialization
from ember_core import node_id
from ember_pb2 import ModelManifest,ChainGenesis,ResourceProfile
from ember_pb2_grpc import add_ShardPipelineServicer_to_server
from engine.moe import MoeWeights,ExpertUnit,AttentionStage
from moe_rpc import ReceiptSigner,MoeExpertService,MoeAttentionService,MoeExpertClient
BASE=Path('/root/work/act2e-nodes')
RELEASE=Path('/root/work/act2e-pubmoe/release')
def main():
    parser=argparse.ArgumentParser();parser.add_argument('name');args=parser.parse_args()
    fleet=json.loads((BASE/'fleet.json').read_text());entry=fleet[args.name];d=BASE/args.name
    torch.set_num_threads(int(os.environ.get('EMBER_MOE_THREADS','12')))
    key=Ed25519PrivateKey.from_private_bytes((d/'identity.key').read_bytes())
    m=ModelManifest.FromString((RELEASE/'manifest.pb').read_bytes());g=ChainGenesis.FromString((RELEASE/'genesis.pb').read_bytes())
    weights=MoeWeights(os.environ.get('EMBER_MOE_WEIGHTS','/root/autodl-tmp/moe-shard-test/olmoe-full'))
    assembler=Ed25519PrivateKey.from_private_bytes((BASE/'assembler.key').read_bytes())
    assembler_id=node_id(assembler.public_key().public_bytes(serialization.Encoding.Raw,serialization.PublicFormat.Raw))
    signer=ReceiptSigner(key,g,m)
    if args.name.startswith('exp-'):
        parity=0 if 'even' in args.name else 1
        def factory(l,e): return ExpertUnit(weights,l,e) if 0<=l<16 and 0<=e<64 and e%2==parity else None
        service=MoeExpertService(factory,signer,[fleet[n]['node_id'] for n in ('stage-a','stage-b')]+[assembler_id],nonce_path=d/'rpc-nonces.json')
    else:
        idx=0 if args.name=='stage-a' else 1
        stage=AttentionStage(weights,m.moe_layout,idx*8,(idx+1)*8)
        clients={}
        for parity,name in enumerate(('exp-even-1','exp-odd-1')):
            p=ResourceProfile.FromString((BASE/name/'profile.pb').read_bytes())
            clients[parity]=MoeExpertClient(fleet[name]['target'],key,profile=p)
        service=MoeAttentionService(stage,signer,lambda l,e:clients[e%2],[assembler_id],nonce_path=d/'rpc-nonces.json')
    server=grpc.server(futures.ThreadPoolExecutor(max_workers=4),options=(('grpc.max_receive_message_length',64*1024*1024),('grpc.max_send_message_length',64*1024*1024)))
    add_ShardPipelineServicer_to_server(service,server)
    credentials=grpc.ssl_server_credentials((((d/'tls.key').read_bytes(),(d/'tls.crt').read_bytes()),))
    if not server.add_secure_port(f"127.0.0.1:{entry['port']}",credentials):raise RuntimeError('bind failed')
    server.start();print(f"READY {args.name} {entry['target']}",flush=True);server.wait_for_termination()
if __name__=='__main__':main()
