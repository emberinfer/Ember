"""Execution role exit submits with an existing identity on the selected ledger."""
import json
from unittest.mock import patch
import pytest
from agent.lifecycle import request_exec_exit_main


def args(tmp_path):
    return ['--state-dir', str(tmp_path), '--chain-rpc', 'unused', '--ledger-chain-id', 'test']


def test_exec_exit_requires_existing_identity(tmp_path):
    with patch('agent.lifecycle.identity') as identity, pytest.raises(ValueError, match='existing'):
        request_exec_exit_main(args(tmp_path))
    identity.assert_not_called()
    assert not (tmp_path / 'identity.key').exists()


def test_exec_exit_chain_mismatch(tmp_path):
    (tmp_path / 'identity.key').touch()
    with patch('agent.lifecycle.identity', return_value=(None, 'node')), patch('agent.lifecycle._chain_json', return_value={'chain_id': 'wrong'}), patch('agent.lifecycle._chain_tx') as tx, pytest.raises(ValueError, match='mismatch'):
        request_exec_exit_main(args(tmp_path))
    tx.assert_not_called()


def test_exec_exit_submit_and_status(tmp_path, capsys):
    (tmp_path / 'identity.key').touch()
    with patch('agent.lifecycle.identity', return_value=(None, 'node')), patch('agent.lifecycle._chain_json', side_effect=[{'chain_id': 'test'}, {'nonce': 7}, {'height': 10, 'exec_exit_pending': True, 'exec_exit_effective_height': 1010}]) as query, patch('agent.lifecycle._chain_tx') as tx:
        request_exec_exit_main(args(tmp_path))
    assert tx.call_args.args[1:] == (tmp_path / 'identity.key', 8, {'type': 'request_exec_exit'})
    assert query.call_args.args[-1] == '/node-status/node'
    lines = capsys.readouterr().out.splitlines()
    assert json.loads(lines[0]) == dict(node_id='node', height=10, exec_exit_pending=True, exec_exit_effective_height=1010, exec_exited=False)
    assert '1010' in lines[1]


def test_exec_exit_dispatch():
    from agent.__main__ import main
    with patch('sys.argv', ['ember-node', 'request-exec-exit', '--help']), patch('agent.lifecycle.request_exec_exit_main') as command:
        main()
    command.assert_called_once_with(['--help'])
