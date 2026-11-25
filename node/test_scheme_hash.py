"""Scheme-hash pinning: env fallback, flag precedence, launcher forwarding."""
import json
import os
from pathlib import Path
import subprocess
from types import SimpleNamespace

import pytest
from agent.join import resolve_scheme_hash
from agent.network import node_config

ROOT = Path(__file__).resolve().parents[1]
SCHEME_HASH = '400120b65600b51587238ea514a711bb57c68d267194e6ed400a1e2b64a7a142'


def _document():
    d = json.loads((ROOT / 'site/network.json').read_text())
    d['access'].update(seed='seed.example:19091', rpc='https://rpc.example',
                       tls_fingerprint='a' * 64, scheduler_id='b' * 64)
    return d


def test_configure_emits_scheme_hash():
    out = node_config(_document(), '/models/qwen', ROOT / 'chain-genesis')
    assert f'EMBER_SCHEME_HASH={SCHEME_HASH}' in out


def test_configure_emits_trust_policy_pointer():
    out = node_config(_document(), '/models/qwen', ROOT / 'chain-genesis',
                      trust_policy='/cfg/trust-policy.json')
    assert 'EMBER_PROFILE_TRUST_POLICY=/cfg/trust-policy.json' in out


def test_configure_rejects_bad_scheme_hash():
    d = _document()
    d['current_profile']['scheme_hash'] = 'not-hex'
    with pytest.raises(ValueError, match='scheme_hash'):
        node_config(d, '/models/qwen', ROOT / 'chain-genesis')

ENV_HASH = 'a' * 64
FLAG_HASH = 'b' * 64


def test_env_scheme_hash_is_adopted():
    args = SimpleNamespace(scheme_hash=None)
    assert resolve_scheme_hash(args, {'EMBER_SCHEME_HASH': ENV_HASH}) == ENV_HASH
    assert args.scheme_hash == ENV_HASH


def test_flag_overrides_env():
    args = SimpleNamespace(scheme_hash=FLAG_HASH)
    assert resolve_scheme_hash(args, {'EMBER_SCHEME_HASH': ENV_HASH}) == FLAG_HASH
    assert args.scheme_hash == FLAG_HASH


def test_no_source_leaves_unset():
    args = SimpleNamespace(scheme_hash=None)
    assert resolve_scheme_hash(args, {}) is None
    assert args.scheme_hash is None
    assert resolve_scheme_hash(args, {'EMBER_SCHEME_HASH': ''}) is None


def test_ember_role_forwards_scheme_hash_and_trust_policy(tmp_path):
    repo = Path(__file__).resolve().parents[1]
    root = tmp_path / 'bundle'
    (root / 'bin').mkdir(parents=True)
    launcher = root / 'bin' / 'ember-role'
    launcher.write_text((repo / 'deploy/ember-role.sh').read_text())
    stub = root / 'bin' / 'ember-node'
    stub.write_text('#!/usr/bin/env bash\nprintf "%s\\n" "$@"\n')
    stub.chmod(0o755)
    config = root / 'join.env'
    config.write_text('\n'.join([
        'EMBER_COORDINATOR=seed.example:19091',
        'EMBER_TLS_FINGERPRINT=' + 'c' * 64,
        'EMBER_MODEL_CHAIN=ember-qwen2.5-1.5b-unified-v1',
        'EMBER_MODEL_PATH=/models/qwen',
        'EMBER_SCHEDULER_ID=' + 'd' * 64,
        'EMBER_CHAIN_RPC=http://127.0.0.1:26657',
        'EMBER_LEDGER_CHAIN_ID=ember-testnet-1',
        'EMBER_GENESIS_SHA256=' + 'e' * 64,
        'EMBER_MANIFEST_SHA256=' + 'f' * 64,
        'EMBER_SCHEME_HASH=' + ENV_HASH,
        'EMBER_PROFILE_TRUST_POLICY=/cfg/trust-policy.json',
        '',
    ]))
    out = subprocess.run(['bash', str(launcher), 'node', str(config)],
                         capture_output=True, text=True, check=True).stdout.splitlines()
    assert out[out.index('--scheme-hash') + 1] == ENV_HASH
    assert out[out.index('--trust-policy') + 1] == '/cfg/trust-policy.json'


def test_ember_role_omits_scheme_hash_when_unset(tmp_path):
    repo = Path(__file__).resolve().parents[1]
    root = tmp_path / 'bundle'
    (root / 'bin').mkdir(parents=True)
    launcher = root / 'bin' / 'ember-role'
    launcher.write_text((repo / 'deploy/ember-role.sh').read_text())
    stub = root / 'bin' / 'ember-node'
    stub.write_text('#!/usr/bin/env bash\nprintf "%s\\n" "$@"\n')
    stub.chmod(0o755)
    config = root / 'join.env'
    config.write_text('\n'.join([
        'EMBER_COORDINATOR=seed.example:19091',
        'EMBER_TLS_FINGERPRINT=' + 'c' * 64,
        'EMBER_MODEL_CHAIN=ember-qwen2.5-1.5b-unified-v1',
        'EMBER_MODEL_PATH=/models/qwen',
        'EMBER_SCHEDULER_ID=' + 'd' * 64,
        'EMBER_CHAIN_RPC=http://127.0.0.1:26657',
        'EMBER_LEDGER_CHAIN_ID=ember-testnet-1',
        'EMBER_GENESIS_SHA256=' + 'e' * 64,
        'EMBER_MANIFEST_SHA256=' + 'f' * 64,
        '',
    ]))
    env = {k: v for k, v in os.environ.items()
           if k not in ('EMBER_SCHEME_HASH', 'EMBER_PROFILE_TRUST_POLICY')}
    out = subprocess.run(['bash', str(launcher), 'node', str(config)],
                         capture_output=True, text=True, check=True, env=env).stdout
    assert '--scheme-hash' not in out
    assert '--trust-policy' not in out
