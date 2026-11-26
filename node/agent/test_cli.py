import sys
from unittest.mock import patch

import pytest

from agent.__main__ import main


@pytest.mark.parametrize('argv, returncode', [([], 0), (['unknown-command'], 2)])
def test_top_level_help(argv, returncode, capsys, monkeypatch):
    monkeypatch.setattr(sys, 'argv', ['ember-node', *argv])
    if returncode:
        with pytest.raises(SystemExit) as error:
            main()
        assert error.value.code == returncode
    else:
        main()
    output = capsys.readouterr()
    help_text = output.err if returncode else output.out
    assert 'usage: ember-node' in help_text
    for command in ('id', 'configure', 'join', 'profile', 'exit'):
        assert command in help_text
    assert '--engine' not in help_text
    if returncode:
        assert "unknown command 'unknown-command'" in help_text


@pytest.mark.parametrize('flags, target', [([], 5000), (['--stake-target', '6000'], 6000)])
def test_join_stake_target_cli(flags, target, monkeypatch):
    argv = ['ember-node', 'join', '--coordinator', 'seed', '--chain', 'profile', '--engine', 'mock']
    monkeypatch.setattr(sys, 'argv', argv + flags)
    monkeypatch.setenv('EMBER_STAKE_TARGET', '5000')
    with patch('agent.join.join') as join:
        main()
    assert join.call_args.args[0].stake_target == target
