"""Default-full registration, legacy CLI semantics, and local refusal boundaries."""
from types import SimpleNamespace
from unittest.mock import patch
import sys
import pytest
from ember_pb2 import EngineCapabilities, ResourceProfile, TunnelFrame
from agent.registration_roles import opt_outs


@pytest.mark.parametrize('roles', [('model',), ('shard-stage',), ('model', 'auditor'), ()])
def test_default_full_independent_of_engine_roles(roles):
    assert opt_outs(SimpleNamespace(), EngineCapabilities(roles=roles)) == dict(no_audit=False, no_exec=False)


@pytest.mark.parametrize('roles,no_exec', [(('model',), False), (('shard-stage',), False), ((), True), (('auditor',), True)])
def test_legacy_audit_role_preserves_actual_previous_roles(roles, no_exec):
    assert opt_outs(SimpleNamespace(audit_role=True), EngineCapabilities(roles=roles)) == dict(no_audit=False, no_exec=no_exec)


def test_conflicting_legacy_flag_fails():
    with pytest.raises(ValueError, match='conflicts'):
        opt_outs(SimpleNamespace(audit_role=True, no_audit=True))


@pytest.mark.parametrize('flags', [[], ['--audit-role'], ['--no-audit'], ['--no-exec'], ['--no-audit', '--no-exec']])
def test_join_cli_accepts_legacy_and_opt_outs(flags):
    from agent.__main__ import main
    with patch.object(sys, 'argv', ['ember-node', 'join', '--coordinator', 'local', '--chain', 'model', '--engine', 'mock', *flags]), patch('agent.join.join') as join:
        main()
        args = join.call_args.args[0]
        assert args.audit_role == ('--audit-role' in flags)
        assert args.no_audit == ('--no-audit' in flags)
        assert args.no_exec == ('--no-exec' in flags)


def test_registration_transaction_contains_explicit_opt_outs(tmp_path):
    from agent.join import register_on_chain
    args = SimpleNamespace(chain_cli='unused', ledger_chain_id='local', chain_rpc='unused', no_exec=True, no_audit=False)
    profile = ResourceProfile(node_id='node', chain_id='model')
    state = dict(height=10, parameters=dict(registration_ttl_blocks=100), accounts={})
    with patch('agent.join._chain_json', side_effect=[dict(nonce=0, stake=5000), dict(profile_hash='00'), dict(registration_ttl_blocks=100), state]), patch('agent.join._chain_state_targeted', return_value=state), patch('agent.join._chain_tx') as submit:
        register_on_chain(args, tmp_path, 'node', profile)
    action = submit.call_args.args[-1]
    assert action['no_exec'] is True and action['no_audit'] is False
    assert action['expires_height'] == 110


def test_review_opt_out_refused_before_chain_query():
    from auditor.worker import execute
    agent = SimpleNamespace(chain_args=SimpleNamespace(no_audit=True, chain_rpc='unused'))
    with patch('agent.join._chain_json') as query, pytest.raises(ValueError, match='audit role'):
        execute(agent, TunnelFrame(), {})
    query.assert_not_called()


def test_default_review_worker_reaches_chain_assignment_validation():
    from auditor.worker import execute
    agent = SimpleNamespace(chain_args=SimpleNamespace(chain_rpc='unused'), genesis_bytes=b'anchor')
    with pytest.raises(ValueError, match='genesis differs'):
        execute(agent, TunnelFrame(), dict(genesis=b'other'.hex()))


def test_local_exec_opt_out_refuses_work():
    import queue
    from agent import Agent
    from engine import load_engine
    agent = Agent('test', load_engine('mock'))
    agent.chain_args = SimpleNamespace(no_exec=True)
    outgoing = queue.Queue()
    agent._run(TunnelFrame(kind='run', request_id='blocked', payload=b'{}'), outgoing)
    reply = outgoing.get_nowait()
    assert reply.kind == 'unavailable' and b'opted out' in reply.payload
