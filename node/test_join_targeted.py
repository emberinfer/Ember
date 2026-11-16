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


def test_height_uses_abci_info():
    response = MagicMock()
    response.__enter__.return_value.read.return_value = json.dumps(
        {'result': {'response': {'last_block_height': '123'}}}).encode()
    with patch('urllib.request.urlopen', return_value=response) as request:
        assert _chain_height('http://localhost:26657/') == 123
    request.assert_called_once_with('http://localhost:26657/abci_info', timeout=10)


def test_best_effort_node_height_and_retry():
    with patch('agent.join._chain_json', side_effect=[{'chain_id': 'ledger'}, OSError('missing')]), \
            patch('agent.join._chain_height', side_effect=OSError('offline')):
        view = _chain_state_targeted('cli', 'rpc', None, 'node')
    assert view['height'] == 0 and view['nodes'] == {} and view['models'] == {}
    with patch('agent.join._chain_state_targeted', side_effect=[OSError('busy')]*3+[view]) as query, \
            patch('agent.join.time.sleep') as sleep:
        assert _chain_state_with_retry('cli', 'rpc', None, 'node') is view
    assert query.call_count == 4
    assert [c.args[0] for c in sleep.call_args_list] == [1, 2, 4]
