"""Installed-layout trust policy regression.

The v0.9.2 release gate found that the standard operator route
(install to a fresh prefix, then configure against the installed
tree) produced a join.env with no EMBER_PROFILE_TRUST_POLICY: the
installer never shipped config/trust-policy.json into the prefix,
and configure only wrote the pointer when that file happened to
exist where it looked.  These tests reproduce the real layouts --
an installer-produced prefix and an installed runtime tree -- rather
than the single unpacked bundle tree in which the defect was
invisible.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]


def _network():
    return json.loads((ROOT / 'site/network.json').read_text())


def _profile_id():
    return _network()['current_profile']['id']


def _publisher():
    return _network()['current_profile']['publisher']


def _make_bundle(tmp_path):
    """A minimal release bundle tree, laid out as release/build.sh emits."""
    bundle = tmp_path / 'bundle'
    for sub in ('bin', 'lib', 'profiles', 'docs', 'config'):
        (bundle / sub).mkdir(parents=True)
        (bundle / sub / 'placeholder.txt').write_text('x')
    for source in (ROOT / 'config').iterdir():
        if source.is_file():
            shutil.copy(source, bundle / 'config' / source.name)
    shutil.copy(ROOT / 'site/network.json', bundle / 'network.json')
    shutil.copy(ROOT / 'deploy/ember-role.sh', bundle / 'ember-role')
    shutil.copy(ROOT / 'deploy/install-release.sh', bundle / 'install-release.sh')
    return bundle


def test_installer_installs_trust_policy(tmp_path):
    bundle = _make_bundle(tmp_path)
    prefix = tmp_path / 'prefix'
    subprocess.run(['bash', str(bundle / 'install-release.sh'),
                    '--role', 'validator', '--prefix', str(prefix)],
                   check=True, capture_output=True, text=True)
    installed = prefix / 'config' / 'trust-policy.json'
    assert installed.is_file()
    policy = json.loads(installed.read_text())
    assert policy['profiles'][_profile_id()]['publishers'] == [_publisher()]


def _make_installed_tree(tmp_path):
    """An installed prefix: runtime code under lib/ember, profiles/,
    network.json and config/ at the root, as the installer leaves it."""
    prefix = tmp_path / 'prefix'
    node_dest = prefix / 'lib' / 'ember' / 'node'
    shutil.copytree(ROOT / 'node', node_dest,
                    ignore=shutil.ignore_patterns(
                        'test_*.py', 'test_support.py', 'testdata',
                        '__pycache__', '*.pyc', 'requirements-*.txt'))
    # release/build.sh drops the repository discovery marker.
    (node_dest / '__init__.py').unlink(missing_ok=True)
    for name in ('core-py', 'proto'):
        shutil.copytree(ROOT / name, prefix / 'lib' / 'ember' / name,
                        ignore=shutil.ignore_patterns('__pycache__', '*.pyc',
                                                      'test_*.py'))
    profile = _profile_id()
    shutil.copytree(ROOT / 'chain-genesis' / profile,
                    prefix / 'profiles' / profile)
    shutil.copy(ROOT / 'site/network.json', prefix / 'network.json')
    (prefix / 'config').mkdir(parents=True)
    (prefix / 'bin').mkdir(parents=True)
    (prefix / 'var').mkdir(parents=True)
    return prefix


def _run_configure(prefix):
    env = dict(os.environ)
    lib = prefix / 'lib' / 'ember'
    env['PYTHONPATH'] = os.pathsep.join(
        [str(lib / 'node'), str(lib / 'core-py'), str(lib / 'proto')])
    result = subprocess.run(
        [sys.executable, '-c',
         'from agent.network import main; main()',
         '--network-parameters', str(prefix / 'network.json'),
         '--model-path', '/models/qwen',
         '--config', str(prefix / 'config' / 'join.env')],
        capture_output=True, text=True, env=env)
    assert result.returncode == 0, result.stderr
    fields = {}
    for line in (prefix / 'config' / 'join.env').read_text().splitlines():
        if '=' in line:
            key, value = line.split('=', 1)
            fields[key] = value.strip("'")
    return fields


def _assert_pointer_inside_prefix(prefix, fields):
    assert 'EMBER_PROFILE_TRUST_POLICY' in fields
    target = Path(fields['EMBER_PROFILE_TRUST_POLICY'])
    assert target.is_file()
    assert target.is_relative_to(prefix.resolve())
    policy = json.loads(target.read_text())
    assert _publisher() in policy['profiles'][_profile_id()]['publishers']
    return target


def test_configure_on_installed_tree_with_bundled_policy(tmp_path):
    prefix = _make_installed_tree(tmp_path)
    # What the fixed installer leaves in config/: an operator pin that
    # configure must keep, not overwrite, while writing the pointer.
    bundled = prefix / 'config' / 'trust-policy.json'
    custom = {'profiles': {_profile_id(): {
        'publishers': [_publisher(), 'operator-added-publisher']}}}
    bundled.write_text(json.dumps(custom))
    fields = _run_configure(prefix)
    target = _assert_pointer_inside_prefix(prefix, fields)
    assert target == bundled.resolve()
    assert json.loads(bundled.read_text()) == custom


def test_configure_on_installed_tree_generates_missing_policy(tmp_path):
    prefix = _make_installed_tree(tmp_path)
    assert not (prefix / 'config' / 'trust-policy.json').exists()
    fields = _run_configure(prefix)
    target = _assert_pointer_inside_prefix(prefix, fields)
    assert target == (prefix / 'config' / 'trust-policy.json').resolve()
