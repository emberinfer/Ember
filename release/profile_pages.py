#!/usr/bin/env python3
"""Generate profile release records from signed artifacts and verified chain state."""
import argparse
import base64
import hashlib
import html
import json
import re
import shlex
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT / 'proto'), str(ROOT / 'core-py')]
from ember_core import digest, node_id, verify
from ember_pb2 import ChainGenesis, ModelManifest
from google.protobuf.json_format import MessageToDict


def records(state, artifacts, rpc):
    """Validate everything before writing any page; no hand-maintained facts."""
    out = {}
    selected = {p.name for p in artifacts.iterdir() if p.is_dir() and (p / 'genesis.pb').is_file() and (p / 'manifest.pb').is_file()}
    by_hash = {m['profile_hash']: m for m in state['models'].values()}
    for ident, model in sorted(state['models'].items()):
        p = model.get('publication')
        if not p or ident not in selected:
            continue
        if not re.fullmatch(r'[a-z0-9][a-z0-9.-]{0,127}', ident) or '..' in ident:
            raise ValueError('profile ID is not a safe release path')
        graw = (artifacts / ident / 'genesis.pb').read_bytes()
        mraw = (artifacts / ident / 'manifest.pb').read_bytes()
        g, m = ChainGenesis.FromString(graw), ModelManifest.FromString(mraw)
        verify(g)
        verify(m)
        if (graw != base64.b64decode(p['genesis']) or mraw != base64.b64decode(p['manifest'])
                or hashlib.sha256(graw).hexdigest() != model['genesis_hash']
                or digest(g).hex() != model['profile_hash']
                or digest(m).hex() != p['manifest_root']
                or g.model_manifest_root != digest(m) or g.chain_id != ident or m.chain_id != ident
                or node_id(g.signer) != p['publisher'] or g.signer != m.signer):
            raise ValueError('signed artifact and chain anchor disagree: ' + ident)
        history, seen, cursor = [], set(), model
        while cursor:
            if cursor['profile_hash'] in seen:
                raise ValueError('cycle in profile version history')
            seen.add(cursor['profile_hash'])
            pub = cursor.get('publication', {})
            previous = pub.get('previous_profile_hash')
            history.append(dict(model_id=cursor['id'], profile_hash=cursor['profile_hash'],
                                previous_profile_hash=previous,
                                registration_height=pub.get('registered_height')))
            if previous and previous not in by_hash:
                raise ValueError('missing previous profile anchor')
            cursor = by_hash.get(previous)
        endorsements = [dict(account=k, **{n: v for n, v in value.items() if n != 'account'})
                        for k, value in sorted(state.get('endorsements', {}).get(p['manifest_root'], {}).items())
                        if not value.get('revoked')]
        origin = p['origin']
        names = [a.name for a in m.artifacts]
        download = ('python -c ' + shlex.quote(
            'from huggingface_hub import snapshot_download; snapshot_download(' +
            repr(origin['repository'].removeprefix('https://huggingface.co/')) +
            ', revision=' + repr(origin['revision']) + ", local_dir='./origin', allow_patterns=" + repr(names) + ')'))
        if not origin['repository'].startswith('https://huggingface.co/'):
            download = ('git clone ' + shlex.quote(origin['repository']) + ' origin\n'
                        'git -C origin checkout --detach ' + shlex.quote(origin['revision']) + '\n'
                        'git -C origin lfs pull')
        verify_command = ('ember-node profile verify --genesis genesis.pb --manifest manifest.pb '
                          '--model-path ./origin')
        out[ident] = dict(
            schema='ember-profile-release-v1', model_id=ident, rpc=rpc,
            chain_id=state['parameters']['chain_id'], snapshot_height=state['height'],
            registration_height=p['registered_height'], profile_hash=model['profile_hash'],
            manifest_root=p['manifest_root'], genesis_sha256=model['genesis_hash'],
            manifest_sha256=hashlib.sha256(mraw).hexdigest(), publisher=p['publisher'],
            origin=origin, version_history=history, endorsements=endorsements,
            invalid=p.get('invalid', False), execution_profile=g.execution_profile_ver,
            audit_parameters=MessageToDict(g.audit_params, preserving_proto_field_name=True),
            numeric_audit_policy=MessageToDict(g.numeric_audit_policy, preserving_proto_field_name=True),
            recompute_commands=[download, verify_command],
            conformance_commands=[verify_command + ' --conformance --device cpu'],
            chain_query_commands=[
                'emberchain query --rpc ' + shlex.quote(rpc) + ' --path /model/' + shlex.quote(ident),
                'emberchain query --rpc ' + shlex.quote(rpc) + ' --path /endorsements'],
            artifacts=MessageToDict(m, preserving_proto_field_name=True)['artifacts'])
    if not out:
        raise ValueError('no published profiles with release anchors')
    return out


def render(record):
    def esc(v): return html.escape(str(v))
    def dump(v): return esc(json.dumps(v, indent=2, sort_keys=True))
    def commands(v): return '<pre>' + esc('\n'.join(v)) + '</pre>'
    endorsers = ''.join('<li><code>' + esc(v['account']) + '</code> (height ' +
                        esc(v['height']) + ')</li>' for v in record['endorsements']) or '<li>None</li>'
    return f'''<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{esc(record['model_id'])} — Ember profile release</title>
<style>body{{max-width:72rem;margin:3rem auto;padding:0 1rem;font:16px/1.6 system-ui;background:#10141c;color:#e5eaf3}}a{{color:#9dcaff}}code,pre{{overflow-wrap:anywhere}}pre{{white-space:pre-wrap;padding:1rem;background:#1c2432}}h1{{font-size:1.6rem}}</style>
<a href="../">All profile records</a><h1>{esc(record['model_id'])}</h1>
<p>Generated from publisher-signed artifacts and a verified chain query. Chain:
<code>{esc(record['chain_id'])}</code>. Endorsement snapshot at height {esc(record['snapshot_height'])}.
Endorsements are data; serving requires your local trust policy. This record does not upgrade another network.</p>
<p>Publication status: {'PROVEN INVALID' if record['invalid'] else 'registered'}.</p>
<h2>Manifest and on-chain anchor</h2>
<p>Manifest root: <code>{esc(record['manifest_root'])}</code><br>
Profile hash: <code>{esc(record['profile_hash'])}</code><br>
Registration height: {esc(record['registration_height'])}<br>
Publisher: <code>{esc(record['publisher'])}</code></p>
<p><a href="genesis.pb">Signed profile</a> · <a href="manifest.pb">Signed manifest</a> ·
<a href="release.json">Machine-readable release record</a> · <a href="ledger-genesis.json">Ledger verification anchor</a></p>
<h2>Recompute from the pinned origin</h2><pre>{dump(record['origin'])}</pre>
{commands(record['recompute_commands'])}
<p>Use the pinned Ember runtime and download the two signed artifacts above first. Verification checks every
artifact's hash/size, both signatures and the domain-separated manifest root. A conformance probe does not
establish numerical calibration on other hardware.</p>
<h2>Version history</h2><pre>{dump(record['version_history'])}</pre>
<h2>Endorsements from chain state</h2><ul>{endorsers}</ul>
<p>This is a build-time snapshot. Query finalized state for current endorsements and revocations:</p>
{commands(record['chain_query_commands'])}
<h2>Executable conformance check</h2>{commands(record['conformance_commands'])}
<h2>Signed audit parameters and bands</h2><pre>{dump(record['audit_parameters'])}</pre>
<pre>{dump(record['numeric_audit_policy'])}</pre>
<h2>Artifact inventory</h2><pre>{dump(record['artifacts'])}</pre>
</html>
'''


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__)
    for name in ('rpc', 'ledger-genesis', 'artifacts', 'output'):
        p.add_argument('--' + name, required=True)
    p.add_argument('--chain-cli', default='emberchain')
    p.add_argument('--check', action='store_true', help='verify committed pages and their current chain data without rewriting')
    opts = p.parse_args(argv)
    verified = subprocess.check_output([opts.chain_cli, 'query', '--rpc', opts.rpc,
        '--path', '/state', '--verify-state', '--genesis', opts.ledger_genesis], text=True)
    proof, raw = verified.split('\n', 1)
    if 'commit=genesis-verified' not in proof:
        raise ValueError('chain query did not verify its consensus commitment')
    state = json.loads(raw)
    releases = records(state, Path(opts.artifacts), opts.rpc)
    output = Path(opts.output)
    if opts.check:
        committed_ids = {p.parent.name for p in output.glob("*/release.json") if json.loads(p.read_text()).get("rpc") == opts.rpc}
        if committed_ids != set(releases):
            raise ValueError("committed profile catalog differs from selected signed artifacts")
        for ident, fresh in releases.items():
            target = output / ident
            committed = json.loads((target / 'release.json').read_text())
            stable = {k: v for k, v in committed.items() if k not in ('snapshot_height', 'state_verification')}
            if stable != {k: v for k, v in fresh.items() if k != 'snapshot_height'}:
                raise ValueError('release record and current signed artifacts/chain disagree: ' + ident)
            if (target / 'index.html').read_text() != render(committed):
                raise ValueError('rendered page differs from validated machine record: ' + ident)
            for name in ('genesis.pb', 'manifest.pb'):
                if (target / name).read_bytes() != (Path(opts.artifacts) / ident / name).read_bytes():
                    raise ValueError('downloadable artifact mismatch: ' + ident)
            if (target / 'ledger-genesis.json').read_bytes() != Path(opts.ledger_genesis).read_bytes():
                raise ValueError('ledger verification anchor mismatch')
        print(json.dumps(dict(result='PASS', checked=list(releases), height=state['height'])))
        return
    output.mkdir(parents=True, exist_ok=True)
    for ident, record in releases.items():
        target = output / ident
        target.mkdir(exist_ok=True)
        record['state_verification'] = proof
        (target / 'release.json').write_text(json.dumps(record, indent=2, sort_keys=True) + '\n')
        (target / 'index.html').write_text(render(record))
        for name in ('genesis.pb', 'manifest.pb'):
            (target / name).write_bytes((Path(opts.artifacts) / ident / name).read_bytes())
        (target / 'ledger-genesis.json').write_bytes(Path(opts.ledger_genesis).read_bytes())
    links = ''.join('<li><a href="' + html.escape(k) + '/">' + html.escape(k) + '</a></li>' for k in releases)
    (output / 'index.html').write_text('<!doctype html><html lang="en"><meta charset="utf-8"><title>Ember profile releases</title><h1>Profile release records</h1><p>Each record names its chain and snapshot height. Each record applies only to its named network.</p><ul>' + links + '</ul></html>\n')
    print(json.dumps(dict(result='PASS', profiles=list(releases), height=state['height'])))


if __name__ == '__main__':
    main()
