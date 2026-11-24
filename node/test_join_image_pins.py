"""Signed image pin matching must preserve all config bindings."""
import json
from pathlib import Path
from types import SimpleNamespace
import pytest
from ember_pb2 import ModelManifest
from agent.join import _validate_image_pins

PINS = json.loads((Path(__file__).parent / 'engine/assets/sd15-fp32-v1.json').read_text())['files']


def manifest():
    m = ModelManifest(config_hash=bytes.fromhex(PINS['unet/config.json']))
    for name, value in PINS.items():
        m.artifacts.add(name='config.json' if name == 'unet/config.json' else name,
                        sha256=bytes.fromhex(value))
    for name in ('arch_manifest.json', 'scheduler_cfg.json'):
        m.artifacts.add(name=name, sha256=b'x'*32)
    return m


def test_v2_twelve_assets():
    m = manifest()
    assert len(m.artifacts) == 12
    _validate_image_pins(m, PINS)


def test_v1_missing_configs_fails():
    m = manifest()
    kept = [a for a in m.artifacts if a.name not in ('vae/config.json', 'text_encoder/config.json')]
    del m.artifacts[:]
    m.artifacts.extend(kept)
    assert len(m.artifacts) == 10
    with pytest.raises(ValueError, match='signed image manifest differs'):
        _validate_image_pins(m, PINS)


@pytest.mark.parametrize('name', ['config.json', 'vae/config.json', 'text_encoder/config.json'])
def test_config_tampering_fails(name):
    m = manifest()
    next(a for a in m.artifacts if a.name == name).sha256 = b'x'*32
    with pytest.raises(ValueError):
        _validate_image_pins(m, PINS)


@pytest.mark.parametrize('name', ['config.json', 'tokenizer/vocab.json'])
def test_duplicate_names_fail(name):
    m = manifest()
    m.artifacts.add().CopyFrom(next(a for a in m.artifacts if a.name == name))
    with pytest.raises(ValueError):
        _validate_image_pins(m, PINS)


def test_config_hash_binding_fails():
    m = manifest()
    m.config_hash = b'x'*32
    with pytest.raises(ValueError):
        _validate_image_pins(m, PINS)


def test_missing_config_hash_client_uses_unique_hash_match():
    m = SimpleNamespace(artifacts=manifest().artifacts)
    _validate_image_pins(m, PINS)


def test_same_name_mismatch_cannot_fall_back():
    m = manifest()
    m.artifacts.add(name='unet/config.json', sha256=b'x'*32)
    with pytest.raises(ValueError):
        _validate_image_pins(m, PINS)


def test_non_config_missing_fails():
    m = manifest()
    with pytest.raises(ValueError):
        _validate_image_pins(m, dict(PINS, **{'other/model.bin': PINS['unet/config.json']}))
