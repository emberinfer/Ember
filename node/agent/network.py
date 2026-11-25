"""Turn a public network artifact into a node config without operator secrets."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import sys


def gateway_coordinators(access, profile, *, same_host_684=False):
    selected = []
    matched = False
    for private, gateways in ((False, access.get('gateways', [])),
                              (True, access.get('private_gateways', []))):
        for gateway in gateways:
            if 'profiles' not in gateway:
                raise ValueError('gateway directory lacks exact profile membership')
            if profile not in gateway['profiles']:
                continue
            matched = True
            ident = str(gateway.get('id', gateway.get('url')))
            if private and not same_host_684:
                print(f'MULTIHOME excluded={ident} reason=private gateway requires same-host-684 context', file=sys.stderr)
                continue
            tunnel = gateway.get('node_tunnel', {})
            entry = dict(endpoint=tunnel.get('local_endpoint' if private else 'endpoint'),
                         tls_fingerprint=tunnel.get('local_tls_fingerprint' if private else 'tls_fingerprint'))
            if not entry['endpoint'] or not entry['tls_fingerprint']:
                raise ValueError('gateway node ingress missing: ' + ident)
            selected.append(entry)
    if not matched:
        raise ValueError('no node gateways for profile: ' + profile)
    from .multihome import coordinators
    if selected:
        coordinators(selected)
    else:
        print(f'MULTIHOME profile={profile} connected=0/0 missing=[] reason=no public gateway; same-host-684 required', file=sys.stderr)
    return selected


def node_config(document, model_path, profiles, *, same_host_684=False, trust_policy=None):
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
    scheme_hash = profile.get('scheme_hash')
    if scheme_hash:
        if not isinstance(scheme_hash, str) or not re.fullmatch('[0-9a-fA-F]{64}', scheme_hash):
            raise ValueError('invalid hash field: current_profile.scheme_hash')
        fields['EMBER_SCHEME_HASH'] = scheme_hash
    if trust_policy:
        fields['EMBER_PROFILE_TRUST_POLICY'] = str(trust_policy)
    ident = fields['EMBER_MODEL_CHAIN']
    if not re.fullmatch(r'ember-[a-zA-Z0-9_.-]+', ident):
        raise ValueError('invalid profile identity')
    selected = gateway_coordinators(access, fields['EMBER_MODEL_CHAIN'], same_host_684=same_host_684)
    fields['EMBER_COORDINATOR'] = json.dumps(selected, separators=(',', ':'))
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
    parser.add_argument('--same-host-684', action='store_true', help='explicitly attest client runs on gateway host 684')
    opts = parser.parse_args(args)
    # Point the generated config at the trust policy example bundled
    # with the release (config/trust-policy.json next to profiles/) so
    # a published profile passes the node's local publisher check out
    # of the box; operators may replace it with their own pin.
    bundled_policy = Path(__file__).resolve().parents[4] / 'config' / 'trust-policy.json'
    content = node_config(json.loads(Path(opts.network_parameters).read_text()), opts.model_path, opts.profiles, same_host_684=opts.same_host_684,
                          trust_policy=str(bundled_policy) if bundled_policy.is_file() else None)
    target = Path(opts.config)
    target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    # Never overwrite an operator's existing configuration implicitly.
    fd = os.open(target, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(fd, 'w') as f:
        f.write(content)
    print('Node configuration written:', target)
