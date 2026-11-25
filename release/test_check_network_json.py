"""Release gate: bundled profile artifacts and publisher pin in network.json."""
import hashlib
import json
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
from check_network_json import main

ROOT = Path(__file__).resolve().parents[1]
PROFILE_ID = 'ember-qwen2.5-1.5b-unified-v1'
GENESIS_SHA = '00973f5dc58bcbe2bad729d164f8f6171f20ad68c4535930799aa70f7495b668'
MANIFEST_SHA = '5b065539711780be0133faa3f9e4e37a177c4f2b400e7ad5cfa4d5702407da75'
PUBLISHER = 'c0058cba3b9014581b4f88679d24f3fc1c524e33668c4974fadfebf3bb29307d'


def _setup(tmp_path, profile_mutation=None, artifact_mode='real'):
    document = json.loads((ROOT / 'site/network.json').read_text())
    if profile_mutation:
        profile_mutation(document['current_profile'])
    network = tmp_path / 'network.json'
    network.write_text(json.dumps(document))
    profiles = tmp_path / 'profiles'
    if artifact_mode != 'absent':
        target = profiles / PROFILE_ID
        target.mkdir(parents=True)
        source = ROOT / 'chain-genesis' / PROFILE_ID
        for name in ('genesis.pb', 'manifest.pb'):
            data = (source / name).read_bytes()
            if artifact_mode == 'corrupt' and name == 'manifest.pb':
                data += b'x'
            (target / name).write_bytes(data)
    return ['--network-json', str(network), '--version-file', str(ROOT / 'VERSION'),
            '--profiles-root', str(profiles)]


def test_current_network_json_passes(tmp_path):
    assert main(_setup(tmp_path)) == 0


def test_publisher_field_is_the_live_publisher():
    document = json.loads((ROOT / 'site/network.json').read_text())
    assert document['current_profile']['publisher'] == PUBLISHER


def test_missing_publisher_fails(tmp_path):
    def drop(profile):
        profile.pop('publisher', None)
    assert main(_setup(tmp_path, drop)) == 1


def test_non_hex_publisher_fails(tmp_path):
    def corrupt(profile):
        profile['publisher'] = 'xyz'
    assert main(_setup(tmp_path, corrupt)) == 1


def test_missing_artifacts_fail(tmp_path):
    assert main(_setup(tmp_path, artifact_mode='absent')) == 1


def test_corrupt_artifact_fails(tmp_path):
    assert main(_setup(tmp_path, artifact_mode='corrupt')) == 1


def test_repo_artifacts_match_published_anchors():
    profile_dir = ROOT / 'chain-genesis' / PROFILE_ID
    assert hashlib.sha256((profile_dir / 'genesis.pb').read_bytes()).hexdigest() == GENESIS_SHA
    assert hashlib.sha256((profile_dir / 'manifest.pb').read_bytes()).hexdigest() == MANIFEST_SHA
