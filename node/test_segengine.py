"""Incremental numerical and session-boundary regressions on tiny BF16 Qwen2."""
import io
import json
from unittest.mock import patch

import pytest
import torch
from transformers import Qwen2Config, Qwen2ForCausalLM
from engine.shard import Qwen2ShardEngine
from ember_pb2 import ShardSpec, ShardForwardRequest
from shard_rpc import StageService, tensor_bytes


@pytest.fixture
def shards(tmp_path):
    torch.set_num_threads(2)
    torch.manual_seed(7)
    config = Qwen2Config(vocab_size=64, hidden_size=32, intermediate_size=64,
                        num_hidden_layers=2, num_attention_heads=4,
                        num_key_value_heads=2, max_position_embeddings=64,
                        tie_word_embeddings=True)
    model = Qwen2ForCausalLM(config).to(torch.bfloat16).eval()
    model.save_pretrained(tmp_path)
    return (Qwen2ShardEngine(tmp_path, 0, 1, True, False, 'cpu'),
            Qwen2ShardEngine(tmp_path, 1, 2, False, True, 'cpu'), model)


def test_incremental_and_head(shards):
    a, b, model = shards
    ids = torch.tensor([[1, 2, 3]])
    pa = pb = None
    for step in range(8):
        with torch.inference_mode():
            full_hidden = a.forward(input_ids=ids)
            full = b.forward(hidden_states=full_hidden)
            old = model(ids, use_cache=False).logits[:, -1:, :].float()
            hidden, pa = a.forward(input_ids=ids if step == 0 else ids[:, -1:],
                use_cache=True, past_key_values=pa, position_offset=0 if step == 0 else ids.shape[1]-1)
            logits, pb = b.forward(hidden_states=hidden, use_cache=True,
                past_key_values=pb, position_offset=0 if step == 0 else ids.shape[1]-1)
        assert full.shape == (1, 1, 64)
        torch.testing.assert_close(full, old, atol=.003, rtol=.02)
        torch.testing.assert_close(hidden[:, -1], full_hidden[:, -1], atol=.02, rtol=.02)
        torch.testing.assert_close(logits, full, atol=.003, rtol=.02)
        assert logits.argmax(-1).item() == full.argmax(-1).item()
        ids = torch.cat((ids, full.argmax(-1)), dim=1)


def test_cached_chunk_uses_absolute_causal_mask(shards):
    a, _, _ = shards
    ids = torch.tensor([[1, 2, 3, 4, 5]])
    _, past = a.forward(input_ids=ids[:, :3], use_cache=True)
    chunk, _ = a.forward(input_ids=ids[:, 3:], use_cache=True,
                         past_key_values=past, position_offset=3)
    full = a.forward(input_ids=ids)
    torch.testing.assert_close(chunk, full[:, 3:], atol=.02, rtol=.02)


@pytest.mark.parametrize('head', [False, True])
def test_session_validation_and_eviction(shards, head):
    a, b, _ = shards
    engine = b if head else a
    spec = ShardSpec(start_layer=int(head), end_layer=2 if head else 1,
                     has_embedding=not head, has_lm_head=head)
    service = StageService(engine, [], spec)
    service.session_limit = 2
    ids = torch.tensor([[1, 2, 3]])
    def call(name, step, value):
        hidden = a.forward(input_ids=value) if head else None
        request = ShardForwardRequest(request_id=name, step=step)
        if head:
            raw, shape = tensor_bytes(hidden)
            request.hidden_bf16 = raw; request.hidden_shape.extend(shape)
            inputs = dict(hidden_states=hidden)
        else:
            request.input_ids.extend(value[0].tolist())
            inputs = dict(input_ids=value)
        with patch.object(engine, 'forward', wraps=engine.forward) as spy:
            result = service.cached_forward(request, spec, 'scheduler', **inputs)
            return result, spy.call_args_list[0].kwargs['position_offset']
    assert call('one', 0, ids)[1] == 0
    grown = torch.tensor([[1, 2, 3, 4]])
    result, offset = call('one', 1, grown)
    assert offset == 3
    if not head: assert result.shape[1] == 4
    expected = engine.forward(hidden_states=a.forward(input_ids=grown)) if head else engine.forward(input_ids=grown)
    torch.testing.assert_close(result[:, -1], expected[:, -1], atol=.02, rtol=.02)
    # Same length progression is insufficient if step skips or prefix changes.
    assert call('one', 3, torch.tensor([[1, 2, 3, 4, 5]]))[1] == 0
    assert call('one', 4, torch.tensor([[9, 2, 3, 4, 5, 6]]))[1] == 0
    assert call('one', 0, ids)[1] == 0
    call('two', 0, ids); call('three', 0, ids)
    assert len(service.sessions) == 2
    assert call('one', 1, grown)[1] == 0
    assert call('one', 1, grown)[1] == 0  # backward/repeated step
    service.session_ttl = -1
    assert call('one', 2, torch.tensor([[1, 2, 3, 4, 5]]))[1] == 0


def test_resident_loop_failure_recovery():
    import pipeline_client as client
    cache_ids = []
    def fake(job, cache):
        cache_ids.append(id(cache))
        if job.get('fail'): raise ValueError('failed job')
        client.emit({'type': 'result'})
    output = io.StringIO()
    with patch.object(client, 'run_job', side_effect=fake), patch('sys.stdin', io.StringIO('{}\n{"fail":true}\n{}\n')), patch('sys.stdout', output):
        client.serve()
    events = [json.loads(line) for line in output.getvalue().splitlines()]
    assert [e['ok'] for e in events if e['type'] == 'job_end'] == [True, False, True]
    assert len(set(cache_ids)) == 1


def test_ambiguous_cached_head_recomputes_full_input(shards):
    a, b, _ = shards
    spec = ShardSpec(start_layer=1, end_layer=2, has_lm_head=True)
    service = StageService(b, [], spec)
    def request(step, ids):
        hidden = a.forward(input_ids=torch.tensor([ids]))
        raw, shape = tensor_bytes(hidden)
        return ShardForwardRequest(request_id='ambiguous', step=step,
            hidden_bf16=raw, hidden_shape=shape), hidden
    req, hidden = request(0, [1, 2, 3])
    service.cached_forward(req, spec, 'scheduler', hidden_states=hidden)
    req, hidden = request(1, [1, 2, 3, 4])
    original = b.forward
    def collapse_margin(**kwargs):
        result, past = original(**kwargs)
        if kwargs.get('past_key_values') is not None:
            result = result.clone()
            result[0, 0, :2] = 30  # a BF16 tie after cached decode
        return result, past
    with patch.object(b, 'forward', side_effect=collapse_margin) as spy:
        result = service.cached_forward(req, spec, 'scheduler', hidden_states=hidden)
    assert spy.call_count == 2
    assert spy.call_args_list[0].kwargs['hidden_states'].shape[1] == 1
    assert spy.call_args_list[1].kwargs['hidden_states'].shape[1] == 4
    torch.testing.assert_close(result, original(hidden_states=hidden))
