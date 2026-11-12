import importlib.util
import tempfile
import unittest
from pathlib import Path
import sys
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'core-py'))
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'proto'))
from ember_core import verify
from ember_pb2 import ChainGenesis, ModelManifest

spec = importlib.util.spec_from_file_location('build', Path(__file__).with_name('build.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class GenesisTests(unittest.TestCase):
    def test_second_model_identity_and_tokenizer_artifacts(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp)
            (path / 'config.json').write_text('{"model_type": "llama", "num_hidden_layers": 32}')
            (path / 'tokenizer.json').write_text('{}')
            (path / 'special_tokens_map.json').write_text('{"eos_token": "<|im_end|>"}')
            (path / 'model.safetensors').write_bytes(b'synthetic llama artifact')
            arguments = (path, path / 'output', 'ember-smollm2-360m-origin', 'transformers-smollm2-v1')
            module.build(*arguments)
            manifest = ModelManifest.FromString((path / 'output/manifest.pb').read_bytes())
            genesis = ChainGenesis.FromString((path / 'output/genesis.pb').read_bytes())
            self.assertEqual(manifest.chain_id, arguments[2])
            self.assertEqual(genesis.execution_profile_ver, arguments[3])
            self.assertIn('special_tokens_map.json', {artifact.name for artifact in manifest.artifacts})
            with self.assertRaisesRegex(ValueError, 'identity mismatch'):
                module.build(path, path / 'output', arguments[2], 'another-profile')

    def test_refuses_incomplete_and_missing(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp)
            with self.assertRaisesRegex(ValueError, 'config.json'):
                module.build(path, path / 'output')
            (path / 'config.json').write_text('{"num_hidden_layers": 28}')
            (path / 'model.safetensors.incomplete').write_bytes(b'partial')
            with self.assertRaisesRegex(ValueError, 'incomplete'):
                module.build(path, path / 'output')
            (path / 'model.safetensors.incomplete').unlink()
            with self.assertRaisesRegex(ValueError, 'safetensors'):
                module.build(path, path / 'output')

    def test_signed_manifest_hashes_every_artifact(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp)
            (path / 'config.json').write_text('{"num_hidden_layers": 28}')
            (path / 'tokenizer.json').write_text('{}')
            (path / 'model.safetensors').write_bytes(b'synthetic test artifact')
            module.build(path, path / 'output')
            manifest = ModelManifest.FromString((path / 'output' / 'manifest.pb').read_bytes())
            genesis = ChainGenesis.FromString((path / 'output' / 'genesis.pb').read_bytes())
            verify(manifest)
            verify(genesis)
            self.assertEqual(genesis.model_manifest_root, module.digest(manifest))
            self.assertEqual(len(manifest.artifacts), 3)
            self.assertEqual({a.name for a in manifest.artifacts},
                             {'model.safetensors', 'config.json', 'tokenizer.json'})
            before = (path / 'output' / 'genesis.pb').read_bytes()
            module.build(path, path / 'output')
            self.assertEqual((path / 'output' / 'genesis.pb').read_bytes(), before)
            (path / 'model.safetensors').write_bytes(b'different test artifact')
            with self.assertRaisesRegex(ValueError, 'immutable'):
                module.build(path, path / 'output')


if __name__ == '__main__':
    unittest.main()
