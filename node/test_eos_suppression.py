"""EOS derivation and the deployed per-token text suppression regression."""
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from agent.join import _model_eos_token_ids
from engine import load_engine
from engine.adapters.http import VLLMEngine
from engine.adapters.transformers import model_eos_token_ids
from engine.spi import Step


class EOSSuppressionTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.auto = Mock()
        self.auto.from_pretrained.side_effect = RuntimeError('no local tokenizer')
        stub = SimpleNamespace(AutoTokenizer=self.auto)
        patcher = patch.dict('sys.modules', {'transformers': stub})
        patcher.start()
        self.addCleanup(patcher.stop)

    def write(self, name, value):
        (self.root / name).write_text(json.dumps(value))

    def test_generation_config_sorted_unique(self):
        self.write('generation_config.json', {'eos_token_id': [248046, 248044, 248046]})
        self.assertEqual(_model_eos_token_ids(self.root), [248044, 248046])

    def test_config_fallback_int_and_union(self):
        self.write('config.json', {'eos_token_id': 248044})
        self.assertEqual(_model_eos_token_ids(self.root), [248044])
        self.write('generation_config.json', {'eos_token_id': []})
        self.assertEqual(_model_eos_token_ids(self.root), [248044])
        self.write('generation_config.json', {'eos_token_id': 248046})
        self.assertEqual(_model_eos_token_ids(self.root), [248044, 248046])

    def test_tokenizer_union(self):
        self.write('generation_config.json', {'eos_token_id': 248046})
        self.auto.from_pretrained.side_effect = None
        self.auto.from_pretrained.return_value.eos_token_id = 248044
        self.assertEqual(_model_eos_token_ids(self.root), [248044, 248046])
        self.auto.from_pretrained.assert_called_once_with(
            self.root, local_files_only=True, trust_remote_code=False)

    def test_none_empty_and_invalid(self):
        self.assertIsNone(_model_eos_token_ids(None))
        self.assertIsNone(_model_eos_token_ids(''))
        self.assertIsNone(_model_eos_token_ids(self.root))
        self.write('generation_config.json', {'eos_token_id': [None, True, -1, '2', 2.5]})
        self.write('config.json', {})
        self.assertIsNone(_model_eos_token_ids(self.root))
        (self.root / 'generation_config.json').write_text('{bad json')
        self.assertIsNone(_model_eos_token_ids(self.root))

    def test_transformers_helper_union(self):
        self.write('generation_config.json', {'eos_token_id': [248046, True, -1, None]})
        self.write('config.json', {'eos_token_id': 248044})
        self.assertEqual(model_eos_token_ids(self.root, 7), frozenset({7, 248044, 248046}))
        self.assertEqual(model_eos_token_ids(None, None), frozenset())
        self.assertEqual(model_eos_token_ids(self.root, False), frozenset({248044, 248046}))

    def test_deployed_rule_preserves_steps_and_evidence(self):
        self.write('generation_config.json', {'eos_token_id': [248046, 248044]})
        eos_set = _model_eos_token_ids(self.root)
        tokenizer = SimpleNamespace(decode=lambda ids: {
            248046: '<|im_end|>', 248044: '<|endoftext|>'}.get(ids[0], 'x'))
        tokens = list(range(9)) + [248046]

        def simulate(eos):
            return [Step(ident, '' if ident in eos else tokenizer.decode([ident]),
                         [(ident, 0.0)], {'0': b'unchanged'}) for ident in tokens]

        before, after = simulate(set()), simulate(eos_set)
        self.assertIn('<|im_end|>', ''.join(step.text for step in before))
        text = ''.join(step.text for step in after)
        self.assertNotIn('<|im_end|>', text)
        self.assertNotIn('<|endoftext|>', text)
        self.assertEqual(len(after), 10)
        self.assertEqual(len(before), len(after))
        for old, new in zip(before, after):
            self.assertEqual({k: v for k, v in vars(old).items() if k != 'text'},
                             {k: v for k, v in vars(new).items() if k != 'text'})

    def test_load_engine_forwarding_and_validation(self):
        engine = load_engine('vllm', endpoint='http://127.0.0.1:9', model='m',
                             eos_token_ids=[248046, 248044])
        self.assertIsInstance(engine, VLLMEngine)
        self.assertEqual(set(engine.eos_token_ids), {248046, 248044})
        for invalid in ([True], [-1], ['248046']):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                load_engine('vllm', endpoint='http://127.0.0.1:9', model='m',
                            eos_token_ids=invalid)
