#!/usr/bin/env python3
"""Loopback-only validator membership acceptance with isolated local processes.

Run after building emberchain; PYTHONPATH must include core-py and proto.
The output directory must not exist. No remote hosts or existing homes are used.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import time
import urllib.parse
import urllib.request

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
import ember_core
import ember_pb2


def b64(raw):
    return base64.b64encode(raw).decode()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--legacy-binary', type=Path, help='exercise a version-4 in-place upgrade and 2->3 join instead')
    parser.add_argument('--resume', action='store_true', help='resume the existing four-member drill at voluntary exit')
    args = parser.parse_args()
    binary, root = args.binary.resolve(), args.out.resolve()
    start_binary = args.legacy_binary.resolve() if args.legacy_binary else binary
    upgrade_manifest = None
    initial_count = 2 if args.legacy_binary else 3
    root.mkdir(parents=True, exist_ok=args.resume)
    os.chmod(root, 0o700)
    chain_id = (json.loads((root / 'genesis.json').read_text())['chain_id'] if args.resume
                else 'ember-validator-local-' + str(time.time_ns()))
    procs, handles = {}, []
    events = json.loads((root / 'events.json').read_text()) if args.resume else []
    ports = []
    # Reserve random loopback ports; release before process startup.
    sockets = []
    for _ in range(8):
        sock = socket.socket()
        sock.bind(('127.0.0.1', 0))
        ports.append(sock.getsockname()[1])
        sockets.append(sock)
    p2ps, rpcs = ports[:4], ports[4:]

    def record(name, **values):
        event = dict(event=name, **values)
        events.append(event)
        print(json.dumps(event), flush=True)
        (root / 'events.json').write_text(json.dumps(events, indent=2) + '\n')

    def run(*cmd, check=True):
        result = subprocess.run([str(binary), *map(str, cmd)], text=True, capture_output=True)
        with (root / 'commands.log').open('a') as log:
            log.write(json.dumps([str(binary), *map(str, cmd)]) + '\n')
            log.write(result.stdout + result.stderr)
        if check and result.returncode:
            raise RuntimeError(result.stderr)
        return result

    def rpc(method, index=0, **params):
        url = f'http://127.0.0.1:{rpcs[index]}/{method}?{urllib.parse.urlencode(params)}'
        with urllib.request.urlopen(url, timeout=3) as response:
            data = json.load(response)
        if 'error' in data:
            raise RuntimeError(data['error'])
        return data['result']

    def state(index=0):
        res = rpc('abci_query', index, path='"/state"')['response']
        assert res['code'] == 0, res
        return json.loads(base64.b64decode(res['value']))

    def height(index=0):
        return int(rpc('status', index)['sync_info']['latest_block_height'])

    def wait_height(target, index=0, timeout=60):
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            try:
                if height(index) >= target:
                    return height(index)
            except (OSError, ValueError, RuntimeError):
                pass
            time.sleep(0.2)
        raise RuntimeError(f'height {target} not reached on node {index}')

    keys, operators, validators, peers = [], [], [], []
    for i in range(4):
        key = (Ed25519PrivateKey.from_private_bytes((root / f'operator-{i}.key').read_bytes())
               if args.resume else Ed25519PrivateKey.generate())
        seed = key.private_bytes(serialization.Encoding.Raw, serialization.PrivateFormat.Raw,
                                 serialization.NoEncryption())
        public = key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
        key_path = root / f'operator-{i}.key'
        key_path.write_bytes(seed)
        os.chmod(key_path, 0o600)
        keys.append(key)
        operators.append(ember_core.node_id(public))
        home = root / f'node-{i}'
        if not args.resume:
            run('init-validator', '--home', home)
            run('validator-public', '--home', home, '--name', f'local-{i}', '--out', root / f'public-{i}.json')
        desc = json.loads((root / f'public-{i}.json').read_text())
        validators.append(dict(pubkey=b64(bytes.fromhex(desc['validator_public_key'])),
                               operator=operators[-1], org_id=f'local-org-{i}', power=10, status='active'))
        peers.append(f"{desc['node_id']}@127.0.0.1:{p2ps[i]}")
    membership = dict(change_delay_blocks=14, change_window_blocks=18, readiness_blocks=30,
                      max_validators=10, floor_reached=True,
                      registry={base64.b64decode(v['pubkey']).hex(): v for v in validators[:initial_count]})
    if args.legacy_binary:
        membership['floor_reached'] = False
    genesis = dict(parameters=dict(chain_id=chain_id, subsidy_cap=0, unbonding_blocks=5,
                                  slash_multiplier=20, dispute_blocks=1, min_node_stake=100,
                                  registration_ttl_blocks=10000),
                   accounts=[dict(id=op, balance=10000, stake=1000) for op in operators],
                   models=[dict(id='local-model', genesis_hash='1' * 64, profile_hash='2' * 64)],
                   membership=membership)
    if args.legacy_binary:
        del genesis['membership']
    if not args.resume:
        app_path = root / 'app-genesis.json'
        app_path.write_text(json.dumps(genesis))
        run('genesis', '--out', root / 'genesis.json', '--chain-id', chain_id,
            '--app-state', app_path, '--validators', ','.join(str(root / f'node-{i}') for i in range(initial_count)))
        # The app-version consensus parameter is explicit on this throwaway chain.
        doc = json.loads((root / 'genesis.json').read_text())
        doc['consensus_params']['version'] = {'app': '4' if args.legacy_binary else '5'}
        (root / 'genesis.json').write_text(json.dumps(doc))
        for i in range(4):
            shutil.copyfile(root / 'genesis.json', root / f'node-{i}/config/genesis.json')
    for sock in sockets:
        sock.close()

    def start(i):
        log = (root / f'node-{i}.log').open('a')
        handles.append(log)
        cmd = [str(start_binary), 'start', '--home', str(root / f'node-{i}'),
               '--p2p', f'tcp://127.0.0.1:{p2ps[i]}', '--rpc', f'tcp://127.0.0.1:{rpcs[i]}',
               '--peers', ','.join(p for j, p in enumerate(peers) if i != j)]
        if upgrade_manifest is not None:
            cmd.extend(['--membership-upgrade', str(upgrade_manifest)])
        procs[i] = subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT)
        record('start-local-validator', index=i, pid=procs[i].pid, command=cmd)

    def stop(i, kill=False):
        proc = procs.pop(i)
        if kill:
            proc.kill()
        else:
            proc.terminate()
        proc.wait(timeout=15)
        record('stop-local-validator', index=i, pid=proc.pid, signal='SIGKILL' if kill else 'SIGTERM')

    def tx(i, action, expect=None):
        s = state()
        result = run('tx', '--rpc', f'http://127.0.0.1:{rpcs[0]}', '--key', root / f'operator-{i}.key',
                     '--chain-id', chain_id, '--nonce', s['accounts'][operators[i]]['nonce'] + 1,
                     '--action', json.dumps(action, separators=(',', ':')), check=expect is None)
        if expect:
            assert result.returncode != 0 and expect in result.stderr, result.stderr
        record('transaction', operator=i, action=action, stdout=result.stdout, stderr=result.stderr,
               expected_rejection=expect)
        return state()

    def snapshot(name):
        s = state()
        (root / f'{name}-state.json').write_text(json.dumps(s, indent=2) + '\n')
        vals = rpc('validators', height=s['height'], per_page=100)
        (root / f'{name}-consensus.json').write_text(json.dumps(vals, indent=2) + '\n')
        record(name, height=s['height'], active=sum(v['status'] in ('active', 'leaving')
                                                  for v in s['membership']['registry'].values()),
               consensus_total=int(vals['total']), history=s['membership'].get('history', []))
        return s

    try:
        for i in range(4):
            start(i)
        wait_height(3)
        if args.legacy_binary:
            before = state()
            assert before['version'] == 4 and 'membership' not in before
            # Stop an initial voter first: at N=2 this freezes the common tip.
            stop(0)
            stopped = json.loads((root / 'node-0/data/ember-state.json').read_text())
            activation = stopped['height'] + 16
            upgrade_manifest = root / 'membership-upgrade.json'
            upgrade_manifest.write_text(json.dumps(dict(chain_id=chain_id, activation_height=activation,
                                                       membership=membership), indent=2))
            record('legacy-upgrade-plan', stopped_height=stopped['height'], activation_height=activation,
                   manifest_sha256=hashlib.sha256(upgrade_manifest.read_bytes()).hexdigest())
            start_binary = binary
            start(0)
            wait_height(stopped['height'] + 3)
            assert state()['version'] == 4 and state(1)['version'] == 4
            record('mixed-binary-progress', before=stopped['height'], after=height(), versions=[state()['version'], state(1)['version']])
            stop(1)
            for i in [2, 3]:
                stop(i)
            # Also replay from an EMPTY observer home using the reviewed manifest.
            observer_home = root / 'node-3'
            historical_home = root / 'node-3-before-upgrade'
            observer_home.rename(historical_home)
            run('init-validator', '--home', observer_home)
            shutil.copyfile(root / 'genesis.json', observer_home / 'config/genesis.json')
            # Preserve the advertised peer identity for peer-list continuity.
            shutil.copyfile(historical_home / 'config/node_key.json', observer_home / 'config/node_key.json')
            for i in [1, 2, 3]:
                start(i)
            wait_height(activation + 2)
            for i in range(4):
                wait_height(activation + 2, i)
                assert state(i)['version'] == 5 and len(state(i)['membership']['registry']) == 2
            record('legacy-activation-applied', height=height(), activation_height=activation,
                   versions=[state(i)['version'] for i in range(4)], empty_observer_replayed=True)
            profile = ember_pb2.ResourceProfile(node_id=operators[2], chain_id='local-model',
                                                manifest_root=b'\x09' * 32, execution_profile_ver='v1')
            ember_core.sign(profile, keys[2])
            tx(2, dict(type='register_node', profile=b64(profile.SerializeToString(deterministic=True)),
                       expires_height=state()['height'] + 9000))
            candidate = dict(validators[2], status='pending')
            s = state()
            at = s['height']
            consensus = json.loads((root / 'node-2/config/priv_validator_key.json').read_text())
            cons_key = Ed25519PrivateKey.from_private_bytes(base64.b64decode(consensus['priv_key']['value'])[:32])
            payload = dict(ChainID=chain_id, Operator=operators[2], PubKey=candidate['pubkey'], Height=at,
                           Hash=s['block_hashes'][str(at)])
            proof = cons_key.sign(b'ember/validator-ready/v1\x00' + json.dumps(payload, separators=(',', ':')).encode())
            tx(2, dict(type='validator_register', validator=candidate, ready=dict(pubkey=candidate['pubkey'], height=at, signature=b64(proof))))
            tx(2, dict(type='validator_ready', ready=dict(pubkey=candidate['pubkey'], height=at, signature=b64(proof))))
            effective = state()['height'] + 17
            tx(0, dict(type='validator_set_change', set_change=dict(add=[candidate], remove=[], effective_height=effective)))
            snapshot('legacy-first-join-pending')
            wait_height(effective + 2)
            s = snapshot('legacy-first-join-applied')
            assert s['membership']['floor_reached']
            assert sum(v['status'] == 'active' for v in s['membership']['registry'].values()) == 3
            record('PASS', chain_id=chain_id, final_height=height(), scope='local version-4 upgrade and first 2->3 join')
            return
        joining = dict(validators[3], status='pending')
        if not args.resume:
            # N=3 boundary: with one killed, committed height stops after in-flight work.
            stop(2, kill=True)
            time.sleep(4)
            halted = height()
            time.sleep(8)
            assert height() == halted, (halted, height())
            record('n3-single-stop-boundary', before=halted, after=height(), observation_seconds=8,
                   interpretation='strict >2/3 requires 3/3; expected halt')
            start(2)
            wait_height(halted + 3)
            # Existing signed profile registration and staking admission.
            profile = ember_pb2.ResourceProfile(node_id=operators[3], chain_id='local-model',
                                                manifest_root=b'\x09' * 32, execution_profile_ver='v1')
            ember_core.sign(profile, keys[3])
            tx(3, dict(type='register_node', profile=b64(profile.SerializeToString(deterministic=True)),
                       expires_height=state()['height'] + 9000))
            joining = dict(validators[3], status='pending')
            s = state()
            at = s['height']
            consensus = json.loads((root / 'node-3/config/priv_validator_key.json').read_text())
            cons_key = Ed25519PrivateKey.from_private_bytes(base64.b64decode(consensus['priv_key']['value'])[:32])
            payload = dict(ChainID=chain_id, Operator=operators[3], PubKey=joining['pubkey'], Height=at,
                           Hash=s['block_hashes'][str(at)])
            proof = cons_key.sign(b'ember/validator-ready/v1\x00' + json.dumps(payload, separators=(',', ':')).encode())
            tx(3, dict(type='validator_register', validator=joining, ready=dict(pubkey=joining['pubkey'], height=at, signature=b64(proof))))
            tx(3, dict(type='validator_ready', ready=dict(pubkey=joining['pubkey'], height=at, signature=b64(proof))))
            effective = state()['height'] + 17
            tx(0, dict(type='validator_set_change', set_change=dict(add=[joining], remove=[], effective_height=effective)))
            snapshot('join-pending')
            # Stop a current validator while the 3->4 change is pending. Consensus
            # cannot advance at N=3; restart within the announcement window.
            stop(1, kill=True)
            time.sleep(4)
            paused = height()
            time.sleep(7)
            assert height() == paused and paused < effective - 2
            record('kill-inside-pending-window', height=paused, effective_height=effective,
                   pending=state()['membership']['pending'], expected='halt at N=3; safe recovery')
            start(1)
            wait_height(effective + 2)
            s = snapshot('join-applied')
            assert len([v for v in s['membership']['registry'].values() if v['status'] == 'active']) == 4
            # Smallest equal-power set tolerating one absence is four.
            stop(2, kill=True)
            before = height()
            after = wait_height(before + 4)
            record('n4-single-stop-progress', before=before, after=after, stopped=2)
            snapshot('n4-one-down')
            start(2)
            wait_height(after + 3)
        # Wait for recovered consensus participation, not only RPC height.
        until = time.monotonic() + 120
        while len(state()['membership'].get('reachable', {})) < 4:
            if time.monotonic() > until:
                raise RuntimeError('four validators did not resume commit participation')
            time.sleep(0.5)
        record('all-four-reachable-before-exit', height=height(), reachable=state()['membership']['reachable'])
        # Leave via the candidate's own operator; old/new quorum checked again.
        s = state()
        wait_height(max(s['height'], s['membership']['last_proposal_height'] + 18))
        effective = state()['height'] + 17
        leaving_id = base64.b64decode(joining['pubkey']).hex()
        tx(3, dict(type='validator_set_change', set_change=dict(add=[], remove=[leaving_id], effective_height=effective)))
        snapshot('exit-pending')
        wait_height(effective + 2)
        s = snapshot('exit-applied')
        assert s['membership']['registry'][leaving_id]['status'] == 'exited'
        tx(3, dict(type='unbond', amount=100))
        release = state()['accounts'][operators[3]]['unbondings'][-1]['release_height']
        wait_height(release)
        tx(3, dict(type='claim_unbonded'))
        s = state()
        wait_height(max(s['height'], s['membership']['last_proposal_height'] + 18))
        effective = state()['height'] + 17
        tx(0, dict(type='validator_set_change', set_change=dict(add=[], remove=[base64.b64decode(validators[0]['pubkey']).hex()],
                                                             effective_height=effective)), expect='floor/cap')
        snapshot('below-floor-rejected')
        record('PASS', chain_id=chain_id, final_height=height(), scope='local throwaway only')
    finally:
        for i in list(procs):
            stop(i)
        for handle in handles:
            handle.close()


if __name__ == '__main__':
    main()
