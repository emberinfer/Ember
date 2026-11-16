"""Operator-owned exit, unbond and claim using the node's existing identity."""
import argparse
import json
from pathlib import Path
import urllib.error
import urllib.parse
import urllib.request

from .join import identity, _chain_json, _chain_tx


def main(args=None):
    parser=argparse.ArgumentParser(prog='ember-node exit')
    parser.add_argument('--state-dir',required=True)
    parser.add_argument('--chain-rpc',required=True)
    parser.add_argument('--chain-cli',default='emberchain')
    parser.add_argument('--ledger-chain-id',required=True)
    parser.add_argument('--action',choices=('status','unbond','claim'),default='status')
    parser.add_argument('--health-url',default='http://127.0.0.1:19192/healthz')
    opts=parser.parse_args(args)
    state_dir=Path(opts.state_dir)
    if not (state_dir/'identity.key').is_file():
        raise ValueError('exit requires an existing node identity; no key was created')
    _, ident=identity(state_dir)
    state=_chain_json(opts.chain_cli,opts.chain_rpc,'/state')
    if state['parameters']['chain_id']!=opts.ledger_chain_id:
        raise ValueError('exit RPC ledger chain ID mismatch')
    account=state['accounts'][ident]
    registration=state.get('nodes',{}).get(ident)
    remaining=max(0,registration['expires_height']-state['height']) if registration else 0
    if opts.action=='unbond':
        health=urllib.parse.urlparse(opts.health_url)
        if health.scheme!='http' or health.hostname not in ('127.0.0.1','localhost','::1'):
            raise ValueError('exit health check must use a loopback HTTP endpoint')
        try:
            with urllib.request.urlopen(opts.health_url,timeout=2):
                pass
        except urllib.error.HTTPError:
            raise ValueError('stop the node process before unbonding; its health listener is still running')
        except (urllib.error.URLError,TimeoutError):
            pass
        else:
            raise ValueError('stop the node process before unbonding; its health listener is still running')
        if remaining:
            raise ValueError('registration remains active for %d blocks; stop renewals and wait for expiry' % remaining)
        if account['stake']<=0:
            raise ValueError('no stake remains to unbond')
        _chain_tx(opts,state_dir/'identity.key',account['nonce']+1,dict(type='unbond',amount=account['stake']))
    elif opts.action=='claim':
        if not any(x['release_height']<=state['height'] for x in account.get('unbondings',[])):
            raise ValueError('no matured unbonding; wait until a release_height is finalized')
        _chain_tx(opts,state_dir/'identity.key',account['nonce']+1,dict(type='claim_unbonded'))
    print(json.dumps(dict(account=ident,height=state['height'],balance=account['balance'],
        stake=account['stake'],registration_blocks_remaining=remaining,
        unbonding_blocks=state['parameters']['unbonding_blocks'],
        unbondings=account.get('unbondings',[])),sort_keys=True))
    if opts.action!='status':
        print('Transaction committed. Run --action status to query the resulting balance and release heights.')
