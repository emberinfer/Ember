"""Thinking-switch carriage through the engine SPI and the vLLM adapter.

Covers the node side of the reasoning fix (2026-11-15): SamplingParams
carries enable_thinking + budgets from the execution request, the vLLM
payload forwards the switch as chat_template_kwargs (the only
engine-side form measured to work), a length-truncated empty answer is
marked on the adapter for the receipt, and reasoning.prepare leaves
the committed prompt untouched for payload-carrying engines.
"""
import unittest

from engine.spi import SamplingParams, EngineCapabilities
from engine.adapters.http import VLLMEngine
from reasoning import prepare


def caps():
    return EngineCapabilities('vllm', 'test', 'remote', 'unknown',
                              ('topk-logprobs',), 'logprobs', 20, False,
                              native_incremental=False)


def completion(text, finish_reason, tokens=1):
    return {'choices': [{
        'text': text,
        'finish_reason': finish_reason,
        'logprobs': {
            'tokens': ['token_id:5'] * tokens,
            'top_logprobs': [{'token_id:5': -0.1}] * tokens,
        },
    }]}


class SamplingParamsThinkingTests(unittest.TestCase):
    def test_defaults_and_validation(self):
        p = SamplingParams()
        self.assertIs(p.enable_thinking, True)
        self.assertEqual((p.thinking_budget, p.answer_budget), (0, 0))
        p.validate(caps())
        SamplingParams(max_tokens=64, enable_thinking=False, answer_budget=64).validate(caps())
        SamplingParams(max_tokens=64, enable_thinking=True, thinking_budget=48, answer_budget=16).validate(caps())
        with self.assertRaises(ValueError):
            SamplingParams(enable_thinking='no').validate(caps())
        with self.assertRaises(ValueError):
            SamplingParams(enable_thinking=False, thinking_budget=1).validate(caps())
        with self.assertRaises(ValueError):
            SamplingParams(max_tokens=8, thinking_budget=6, answer_budget=6).validate(caps())
        with self.assertRaises(ValueError):
            SamplingParams(answer_budget=-1).validate(caps())


class VLLMThinkingPayloadTests(unittest.TestCase):
    def setUp(self):
        self.engine = VLLMEngine('http://127.0.0.1:8000', 'qwen-test')

    def test_payload_carries_enable_thinking_false(self):
        payload = self.engine.payload('prompt', SamplingParams(max_tokens=64, enable_thinking=False, answer_budget=64))
        self.assertEqual(payload['chat_template_kwargs'], {'enable_thinking': False})

    def test_payload_carries_enable_thinking_true(self):
        payload = self.engine.payload('prompt', SamplingParams(max_tokens=64, enable_thinking=True, thinking_budget=48, answer_budget=16))
        self.assertEqual(payload['chat_template_kwargs'], {'enable_thinking': True})

    def test_parse_marks_thinking_truncation(self):
        self.engine.thinking_truncated = False
        self.engine.parse(completion('', 'length', tokens=4), 16)
        self.assertTrue(self.engine.thinking_truncated)

    def test_parse_no_mark_for_content_or_stop(self):
        self.engine.thinking_truncated = False
        self.engine.parse(completion('answer', 'length', tokens=2), 16)
        self.assertFalse(self.engine.thinking_truncated)
        self.engine.parse(completion('', 'stop', tokens=1), 16)
        self.assertFalse(self.engine.thinking_truncated)


class PreparePayloadEngineTests(unittest.TestCase):
    def test_payload_engine_keeps_committed_prompt(self):
        engine = VLLMEngine('http://127.0.0.1:8000', 'qwen-test')
        job = dict(max_tokens=64, thinking_enabled=False, answer_budget=64)
        self.assertEqual(prepare(engine, 'committed prompt', job), 'committed prompt')
        job = dict(max_tokens=64, thinking_enabled=True, thinking_budget=48, answer_budget=16)
        self.assertEqual(prepare(engine, 'committed prompt', job), 'committed prompt')

    def test_engine_without_any_enforcement_still_fails_closed(self):
        class Bare:
            pass
        with self.assertRaisesRegex(ValueError, 'cannot enforce'):
            prepare(Bare(), 'hi', dict(max_tokens=8, thinking_enabled=False, answer_budget=8))


if __name__ == '__main__':
    unittest.main()
