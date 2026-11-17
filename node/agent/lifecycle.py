"""Operator-owned exit, unbond and claim using the node's existing identity."""
import argparse
import json
import subprocess
from pathlib import Path
import urllib.error
import urllib.parse
import urllib.request

from .join import identity, _chain_json, _chain_tx, _chain_height


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
    state={'parameters': _chain_json(opts.chain_cli,opts.chain_rpc,'/parameters'),
           'accounts': {ident: _chain_json(opts.chain_cli,opts.chain_rpc,'/account/'+ident)},
           'nodes': {}, 'height': _chain_height(opts.chain_rpc)}
    try:
        state['nodes'][ident]=_chain_json(opts.chain_cli,opts.chain_rpc,'/node/'+ident)
    except subprocess.CalledProcessError as exc:
        if 'not found' not in str(exc.stderr):
            raise
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


def request_exec_exit_main(args=None):
    parser = argparse.ArgumentParser(prog='ember-node request-exec-exit')
    parser.add_argument('--state-dir', required=True)
    parser.add_argument('--chain-rpc', required=True)
    parser.add_argument('--chain-cli', default='emberchain')
    parser.add_argument('--ledger-chain-id', required=True)
    opts = parser.parse_args(args)
    state_dir = Path(opts.state_dir)
    key = state_dir / 'identity.key'
    if not key.is_file():
        raise ValueError('execution exit requires an existing node identity; no key was created')
    _, ident = identity(state_dir)
    parameters = _chain_json(opts.chain_cli, opts.chain_rpc, '/parameters')
    if parameters['chain_id'] != opts.ledger_chain_id:
        raise ValueError('execution exit RPC ledger chain ID mismatch')
    account = _chain_json(opts.chain_cli, opts.chain_rpc, '/account/' + ident)
    _chain_tx(opts, key, account['nonce'] + 1, {'type': 'request_exec_exit'})
    status = _chain_json(opts.chain_cli, opts.chain_rpc, '/node-status/' + ident)
    summary = dict(node_id=ident, height=status['height'],
                   exec_exit_pending=status.get('exec_exit_pending', False),
                   exec_exit_effective_height=status.get('exec_exit_effective_height', 0),
                   exec_exited=status.get('exec_exited', False))
    print(json.dumps(summary, sort_keys=True))
    print('Execution exit effective height: %d' % summary['exec_exit_effective_height'])
