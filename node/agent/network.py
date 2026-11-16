"""Turn a public network artifact into a node config without operator secrets."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex


def node_config(document, model_path, profiles):
    if document.get('schema_version') != 1:
        raise ValueError('unsupported network-parameters schema')
    access, ledger, profile = (document[k] for k in ('access', 'ledger', 'current_profile'))
    fields = {
        'EMBER_COORDINATOR': access.get('seed'),
        'EMBER_TLS_FINGERPRINT': access.get('tls_fingerprint'),
        'EMBER_SCHEDULER_ID': access.get('scheduler_id'),
        'EMBER_CHAIN_RPC': access.get('rpc'),
        'EMBER_LEDGER_CHAIN_ID': ledger.get('chain_id'),
        'EMBER_MODEL_CHAIN': profile.get('id'),
        'EMBER_GENESIS_SHA256': profile.get('genesis_sha256'),
        'EMBER_MANIFEST_SHA256': profile.get('manifest_sha256'),
    }
    if document.get('public_testnet') != 'active' or any(not isinstance(v, str) or not v for v in fields.values()):
        raise ValueError('public network is not launched: complete public access parameters are required')
    for key in ['EMBER_TLS_FINGERPRINT', 'EMBER_SCHEDULER_ID', 'EMBER_GENESIS_SHA256', 'EMBER_MANIFEST_SHA256']:
        if not re.fullmatch('[0-9a-fA-F]{64}', fields[key]):
            raise ValueError('invalid hash/account field: ' + key)
    ident = fields['EMBER_MODEL_CHAIN']
    if not re.fullmatch(r'ember-[a-zA-Z0-9_.-]+', ident):
        raise ValueError('invalid profile identity')
    for name, key in [('genesis.pb', 'EMBER_GENESIS_SHA256'), ('manifest.pb', 'EMBER_MANIFEST_SHA256')]:
        if hashlib.sha256((Path(profiles)/ident/name).read_bytes()).hexdigest() != fields[key].lower():
            raise ValueError('network artifact differs from bundled profile: ' + name)
    fields.update(EMBER_MODEL_PATH=str(Path(model_path).expanduser()), EMBER_DEVICE='cpu',
                  EMBER_REGISTRATION_TTL='200000', EMBER_NODE_HEALTH='127.0.0.1:19192')
    return ''.join(k + '=' + shlex.quote(v) + '\n' for k, v in fields.items())


def main(args=None):
    parser = argparse.ArgumentParser(prog='ember-node configure')
    parser.add_argument('--network-parameters', required=True)
    parser.add_argument('--model-path', required=True)
    parser.add_argument('--config', required=True)
    parser.add_argument('--profiles', default=str(Path(__file__).resolve().parents[4]/'profiles'))
    opts = parser.parse_args(args)
    content = node_config(json.loads(Path(opts.network_parameters).read_text()), opts.model_path, opts.profiles)
    target = Path(opts.config)
    target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    # Never overwrite an operator's existing configuration implicitly.
    fd = os.open(target, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(fd, 'w') as f:
        f.write(content)
    print('Node configuration written:', target)
