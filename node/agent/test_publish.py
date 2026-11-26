import subprocess
from unittest.mock import patch

import pytest

from agent.publish import main, submit_publication


def run_publish(tmp_path, error):
    pending = tmp_path / 'submission-pending'
    inputs = dict(state_dir='state', model_path='weights', model_id='ember-new', profile_version='v1',
                  origin='https://example.org/model', revision='1' * 40, output=str(tmp_path),
                  policy='policy.json', chain_rpc='rpc', ledger_chain_id='ledger', device='cpu')
    cli = ['publish']
    for name, value in inputs.items():
        cli.extend(('--' + name.replace('_', '-'), value))
    with patch('agent.publish._chain_tx', side_effect=error), \
         patch('agent.publish.publish', side_effect=lambda opts: submit_publication(
             opts, tmp_path / 'identity.key', 1, {}, pending)), pytest.raises(SystemExit) as exc:
        main(cli)
    assert exc.value.code == 1
    return pending


@pytest.mark.parametrize('stderr', [
    '2026/11/26 11:00:00 DeliverTx code=1 log=model already published as ember-existing\n',
    "RPC error -32603 - Internal error: broadcast error on transaction validation: tx ABC "
    "is invalid: code=1, data=, log='model already published as ember-existing', codespace=''\n",
])
def test_publish_rejection_is_on_the_headline(tmp_path, capsys, stderr):
    pending = run_publish(tmp_path, subprocess.CalledProcessError(1, 'emberchain', stderr=stderr))
    assert capsys.readouterr().err == (
        'Publication stopped: chain rejected the publication: model already published as ember-existing\n')
    assert not pending.exists()


def test_publish_timeout_keeps_pending_marker(tmp_path, capsys):
    assert run_publish(tmp_path, subprocess.TimeoutExpired('emberchain', 60)).is_file()
    assert 'submission outcome is uncertain' in capsys.readouterr().err
