"""Atom-only replay never executes adjacent layers, even with larger own weights."""
import hashlib
from types import SimpleNamespace

import pytest
import torch
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import digest, node_id, sign
from ember_pb2 import (ChainGenesis, ModelManifest, NumericAuditPolicy, RangeBandParams,
    RangeDeviceBase, ResourceProfile, ShardSpec, ShardForwardRequest,
    ShardForwardResponse, PipelineExecutionReceipt)
from engine import Step
from engine.shard import Qwen2ShardEngine
from evidence import canonical, opening, encode_step
from shard_rpc import tensor_bytes, sign_stage_response
from auditor.pipeline import audit_pipeline, load_pipeline_policy


@pytest.mark.parametrize('draw', [0, 2])
def test_embedding_and_head_atoms_only_own_route(draw):
    torch.manual_seed(4)
    key = Ed25519PrivateKey.generate()
    engine = Qwen2ShardEngine.__new__(Qwen2ShardEngine)
    class Layer(torch.nn.Module):
        def forward(self, hidden, **_):
            return hidden + .125
    engine.__dict__.update(torch=torch, device='cpu', dtype=torch.bfloat16,
        start_layer=0, end_layer=2, has_embedding=True, has_lm_head=True,
        embedding=torch.nn.Embedding(32, 4).to(torch.bfloat16),
        layers=torch.nn.ModuleList([Layer(), Layer()]), rotary=lambda *_: None,
        norm=torch.nn.Identity(), lm_head=torch.nn.Linear(4, 32, bias=False).to(torch.bfloat16),
        config=SimpleNamespace(num_hidden_layers=2, hidden_size=4, max_position_embeddings=8),
        check_memory=lambda *_: None, memory_budget=None, rss_bytes=lambda: 0)
    manifest = ModelManifest(chain_id='atoms', total_layers=2)
    sign(manifest, key)
    base = RangeDeviceBase(producer_device='cpu', reference_device='cpu', base=.001)
    genesis = ChainGenesis(chain_id='atoms', model_manifest_root=digest(manifest),
        execution_profile_ver='transformers-qwen2.5-pipeline-range-v2',
        numeric_audit_policy=NumericAuditPolicy(top_k=16, calibration_ref=b'a'*32,
            range_band=RangeBandParams(metric='relative-l2-v1', base=[base], head_base=[base],
                                       depth_coef=.5, hard_multiplier=3)))
    sign(genesis, key)
    specs = [ShardSpec(index=0, start_layer=0, end_layer=0, has_embedding=True, unit_kind='embedding'),
             ShardSpec(index=1, start_layer=0, end_layer=2, unit_kind='layers'),
             ShardSpec(index=2, start_layer=2, end_layer=2, has_lm_head=True, unit_kind='lm_head')]
    profiles, invocations, responses, activations = [], [], [], {}
    raw, shape = b'', []
    top = None
    for spec in specs:
        spec.parameters_hash = bytes([spec.index+1])*32
        profile = ResourceProfile(chain_id='atoms', manifest_root=digest(manifest),
            execution_profile_ver=genesis.execution_profile_ver, actual_device='cpu',
            shard_index=spec.index, shard_start_layer=spec.start_layer, shard_end_layer=spec.end_layer,
            shard_total_layers=2, shard_has_embedding=spec.has_embedding, shard_has_lm_head=spec.has_lm_head,
            shard_unit_kind=spec.unit_kind, shard_parameters_hash=spec.parameters_hash)
        sign(profile, key); profile.node_id = node_id(profile.signer); sign(profile, key)
        profiles.append(profile)
        request = ShardForwardRequest(request_id='atoms', step=0)
        kwargs = dict(start_layer=spec.start_layer, end_layer=spec.end_layer,
                      has_embedding=spec.has_embedding, has_lm_head=spec.has_lm_head)
        if spec.has_embedding:
            request.input_ids.append(1)
            kwargs['input_ids'] = torch.tensor([[1]])
        else:
            from shard_rpc import bytes_tensor
            request.hidden_bf16 = raw; request.hidden_shape.extend(shape)
            kwargs['hidden_states'] = bytes_tensor(raw, shape, 'cpu')
        with torch.inference_mode():
            result = engine.forward(**kwargs)
        response = ShardForwardResponse(shard_index=spec.index, shard_parameters_hash=spec.parameters_hash,
                                        activations=engine.last_activations)
        activations.update(engine.last_activations)
        if spec.has_lm_head:
            top = torch.topk(torch.log_softmax(result[0, -1], dim=-1), 16)
            response.token_id = int(top.indices[0])
            response.topk_ids.extend(top.indices.tolist()); response.topk_logits.extend(top.values.tolist())
        else:
            raw, shape = tensor_bytes(result)
            response.hidden_bf16 = raw; response.hidden_shape.extend(shape)
        sign_stage_response(request, response, key, genesis, manifest, spec)
        responses.append(response)
        invocations.append(dict(request=request.SerializeToString().hex(), response=response.SerializeToString().hex(),
                                profile=profile.SerializeToString().hex()))
    step = Step(int(top.indices[0]), '', list(zip(top.indices.tolist(), top.values.tolist())), activations, 'logprobs')
    receipt = PipelineExecutionReceipt(request_id='atoms', input_tokens=1, output_tokens=1, elapsed_ms=1,
        stage_receipts=[r.receipt for r in responses], output_commitment=hashlib.sha256(canonical([encode_step(step)])).digest())
    spec = specs[draw]
    job = dict(genesis=genesis.SerializeToString().hex(),
        policy_ref=hashlib.sha256(genesis.SerializeToString()).hexdigest(),
        pipeline_receipt=receipt.SerializeToString().hex(), invocations=invocations, openings=[opening(step)],
        drawn_stage=dict(index=draw, node_id=profiles[draw].node_id, start_layer=spec.start_layer,
            end_layer=spec.end_layer, has_embedding=spec.has_embedding, has_lm_head=spec.has_lm_head))
    # Any adjacent decoder execution is a test failure, not merely a larger distance.
    for layer in engine.layers:
        def forbidden(*_, **__):
            raise AssertionError('atom replay executed adjacent decoder layer')
        layer.forward = forbidden
    metric, thresholds, _ = load_pipeline_policy(job, engine)
    result = audit_pipeline(job, engine, thresholds, metric)
    assert result[0] == 'accept', result
    assert set(job['_range_result']['distances']) == ({'layer'} if draw == 0 else {'head'})


def test_local_confirmation_does_not_mix_layer_and_head_scores():
    from auditor.range_pipeline import confirm_range_results
    thresholds = {'layer': (.01, .03), 'head': (.001, .003)}
    first = dict(distances=dict(layer=.02, head=0))
    job = dict(drawn_stage=dict(start_layer=1, end_layer=2),
               _range_result=dict(distances=dict(layer=0, head=.004)))
    assert confirm_range_results(job, first, thresholds) is None
    first['distances']['head'] = .005
    assert confirm_range_results(job, first, thresholds) == .005
    assert job['_range_result']['length_class'] == 'head:1-2'
