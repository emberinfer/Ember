"""Byte-split UTF-8 regressions using a self-contained byte BPE tokenizer."""
from unittest.mock import patch

import pytest
from tokenizers import Tokenizer, models, pre_tokenizers, decoders
from transformers import PreTrainedTokenizerFast

from tokenization import TokenTextDecoder, decode_token_texts
from engine import VLLMEngine
from shard_rpc import PipelineEngine
from auditor.worker import feed_openings_budget
from reasoning import Budget

CASES = [
    '随时告诉我～😄', '随时告诉我～😊', '🌞🎉👍👌', '🇨🇳', '🏃‍♂️',
    '我帮你查查具体的天气情况哦！☀️🌧️', '👨‍👩‍👧‍👦', '👍🏽', '❤️', '✨🚀',
    '𝄞', '纯中文对照组', 'plain english control', 'a😄b mixed', '🇨🇳🇺🇸 flags',
    '1️⃣ keycap', 'café naïve résumé', '日本語テスト', '😄😊🌞🎉 consecutive',
]


@pytest.fixture
def tok():
    vocab = {char: index for index, char in enumerate(sorted(pre_tokenizers.ByteLevel.alphabet()))}
    backend = Tokenizer(models.BPE(vocab=vocab, merges=[]))
    backend.pre_tokenizer = pre_tokenizers.ByteLevel(add_prefix_space=False)
    backend.decoder = decoders.ByteLevel()
    tokenizer = PreTrainedTokenizerFast(tokenizer_object=backend, clean_up_tokenization_spaces=False)
    tokenizer.add_special_tokens({'eos_token': '<eos>'})
    return tokenizer


def decoded(tok, ids):
    decoder = TokenTextDecoder(tok)
    return ''.join(decoder.feed(token) for token in ids) + decoder.flush()


@pytest.mark.parametrize('text', CASES)
def test_byte_split_roundtrip(tok, text):
    ids = tok.encode(text)
    assert decoded(tok, ids) == text == tok.decode(ids)
    assert ''.join(decode_token_texts(tok, ids)) == text


def test_old_corruption_evidence(tok):
    old = [''.join(tok.decode([token]) for token in tok.encode(text)) for text in CASES]
    assert sum(before != text for before, text in zip(old, CASES)) >= 3
    for index in (0, 3, 4):
        assert old[index] != CASES[index] and '\ufffd' in old[index]


def test_truncated_flush(tok):
    ids = tok.encode(CASES[0])[:-1]
    result = decoded(tok, ids)
    assert result == tok.decode(ids)
    assert result.endswith('\ufffd') and not result.endswith('\ufffd\ufffd')
    assert decode_token_texts(tok, []) == []


def test_cross_frames(tok):
    ids = tok.encode(CASES[5])
    half = len(ids) // 2
    decoder = TokenTextDecoder(tok)
    first = ''.join(decoder.feed(token) for token in ids[:half])
    second = ''.join(decoder.feed(token) for token in ids[half:])
    assert first + second + decoder.flush() == CASES[5]


def frame(ids, finish=None):
    return {'choices': [{'text': '', 'logprobs': {
        'tokens': [f'token_id:{token}' for token in ids],
        'top_logprobs': [{f'token_id:{token}': -.1} for token in ids]},
        'finish_reason': finish}]}


def engine_for(tok):
    engine = VLLMEngine('http://localhost', 'fixture', eos_token_ids=(tok.eos_token_id,))
    engine._tokenizer = tok
    return engine


def test_vllm_audit_equivalence(tok):
    ids = tok.encode(CASES[5])
    half = len(ids) // 2
    engine = engine_for(tok)
    first, _ = engine.parse_stream_chunk(frame(ids[:half]), 16)
    last, finish = engine.parse_stream_chunk(frame(ids[half:], 'stop'), 16)
    steps = first + last
    assert finish == 'stop'
    assert [step.token for step in steps] == ids
    text = ''.join(step.text for step in steps)
    assert text == CASES[5]
    job = dict(thinking_enabled=True, thinking_budget=1000, answer_budget=1000)
    executor, auditor = Budget(job), Budget(job)
    for step in steps:
        executor.add(step.text)
    audit_text = feed_openings_budget(tok, [{'token': token} for token in ids], auditor)
    assert audit_text.encode() == text.encode()
    assert (executor.reasoning, executor.content) == (auditor.reasoning, auditor.content) == (len(ids), 0)


@pytest.mark.parametrize('eos', [False, True])
def test_vllm_truncated_finish(tok, eos):
    ids = tok.encode(CASES[0])[:-1]
    engine = engine_for(tok)
    steps, _ = engine.parse_stream_chunk(frame(ids + ([tok.eos_token_id] if eos else []), 'stop'), 16)
    assert ''.join(step.text for step in steps) == tok.decode(ids)
    assert steps[-1].text == '\ufffd'


@pytest.mark.parametrize('eos', [False, True])
def test_shard_generate_replay(tok, eos):
    ids = tok.encode(CASES[4])
    engine = PipelineEngine.__new__(PipelineEngine)
    engine.tokenizer, engine.last_score_kind = tok, 'raw-logits'
    generated = ids + ([tok.eos_token_id] if eos else [])
    engine._step = lambda request, index, prefix: (generated[index], [(generated[index], 1.)], {})
    steps = list(engine.generate_for_request('a', 0, len(generated), 'fixture'))
    assert [step.token for step in steps] == generated
    assert ''.join(step.text for step in steps) == CASES[4]
    assert ''.join(step.text for step in engine.replay('a', 0, ids)) == CASES[4]


def test_shard_truncated_held_step(tok):
    ids = tok.encode(CASES[0])[:-1] + [tok.eos_token_id]
    engine = PipelineEngine.__new__(PipelineEngine)
    engine.tokenizer, engine.last_score_kind = tok, 'raw-logits'
    engine._step = lambda request, index, prefix: (ids[index], [(ids[index], 1.)], {})
    steps = []
    for step in engine.generate_for_request('a', 0, len(ids), 'fixture'):
        # Capture at delivery time to detect mutations after yield.
        steps.append((step.token, step.text))
    assert ''.join(text for _, text in steps) == tok.decode(ids[:-1])
    assert steps[-2][1] == '\ufffd' and steps[-1][1] == ''


@pytest.mark.parametrize('eos', [False, True])
def test_transformers_stubbed(tok, eos):
    import torch
    from engine.adapters.transformers import TransformersEngine
    engine = TransformersEngine.__new__(TransformersEngine)
    engine.torch, engine.device, engine.tokenizer = torch, 'cpu', tok
    engine.eos_token_ids, engine.tamper, engine.memory_budget = {tok.eos_token_id}, False, None
    engine.check_contribution_context = lambda *args: None
    ids = tok.encode(CASES[4]) + ([tok.eos_token_id] if eos else [])
    sequence = iter(ids)
    engine._step = lambda prefix: ([(next(sequence), 1.)], {})
    with patch('engine.adapters.transformers.check_memory'):
        steps = [(step.token, step.text) for step in engine._generate('a', 0, len(ids))]
        assert ''.join(text for _, text in steps) == CASES[4]
        sequence = iter(ids)
        assert ''.join(step.text for step in engine.replay('a', 0, ids)) == tok.decode(ids)


@pytest.mark.parametrize('mode,streaming', [('ok', False), ('json', True), ('rejected', True), ('other', True)])
def test_vllm_buffered_flush_and_reset(tok, mode, streaming):
    from test_http_streaming import fixture_server
    full = tok.encode(CASES[0])
    with fixture_server(mode, .001) as (url, _, _):
        engine = engine_for(tok)
        engine.endpoint, engine.streaming = url, streaming
        for ids in (full[:-1], full):
            with patch('test_http_streaming.frame', return_value=frame(ids, 'stop')):
                steps = list(engine.generate('a', 0, len(ids)))
            assert [step.token for step in steps] == ids
            assert ''.join(step.text for step in steps) == tok.decode(ids)


@pytest.mark.parametrize('truncated,eos', [(False, False), (True, False), (True, True)])
def test_transformers_forced_stubbed(tok, truncated, eos):
    import torch
    from types import SimpleNamespace
    from engine.adapters.transformers import TransformersEngine
    ids = tok.encode(CASES[0])
    if truncated:
        ids = ids[:-1]
    expected = tok.decode(ids)
    if eos:
        ids.append(tok.eos_token_id)
    engine = TransformersEngine.__new__(TransformersEngine)
    engine.torch, engine.device, engine.tokenizer = torch, 'cpu', tok
    engine.eos_token_ids, engine.hidden, engine.memory_budget = {tok.eos_token_id}, {}, None
    engine.check_contribution_context = lambda *args: None
    logits = torch.full((1, len(ids), len(tok)), -100.)
    for index, token in enumerate(ids):
        logits[0, index, token] = 1.
    engine.model = lambda **kwargs: SimpleNamespace(logits=logits)
    with patch('engine.adapters.transformers.check_memory'):
        steps = engine.score_forced('a', 0, ids)
    assert [step.token for step in steps] == ids
    assert ''.join(step.text for step in steps) == expected
    assert engine.score_forced('a', 0, []) == []
