"""Turn a public network artifact into a node config without operator secrets."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import sys


def validate_artifacts(model, ident):
    """Authenticate exact protobuf bytes against an operator-trusted record."""
    import base64
    from ember_core import verify, digest, node_id
    from ember_pb2 import ChainGenesis, ModelManifest
    if not re.fullmatch(r'ember-[a-zA-Z0-9_.-]{1,192}', ident) or model.get('id') != ident:
        raise ValueError('model ID mismatch')
    publication = model.get('publication')
    if publication and publication.get('invalid'):
        raise ValueError('publication invalid')
    source = publication or model
    graw = base64.b64decode(source.get('genesis' if publication else 'signed_profile') or '', validate=True)
    mraw = base64.b64decode(source.get('manifest' if publication else 'signed_manifest') or '', validate=True)
    if not graw or not mraw or len(graw) + len(mraw) > 16 << 20:
        raise ValueError('signed artifacts unavailable or exceed limit')
    g, m = ChainGenesis.FromString(graw), ModelManifest.FromString(mraw)
    verify(g); verify(m)
    gh, mh = hashlib.sha256(graw).hexdigest(), hashlib.sha256(mraw).hexdigest()
    if (g.chain_id != ident or m.chain_id != ident or g.signer != m.signer or
            digest(m) != g.model_manifest_root or gh != model.get('genesis_hash')):
        raise ValueError('artifact anchor mismatch')
    if publication:
        if (publication.get('publisher') != node_id(g.signer) or
                publication.get('manifest_root') != digest(m).hex() or
                model.get('profile_hash') != digest(g).hex()):
            raise ValueError('publication anchor mismatch')
    elif model.get('profile_hash') != mh:
        raise ValueError('legacy manifest SHA256 mismatch')
    return graw, mraw, dict(id=ident, genesis_sha256=gh, manifest_sha256=mh)


def fetch_profile(document, ident, profiles, rpc, cli, trust_policy=None):
    # Explicit --chain-rpc is the minimum operator trust pin. Ordinary ABCI
    # JSON is not a Merkle proof and cannot establish trust in an unknown RPC.
    if not rpc:
        raise ValueError('unbundled profiles require operator-pinned --chain-rpc')
    if not re.fullmatch(r'ember-[a-zA-Z0-9_.-]{1,192}', ident):
        raise ValueError('invalid profile ID')
    from .join import _chain_json
    from .trust import check_model_trust
    model = _chain_json(cli, rpc, '/model/' + ident)
    state = _chain_json(cli, rpc, '/model-trust/' + ident)
    parameters = _chain_json(cli, rpc, '/parameters')
    if parameters['chain_id'] != document['ledger']['chain_id']:
        raise ValueError('ledger chain ID mismatch')
    if model != state['models'][ident]:
        raise ValueError('model changed during trust query; retry configure')
    graw, mraw, profile = validate_artifacts(model, ident)
    check_model_trust(state, ident, trust_policy)
    target = Path(profiles) / ident
    target.mkdir(parents=True, exist_ok=True)
    for name, raw in [('genesis.pb', graw), ('manifest.pb', mraw)]:
        path = target / name
        if path.exists() and path.read_bytes() != raw:
            raise ValueError('cached profile bytes differ: ' + name)
        if not path.exists():
            with path.open('xb') as stream:
                stream.write(raw)
    document['current_profile'] = profile
    document['access']['rpc'] = rpc
    return profile


def gateway_coordinators(access, profile, *, same_host_684=False):
    selected = []
    matched = False
    checked = skipped = 0
    for private, gateways in ((False, access.get('gateways', [])),
                              (True, access.get('private_gateways', []))):
        for gateway in gateways:
            checked += 1
            ident = str(gateway.get('id', gateway.get('url')))
            if 'profiles' not in gateway and gateway.get('dynamic_profiles') is not True:
                if 'node_tunnel' in gateway:
                    raise ValueError('gateway directory lacks exact profile membership: ' + ident)
                skipped += 1
                print(f'{ident} skipped: no profile membership declared', file=sys.stderr)
                continue
            if profile not in gateway.get('profiles', []) and gateway.get('dynamic_profiles') is not True:
                continue
            matched = True
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
        raise ValueError(f'no node gateways for profile {profile} (checked {checked} entries, skipped {skipped}); '
                         'use network.json from the release package or select a node ingress with explicit --gateway')
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


def resolve_trust_policy(document, config_target, profiles=None):
    """Return the local trust-policy path a generated config must pin.

    The policy lives at config/trust-policy.json under the installation
    root (the directory holding the generated config, in the standard
    layout).  Look, in order, at the config directory itself, at the
    installation/bundle root derived from this module's location (both
    the installed lib/ember/node/agent layout and the repository
    node/agent layout), and at the root implied by the profiles
    directory.  An existing file is an operator pin: it is used as-is
    and never overwritten.  If no candidate exists, generate the file
    next to the generated config from the network document's
    current_profile.publisher so a standard installation works even
    when the bundled example is missing.
    """
    config_dir = Path(config_target).expanduser().parent
    candidates = [config_dir / 'trust-policy.json']
    here = Path(__file__).resolve()
    roots = []
    if len(here.parents) > 4:
        roots.append(here.parents[4])
    if len(here.parents) > 2:
        roots.append(here.parents[2])
    if profiles:
        roots.append(Path(profiles).expanduser().parent)
    for root in roots:
        candidates.append(root / 'config' / 'trust-policy.json')
    seen = set()
    for candidate in candidates:
        key = str(candidate)
        if key in seen:
            continue
        seen.add(key)
        if candidate.is_file():
            return str(candidate)
    profile = document.get('current_profile') or {}
    profile_id, publisher = profile.get('id'), profile.get('publisher')
    if not isinstance(profile_id, str) or not profile_id or \
            not isinstance(publisher, str) or not publisher:
        return None
    generated = config_dir / 'trust-policy.json'
    config_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        fd = os.open(generated, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o644)
    except FileExistsError:
        return str(generated)
    with os.fdopen(fd, 'w') as f:
        f.write(json.dumps({'profiles': {profile_id: {'publishers': [publisher]}}}, indent=2) + '\n')
    return str(generated)


def main(args=None):
    parser = argparse.ArgumentParser(prog='ember-node configure')
    parser.add_argument('--network-parameters', required=True)
    parser.add_argument('--model-path', required=True)
    parser.add_argument('--config', required=True)
    parser.add_argument('--profiles', default=str(Path(__file__).resolve().parents[4]/'profiles'))
    parser.add_argument('--profile-id')
    parser.add_argument('--chain-rpc', help='operator-pinned trusted ledger RPC (not a light-client proof)')
    parser.add_argument('--chain-cli', default='emberchain')
    parser.add_argument('--trust-policy')
    parser.add_argument('--gateway', help='explicit gateway directory ID or URL')
    parser.add_argument('--same-host-684', action='store_true', help='explicitly attest client runs on gateway host 684')
    opts = parser.parse_args(args)
    document = json.loads(Path(opts.network_parameters).read_text())
    if opts.profile_id and not re.fullmatch(r'ember-[a-zA-Z0-9_.-]{1,192}', opts.profile_id):
        raise ValueError('invalid profile ID')
    dynamic = opts.profile_id and (opts.profile_id != document.get('current_profile', {}).get('id') or
        not all((Path(opts.profiles) / opts.profile_id / name).is_file() for name in ('genesis.pb', 'manifest.pb')))
    if dynamic:
        fetch_profile(document, opts.profile_id, opts.profiles, opts.chain_rpc,
                      opts.chain_cli, opts.trust_policy)
    if opts.gateway:
        private = document['access'].get('private_gateways', [])
        gateways = document['access'].get('gateways', []) + private
        chosen = [g for g in gateways if opts.gateway in (g.get('id'), g.get('url'))]
        if len(chosen) != 1:
            raise ValueError('explicit gateway not found or ambiguous')
        is_private = chosen[0] in private
        chosen[0] = dict(chosen[0], profiles=[document['current_profile']['id']])
        document['access']['gateways'] = [] if is_private else chosen
        document['access']['private_gateways'] = chosen if is_private else []
    # Validate the document (and the bundled profile binding) before
    # resolving the trust policy, so a refused configure leaves no
    # generated policy file behind.
    content = node_config(document, opts.model_path, opts.profiles, same_host_684=opts.same_host_684)
    # Point the generated config at the installation's trust policy
    # (config/trust-policy.json) so a published profile passes the
    # node's local publisher check out of the box; operators may
    # replace the file with their own pin.  See resolve_trust_policy:
    # an existing file is kept, a missing one is generated from the
    # network document's current_profile.publisher.
    trust_policy = opts.trust_policy or (None if dynamic else resolve_trust_policy(document, opts.config, opts.profiles))
    if trust_policy:
        content += 'EMBER_PROFILE_TRUST_POLICY=' + shlex.quote(str(trust_policy)) + '\n'
    target = Path(opts.config)
    target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    # Never overwrite an operator's existing configuration implicitly.
    fd = os.open(target, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(fd, 'w') as f:
        f.write(content)
    print('Node configuration written:', target)
