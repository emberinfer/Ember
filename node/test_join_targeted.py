"""Production targeted state-view contract and fail-closed trust."""
import json
from unittest.mock import patch, MagicMock
import pytest
from agent.join import _chain_state_targeted, _chain_state_with_retry, _chain_height
from agent.trust import check_model_trust


def test_targeted_view_fields_and_height():
    responses = {'/parameters': {'chain_id': 'ledger'},
                 '/model/model': {'publication': {'publisher': 'publisher', 'manifest_root': 'root'}},
                 '/node/node': {'registered_at': 12, 'last_proof_height': 20, 'expires_height': 40}}
    with patch('agent.join._chain_json', side_effect=lambda c, r, p: responses[p]) as query, \
            patch('agent.join._chain_height', return_value=31):
        state = _chain_state_targeted('cli', 'rpc', 'model', 'node')
    assert set(state) == {'parameters', 'models', 'nodes', 'height', 'governors', 'endorsements'}
    assert state == dict(parameters=responses['/parameters'], models={'model': responses['/model/model']},
                         nodes={'node': responses['/node/node']}, height=31, governors={}, endorsements={})
    assert [c.args[2] for c in query.call_args_list] == ['/parameters', '/model/model', '/node/node']
    with pytest.raises(ValueError, match='does not satisfy'):
        check_model_trust(state, 'model')


def _rpc_response(payload):
    response = MagicMock()
    response.__enter__.return_value.read.return_value = json.dumps(payload).encode()
    return response


def test_height_prefers_status():
    with patch('urllib.request.urlopen',
               return_value=_rpc_response(
                   {'result': {'sync_info': {'latest_block_height': '123'}}})) as request:
        assert _chain_height('http://localhost:26657/') == 123
    request.assert_called_once_with('http://localhost:26657/status', timeout=10)


def test_height_falls_back_when_status_fails():
    def fake_urlopen(url, timeout=10):
        if url.endswith('/status'):
            raise OSError('404 Not Found')
        return _rpc_response({'result': {'response': {'last_block_height': '77'}}})

    with patch('urllib.request.urlopen', side_effect=fake_urlopen) as request:
        assert _chain_height('http://localhost:26657') == 77
    assert [c.args[0] for c in request.call_args_list] == [
        'http://localhost:26657/status', 'http://localhost:26657/abci_info']


def test_height_falls_back_when_abci_info_missing():
    # Public RPC endpoints that 404 /abci_info still serve /status, so
    # the height is recovered from /status without ever depending on
    # the missing endpoint.
    def fake_urlopen(url, timeout=10):
        if url.endswith('/abci_info'):
            raise OSError('404 Not Found')
        return _rpc_response({'result': {'sync_info': {'latest_block_height': '55'}}})

    with patch('urllib.request.urlopen', side_effect=fake_urlopen):
        assert _chain_height('http://localhost:26657') == 55


def test_height_raises_when_both_endpoints_fail():
    with patch('urllib.request.urlopen', side_effect=OSError('404 Not Found')):
        with pytest.raises(RuntimeError, match='could not determine chain height'):
            _chain_height('http://localhost:26657')


def test_best_effort_node_lookup():
    with patch('agent.join._chain_json', side_effect=[{'chain_id': 'ledger'}, OSError('missing')]), \
            patch('agent.join._chain_height', return_value=31):
        view = _chain_state_targeted('cli', 'rpc', None, 'node')
    assert view['height'] == 31 and view['nodes'] == {} and view['models'] == {}


def test_height_failure_is_not_silent():
    with patch('agent.join._chain_json', return_value={'chain_id': 'ledger'}), \
            patch('agent.join._chain_height', side_effect=RuntimeError('could not determine chain height')):
        with pytest.raises(RuntimeError, match='could not determine chain height'):
            _chain_state_targeted('cli', 'rpc', None, 'node')
    view = {'parameters': {}, 'models': {}, 'nodes': {}, 'height': 7, 'governors': {}, 'endorsements': {}}
    with patch('agent.join._chain_state_targeted', side_effect=[OSError('busy')]*3+[view]) as query, \
            patch('agent.join.time.sleep') as sleep:
        assert _chain_state_with_retry('cli', 'rpc', None, 'node') is view
    assert query.call_count == 4
    assert [c.args[0] for c in sleep.call_args_list] == [1, 2, 4]
    with patch('agent.join._chain_state_targeted', side_effect=RuntimeError('no height')) as query, \
            patch('agent.join.time.sleep'):
        with pytest.raises(RuntimeError, match='no height'):
            _chain_state_with_retry('cli', 'rpc', None, 'node')
    assert query.call_count == 4
