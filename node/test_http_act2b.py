"""ACT-2b HTTP port: local tokenizer identity, priced counting and replay text."""
import tempfile
from pathlib import Path
from unittest.mock import patch

import pytest
from tokenizers import Tokenizer, models
from transformers import PreTrainedTokenizerFast

from engine import VLLMEngine, UnsupportedCapability
from tokenization import artifact_version, count, TokenizerUnavailable
from reasoning import prepare, Budget
from auditor.worker import feed_openings_budget
from test_http_streaming import fixture_server, frame
from evidence import encode_step, canonical, chunks
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey


@pytest.fixture
def local_model():
    with tempfile.TemporaryDirectory() as directory:
        vocab = {'[UNK]': 0, 'hello': 1, 'H': 100, '</think>': 101, '!': 102}
        # A real AutoTokenizer-compatible local fixture, never a Hub download.
        vocab.update({f'unused{i}': i for i in range(2, 100)})
        tokenizer = PreTrainedTokenizerFast(
            tokenizer_object=Tokenizer(models.WordLevel(vocab, unk_token='[UNK]')),
            unk_token='[UNK]')
        tokenizer.save_pretrained(directory)
        yield directory


def test_identity_pinned_and_fail_closed(local_model):
    engine = VLLMEngine('http://localhost', local_model)
    assert engine._tokenizer is None
    assert engine.tokenizer_version == artifact_version(local_model)
    assert engine.tokenizer.encode('hello') == [1]
    config = Path(local_model) / 'tokenizer_config.json'
    config.write_text(config.read_text() + '\n')
    with pytest.raises(TokenizerUnavailable):
        _ = engine.tokenizer_version
    with pytest.raises(TokenizerUnavailable):
        _ = VLLMEngine('http://localhost', local_model + '/missing').tokenizer_version
    Path(local_model, 'tokenizer.json').unlink()
    with pytest.raises(TokenizerUnavailable):
        _ = engine.tokenizer_version


def test_priced_count_endpoint(local_model):
    with fixture_server() as (url, seen, _):
        engine = VLLMEngine(url, local_model)
        version = artifact_version(local_model)
        assert count(engine, 'hello', expected=version) == (1, version)
        assert seen == [{'model': local_model, 'prompt': 'hello'}]
        with pytest.raises(TokenizerUnavailable):
            count(engine, 'hello', expected='wrong')
        assert len(seen) == 1


@pytest.mark.parametrize('enabled', [True, False])
def test_thinking_prompt_and_call_chain(local_model, enabled):
    with fixture_server(interval=.001) as (url, seen, _):
        engine = VLLMEngine(url, local_model)
        job = dict(thinking_enabled=enabled, thinking_budget=2 if enabled else 0,
                   answer_budget=3, max_tokens=5)
        expected = 'hello' if enabled else 'hello<think>\n\n</think>\n\n'
        assert engine.thinking_prompt('hello', enabled) == expected
        prompt = prepare(engine, 'hello', job)
        assert prompt == expected
        list(engine.generate(prompt, 0, 3, enable_thinking=enabled))
        assert seen[0]['prompt'] == expected
        assert seen[0]['chat_template_kwargs'] == {'enable_thinking': enabled}


def test_per_token_budget_and_evidence_all_transports(local_model):
    key = Ed25519PrivateKey.generate()
    reference = None
    for mode, streaming in [('ok', False), ('ok', True), ('multi', True),
                            ('json', True), ('rejected', True), ('other', True)]:
        with fixture_server(mode, .001) as (url, _, _):
            engine = VLLMEngine(url, local_model, streaming=streaming)
            steps = list(engine.generate('hello', 0, 3))
            assert [step.text for step in steps] == ['H', '</think>', '!']
            job = dict(thinking_enabled=True, thinking_budget=2, answer_budget=1)
            executor, auditor = Budget(job), Budget(job)
            for step in steps:
                executor.add(step.text)
            feed_openings_budget(engine.tokenizer, [{'token': step.token} for step in steps], auditor)
            assert (executor.reasoning, executor.content) == (2, 1)
            assert (executor.reasoning, executor.content) == (auditor.reasoning, auditor.content)
            result = (canonical([encode_step(step) for step in steps]), chunks('test', steps, key))
            if reference is None:
                reference = result
            assert result == reference
    engine = VLLMEngine('http://localhost', local_model, eos_token_ids=(102,))
    assert engine.parse_stream_chunk(frame(), 16)[0][-1].text == ''


def test_scoring_port(local_model):
    engine = VLLMEngine('http://localhost', local_model, eos_token_ids=(102,))
    scored = [None, {'100': {'logprob': -.1}, '101': {'logprob': -2}},
              {'102': {'logprob': -.1}}]
    with patch.object(engine, '_post', return_value={'tokens': [1]}) as tokenize, \
         patch.object(engine, '_post_scoring', return_value={
             'choices': [{'prompt_logprobs': scored}], 'usage': {'prompt_tokens': 3}}) as score:
        steps = engine.score_forced('hello', 0, [100, 102])
        assert [step.token for step in steps] == [100, 102]
        assert tokenize.call_args.args[1]['prompt'] == 'hello'
        assert score.call_args.args[1]['prompt'] == [1, 100, 102]
    with patch.object(engine, '_post', return_value={'tokens': [1]}), \
         patch.object(engine, '_post_scoring', return_value={'choices': [{}]}):
        with pytest.raises(UnsupportedCapability):
            engine.score_forced('hello', 0, [100])
