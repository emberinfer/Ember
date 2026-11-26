"""Registration broadcast ambiguity and stable wire payload regression tests."""
import base64
import json
import subprocess
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from ember_pb2 import ResourceProfile
from agent.join import _ensure_stake, _registration_tx, register_on_chain


def args():
    return SimpleNamespace(chain_cli='cli', chain_rpc='rpc', ledger_chain_id='ledger')


def action():
    return dict(type='register_node', profile='signed-profile', expires_height=100)


def test_stake_target_tops_up_only_the_difference(tmp_path):
    account = dict(stake=500, balance=4500, nonce=4)
    with patch('agent.join._chain_tx') as tx, patch('agent.join._chain_json', return_value=dict(nonce=5)):
        assert _ensure_stake(args(), tmp_path/'identity.key', 'node', account, 5000) == 5
    assert tx.call_args.args[2:] == (5, dict(type='stake', amount=4500))


def test_stake_target_reports_shortfall_before_broadcast(tmp_path):
    with patch('agent.join._chain_tx') as tx, pytest.raises(ValueError, match='4000 more subunits'):
        _ensure_stake(args(), tmp_path/'identity.key', 'node', dict(stake=500, balance=500, nonce=4), 5000)
    tx.assert_not_called()


@pytest.mark.parametrize('error', [subprocess.TimeoutExpired('cli', 60),
    subprocess.CalledProcessError(1, 'cli', stderr='tx already exists in cache'),
    ConnectionResetError('connection reset')])
def test_committed_after_unknown_broadcast(error, tmp_path):
    with patch('agent.join._chain_tx', side_effect=error) as tx, \
         patch('agent.join._chain_json', return_value=action()) as query, \
         patch('agent.join.time.sleep'):
        _registration_tx(args(), tmp_path/'identity.key', 'node', 4, action())
    tx.assert_called_once()
    assert query.call_args.args[-1] == '/node/node'


def test_uncommitted_rereads_nonce_without_blind_increment(tmp_path):
    with patch('agent.join._chain_tx', side_effect=[subprocess.TimeoutExpired('cli', 60), None]) as tx, \
         patch('agent.join._chain_json', side_effect=[None, dict(nonce=8)]), \
         patch('agent.join._chain_height', return_value=10), patch('agent.join.time.sleep'):
        _registration_tx(args(), tmp_path/'identity.key', 'node', 4, action())
    assert [c.args[2] for c in tx.call_args_list] == [5, 9]
    assert tx.call_args_list[0].args[3] == tx.call_args_list[1].args[3]


def test_unknown_confirmation_never_authorizes_rebroadcast(tmp_path):
    with patch('agent.join._chain_tx', side_effect=OSError('lost reply')) as tx, \
         patch('agent.join._chain_json', side_effect=[OSError('query down'), action()]), \
         patch('agent.join.time.sleep') as sleep:
        _registration_tx(args(), tmp_path/'identity.key', 'node', 4, action())
    assert tx.call_count == 1
    assert [c.args[0] for c in sleep.call_args_list] == [1, 2]


def test_nonce_alone_cannot_confirm_liveness_as_registration(tmp_path):
    different = dict(profile='older-profile', expires_height=100)
    with patch('agent.join._chain_tx', side_effect=[OSError('lost'), None]) as tx, \
         patch('agent.join._chain_json', side_effect=[different, dict(nonce=7)]), \
         patch('agent.join._chain_height', return_value=10), patch('agent.join.time.sleep'):
        _registration_tx(args(), tmp_path/'identity.key', 'node', 4, action())
    assert tx.call_count == 2
    assert tx.call_args.args[2] == 8


def test_setup_failures_do_not_kill_registration_thread(tmp_path):
    with patch('agent.join._register_on_chain', side_effect=[OSError('down')]*8+[None]) as setup, \
         patch('agent.join.time.sleep') as sleep:
        register_on_chain(args(), tmp_path, 'node', ResourceProfile())
    assert setup.call_count == 9
    assert [c.args[0] for c in sleep.call_args_list] == [1, 2, 4, 8, 16, 30, 30, 30]


def test_missing_account_retry_explains_faucet(tmp_path, capsys):
    with patch('agent.join._chain_json', return_value=None), \
         patch('agent.join.time.sleep', side_effect=[None, KeyboardInterrupt]), \
         pytest.raises(KeyboardInterrupt):
        register_on_chain(args(), tmp_path, 'node', ResourceProfile())
    assert capsys.readouterr().out.splitlines() == [
        f'REGISTER retry in {delay}s: Account not found on chain. Claim from the faucet first.'
        for delay in (1, 2)]


def test_action_and_cli_signing_payload_match_baseline(tmp_path):
    # Frozen successful-path builder from the parent implementation. Keep this
    # independent of git history so shallow source checkouts can run the test.
    def legacy_action(args, profile, chain_state, ttl):
        from agent.registration_roles import opt_outs
        return {'type': 'register_node',
                **opt_outs(args, profile.engine_capabilities),
                'profile': base64.b64encode(profile.SerializeToString(
                    deterministic=True)).decode(),
                'expires_height': int(chain_state['height']) + ttl,
                **({'caps': {'profiles': args._contribution_policy['profiles'],
                              'global': args._contribution_policy['global']}}
                   if hasattr(args, '_contribution_policy') else {})}

    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from ember_core import sign
    profile = ResourceProfile(node_id='fixture-node', chain_id='fixture-model')
    sign(profile, Ed25519PrivateKey.from_private_bytes(bytes(range(32))))
    state = dict(height=10, nodes={})
    for policy in (None, dict(profiles={'fixture-model': 20}, global_limit=30)):
        options = args()
        options.no_exec = True
        if policy:
            options._contribution_policy = {'profiles': policy['profiles'],
                                            'global': policy['global_limit']}
        with patch('agent.join._chain_json', side_effect=[dict(nonce=4, stake=5000),
                 dict(profile_hash='00'), dict(registration_ttl_blocks=100)]), \
             patch('agent.join._chain_state_targeted', return_value=state), \
             patch('agent.join.subprocess.run') as run:
            register_on_chain(options, tmp_path, 'fixture-node', profile)
        # Original _chain_tx serialization and all CLI signing inputs.
        expected = ['cli', 'tx', '--rpc', 'rpc', '--key', str(tmp_path/'identity.key'),
                    '--chain-id', 'ledger', '--nonce', '5', '--action',
                    json.dumps(legacy_action(options, profile, state, 100), separators=(',', ':'))]
        run.assert_called_once_with(expected, check=True, timeout=60, text=True, capture_output=True)
        wire = json.loads(run.call_args.args[0][-1])
        assert base64.b64decode(wire['profile']) == profile.SerializeToString(deterministic=True)
