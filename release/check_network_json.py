#!/usr/bin/env python3
"""Fail the release build if the bundled network.json is stale.

The release bundle ships site/network.json at its root, and the node
configure helper (node/agent/network.py) consumes it at runtime:
current_profile.id / genesis_sha256 / manifest_sha256 become the
node's chain anchors, access.seed becomes its coordinator, and the
gateway directory decides which node tunnels a profile may use.
A stale copy therefore misleads running nodes, not just readers --
in v0.9.1 the shipped file still named the retired gpu-v1 profile
with its old genesis/manifest pair and release version 0.7.0.

Checks (all hard failures):
  * release.version equals the repository VERSION file;
  * release.base_url is the download URL of that same version;
  * current_profile.id is not a retired profile;
  * current_profile genesis/manifest hashes are 64-hex;
  * every gateway whose node tunnel endpoint is the public seed
    (access.seed) advertises the current profile -- the seed tunnel
    serves the current profile, so listing any other profile for it
    contradicts the live network.
"""
import argparse
import json
import re
import sys
from pathlib import Path

# Profiles that must never again be the current profile.  They may
# still appear in the gateway directory or in chain-genesis history
# where they describe real past serving; this list only gates the
# current_profile slot.
RETIRED_CURRENT_PROFILE_IDS = frozenset({
    'ember-qwen2.5-1.5b-gpu-v1',
    'ember-qwen2.5-1.5b-s22-cpu-v1',
})

HEX64 = re.compile(r'[0-9a-fA-F]{64}')


def main(argv=None):
    parser = argparse.ArgumentParser(prog='check_network_json')
    parser.add_argument('--network-json', default='site/network.json')
    parser.add_argument('--version-file', default='VERSION')
    args = parser.parse_args(argv)

    version = Path(args.version_file).read_text().strip()
    try:
        document = json.loads(Path(args.network_json).read_text())
    except (OSError, ValueError) as exc:
        print(f'network.json check FAILED: cannot parse: {exc}',
              file=sys.stderr)
        return 1

    problems = []
    release = document.get('release') or {}
    if release.get('version') != version:
        problems.append(
            f"release.version={release.get('version')!r}; "
            f'expected VERSION {version!r}')
    expected_base = ('https://github.com/emberinfer/Ember/releases/'
                     f'download/v{version}')
    if release.get('base_url') != expected_base:
        problems.append(
            f"release.base_url={release.get('base_url')!r}; "
            f'expected {expected_base!r}')

    profile = document.get('current_profile') or {}
    profile_id = profile.get('id')
    if profile_id in RETIRED_CURRENT_PROFILE_IDS:
        problems.append(
            f'current_profile.id={profile_id!r} is a retired profile')
    if not profile_id:
        problems.append('current_profile.id is missing')
    for key in ('genesis_sha256', 'manifest_sha256'):
        value = profile.get(key)
        if not (isinstance(value, str) and HEX64.fullmatch(value)):
            problems.append(f'current_profile.{key} is not a 64-hex hash')

    access = document.get('access') or {}
    seed = access.get('seed')
    if seed and profile_id:
        for gateway in access.get('gateways') or []:
            tunnel = gateway.get('node_tunnel') or {}
            if tunnel.get('endpoint') == seed:
                listed = gateway.get('profiles') or []
                if profile_id not in listed:
                    problems.append(
                        f'gateway {gateway.get("url")!r} serves the seed '
                        f'tunnel {seed} but lists profiles {listed!r}, '
                        f'not the current profile {profile_id!r}')

    if problems:
        print('network.json check FAILED:', file=sys.stderr)
        for problem in problems:
            print(f'  - {problem}', file=sys.stderr)
        return 1
    print(f'network.json OK: release {version}, '
          f'current profile {profile_id}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
