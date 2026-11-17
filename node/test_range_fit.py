"""Range admission against real local Qwen2.5-1.5B tensor headers."""
import json
import os
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest
from safetensors import safe_open

from agent.range_fit import check_range_fit, RangeFitError, tensor_sizes


@pytest.fixture
def weights():
    root = Path(os.environ.get('EMBER_TEST_1_5B_PATH', '/root/models/Qwen2.5-1.5B-Instruct'))
    if not (root / 'model.safetensors').exists():
        pytest.skip('local Qwen2.5-1.5B weights unavailable')
    config = json.loads((root / 'config.json').read_text())
    artifacts = [SimpleNamespace(name=p.name) for p in root.iterdir() if p.is_file()]
    return root, config, artifacts


def test_real_range_fits_and_full_range_rejects(weights):
    root, config, artifacts = weights
    # Independently sum actual tensor shapes/dtypes via safetensors, rather
    # than reusing the estimator's data_offsets accounting.
    sizes = {}
    import torch
    with safe_open(str(root / 'model.safetensors'), framework='pt') as reader:
        for key in reader.keys():
            tensor = reader.get_slice(key)
            count = 1
            for n in tensor.get_shape():
                count *= n
            assert tensor.get_dtype() == 'BF16'
            sizes[key] = count * torch.tensor([], dtype=torch.bfloat16).element_size()
    layers = tuple(sum(v for k, v in sizes.items() if k.startswith(f'model.layers.{i}.'))
                   for i in range(28))
    result = check_range_fit(artifacts, config, root, 0, 14, 64 * 1024**3)
    assert result.layer_bytes == layers
    assert result.weight_bytes == sum(layers[:14]) + sizes['model.embed_tokens.weight']
    assert result.head_bytes == 0
    with pytest.raises(RangeFitError) as exc:
        check_range_fit(artifacts, config, root, 0, 28, 1024**3)
    result = exc.value.estimate
    expected = sum(layers) + 2 * sizes['model.embed_tokens.weight'] + sizes['model.norm.weight']
    assert result.weight_bytes == expected
    assert result.required_bytes == 2 * expected + result.activation_bytes
    assert result.available_bytes == 1024**3
    assert str(result.required_bytes) in str(exc.value)
    assert str(1024**3) in str(exc.value)
    tail = check_range_fit(artifacts, config, root, 14, 28, 64 * 1024**3)
    assert tail.embedding_bytes == 0
    assert tail.head_bytes == sizes['model.embed_tokens.weight'] + sizes['model.norm.weight']


def test_index_accounting_and_missing_artifact(weights, tmp_path):
    root, _, artifacts = weights
    original = tensor_sizes(root, artifacts)
    (tmp_path / 'model.safetensors').symlink_to(root / 'model.safetensors')
    (tmp_path / 'config.json').symlink_to(root / 'config.json')
    (tmp_path / 'model.safetensors.index.json').write_text(json.dumps(
        {'weight_map': {k: 'model.safetensors' for k in original}}))
    declared = [SimpleNamespace(name=p.name) for p in tmp_path.iterdir()]
    assert tensor_sizes(tmp_path, declared) == original
    with pytest.raises(ValueError, match='undeclared'):
        tensor_sizes(tmp_path, [a for a in declared if a.name != 'model.safetensors'])


def test_dynamic_admission_before_engine_or_registration(weights):
    from shard_rpc import serve, register_stage
    from ember_pb2 import ModelManifest, ShardSpec
    root, _, artifacts = weights
    manifest = ModelManifest(dynamic_ranges=True, total_layers=28)
    for a in artifacts:
        manifest.artifacts.add(name=a.name)
    spec = ShardSpec(start_layer=0, end_layer=28)
    args = SimpleNamespace(model_path=str(root), genesis='unused', manifest='unused',
                           shard_index=0, range_start=0, range_end=28,
                           fit_memory_bytes=1024**3)
    with patch('shard_rpc.load_shard_anchor', return_value=(None, manifest, spec)), \
            patch('shard_rpc.Qwen2ShardEngine') as engine:
        with pytest.raises(RangeFitError):
            serve(args)
        engine.assert_not_called()
    with patch('agent.join.register_on_chain') as register:
        with pytest.raises(RangeFitError):
            register_stage(args, None, None, None, manifest, spec, None, None)
        register.assert_not_called()
    args.fit_memory_bytes = None
    with patch.dict(os.environ, {}, clear=True):
        with pytest.raises(ValueError, match='positive memory budget'):
            from shard_rpc import check_dynamic_range_fit
            check_dynamic_range_fit(args, manifest, spec)
    with patch.dict(os.environ, {'EMBER_RANGE_MEMORY_BYTES': str(64 * 1024**3)}):
        assert check_dynamic_range_fit(args, manifest, spec).available_bytes == 64 * 1024**3
    manifest.dynamic_ranges = False
    assert check_dynamic_range_fit(args, manifest, spec) is None
