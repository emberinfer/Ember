"""Replay exactly one assigned range from signed producer inputs.

The head route consumes the producer's committed input to the head atom,
never the auditor's freshly computed layer output. For a mixed range this is
its signed pre-norm boundary activation; for a pure head it is request.hidden.
Dynamic responses use the existing topk_logits field for full-vocabulary
log-softmax scores. No top-k renormalization or new commitment is introduced.
"""
import hashlib
import base64
import math
import struct

from ember_core import verify, node_id
from ember_core.range_band import BAND, range_thresholds
from ember_pb2 import ChainGenesis, ResourceProfile, ShardForwardRequest, ShardForwardResponse, OffenseGrade
from .numeric import relative_distance, union_distance
from cryptography.exceptions import InvalidSignature


def stage_invocations(job):
    d = job['drawn_stage']
    start, end = d['start_layer'], d['end_layer']
    selected = []
    for item in job['invocations']:
        response = ShardForwardResponse.FromString(bytes.fromhex(item['response']))
        r = response.receipt
        if (r.node_id, r.start_layer, r.end_layer) == (d['node_id'], start, end):
            selected.append((ShardForwardRequest.FromString(bytes.fromhex(item['request'])),
                             ResourceProfile.FromString(bytes.fromhex(item['profile'])), response))
    if not selected:
        raise ValueError('missing drawn stage evidence')
    selected.sort(key=lambda v: v[0].step)
    if [r.step for r, _, _ in selected] != list(range(len(job['openings']))):
        raise ValueError('drawn stage evidence is missing or misaligned')
    for request, profile, _ in selected:
        if (d['has_embedding'] != (start == 0 and profile.shard_has_embedding)
                or d['has_lm_head'] != (end == profile.shard_total_layers and profile.shard_has_lm_head)):
            raise ValueError('drawn boundary atoms differ from signed profile')
    return selected


def load_range_policy(job, reference):
    raw = bytes.fromhex(job['genesis'])
    genesis = ChainGenesis.FromString(raw)
    verify(genesis)
    policy_ref = hashlib.sha256(raw).hexdigest()
    if job.get('policy_ref') != policy_ref or not genesis.numeric_audit_policy.HasField('range_band'):
        raise ValueError('invalid range policy anchor')
    if not genesis.execution_profile_ver.startswith('transformers-qwen2.5-pipeline'):
        raise ValueError('invalid range execution profile')
    params = genesis.numeric_audit_policy.range_band
    d = job['drawn_stage']
    if 'hold' in job:
        hold = job['hold']
        if not hold.get('assignments') or hold['assignments'][-1].get('drawn_stage') != d:
            raise ValueError('drawn stage differs from finalized assignment')
        auth = hold['pipeline']['authorizations'][d['index']]
        if (auth['node_id'], auth['start_layer'], auth['end_layer']) != (d['node_id'], d['start_layer'], d['end_layer']):
            raise ValueError('drawn stage differs from frozen authorization')
        if bytes.fromhex(job['pipeline_receipt']) != base64.b64decode(hold['pipeline']['receipt']):
            raise ValueError('pipeline receipt differs from finalized hold')
    total = None
    pairs = {}
    for _, profile, _ in stage_invocations(job):
        if 'hold' in job and profile.SerializeToString(deterministic=True) != base64.b64decode(auth['profile']):
            raise ValueError('range producer differs from frozen registration')
        verify(profile)
        if (profile.node_id != node_id(profile.signer) or profile.chain_id != genesis.chain_id
                or profile.manifest_root != genesis.model_manifest_root
                or profile.execution_profile_ver != genesis.execution_profile_ver):
            raise ValueError('range producer differs from signed genesis')
        if total is not None and total != profile.shard_total_layers:
            raise ValueError('inconsistent range layer count')
        total = profile.shard_total_layers
        config = reference.config if hasattr(reference, "config") else reference.model.config
        if total != config.num_hidden_layers:
            raise ValueError('reference layer count differs from producer')
        for head in (False, True):
            if head and not d['has_lm_head'] or not head and d['start_layer'] == d['end_layer'] and not d['has_embedding']:
                continue
            route = 'head' if head else 'layer'
            pair = range_thresholds(params, d['start_layer'], d['end_layer'], total,
                                    profile.actual_device, reference.device, head)
            if route in pairs and pair != pairs[route]:
                raise ValueError('range producer device changed')
            pairs[route] = pair
    if genesis.numeric_audit_policy.top_k != 16 or len(genesis.numeric_audit_policy.calibration_ref) != 32:
        raise ValueError('incomplete range calibration policy')
    # The assigned hold must freeze exactly the same values as the signed policy.
    if 'hold' in job:
        hold = job['hold']
        if not hold.get('assignments') or hold['assignments'][-1].get('drawn_stage') != d:
            raise ValueError('drawn stage differs from finalized assignment')
        frozen = hold.get('misjudge_thresholds', [])
        for route, pair in pairs.items():
            matches = [p for p in frozen if p['model'] == hold['model_id'] and p['band'] == BAND
                       and p['length_class'] == length_class(d, route)]
            if len(matches) != 1 or (matches[0]['theta_soft'], matches[0]['theta_hard']) != pair:
                raise ValueError('range thresholds differ from frozen policy')
    return params.metric, pairs, policy_ref


def length_class(d, route):
    return f"{route}:{d['start_layer']}-{d['end_layer']}"


def _reference_shard(engine):
    """Reuse already loaded own weights; never load any producer weight file."""
    from engine.shard import Qwen2ShardEngine
    if isinstance(engine, Qwen2ShardEngine):
        return engine
    # Full-model auditors are a superset. Execute only the requested slice with
    # the same decoder implementation as stage engines, without model.forward.
    from engine.device import check_memory, rss_bytes
    model = engine.model
    obj = Qwen2ShardEngine.__new__(Qwen2ShardEngine)
    obj.__dict__.update(torch=engine.torch, device=engine.device, dtype=engine.torch.bfloat16,
        config=model.config, start_layer=0, end_layer=len(model.model.layers),
        has_embedding=True, has_lm_head=True, embedding=model.model.embed_tokens,
        layers=model.model.layers, rotary=model.model.rotary_emb,
        norm=model.model.norm, lm_head=model.lm_head, last_activations={},
        check_memory=check_memory, rss_bytes=rss_bytes, memory_budget=engine.memory_budget)
    return obj


def audit_range(job, engine, thresholds):
    from .pipeline import reconstruct
    from shard_rpc import bytes_tensor
    try:
        receipt, _ = reconstruct(job)
        selected = stage_invocations(job)
        d = job['drawn_stage']
        shard = _reference_shard(engine)
        torch = shard.torch
        start, end = d['start_layer'], d['end_layer']
        if not shard.start_layer <= start <= end <= shard.end_layer:
            raise ValueError('reference does not cover drawn range')
        if d['has_embedding'] and not shard.has_embedding or d['has_lm_head'] and not shard.has_lm_head:
            raise ValueError('reference lacks assigned boundary atom')
        # Aggregate = mean, per FLR-5 supplemental calibration (CALIB-ADDENDUM);
        # max aggregation was rejected by the measured data.
        distances = {route: 0.0 for route in thresholds}
        counts = {route: 0 for route in thresholds}
        for request, profile, response in selected:
            if len(selected) != receipt.output_tokens:
                raise ValueError('missing drawn step')
            hidden = None
            ids = None
            if d['has_embedding']:
                if not request.input_ids or request.hidden_bf16:
                    raise ValueError('missing committed embedding input')
                ids = torch.tensor([list(request.input_ids)], dtype=torch.long, device=shard.device)
            else:
                hidden = bytes_tensor(request.hidden_bf16, request.hidden_shape, shard.device,
                                      shard.config.hidden_size, shard.config.max_position_embeddings)
            if 'layer' in thresholds:
                with torch.inference_mode():
                    output = shard.forward(input_ids=ids, hidden_states=hidden,
                        start_layer=start, end_layer=end, has_embedding=d['has_embedding'], has_lm_head=False)
                if d['has_lm_head']:
                    # Existing activation map is signed by response_hash and the
                    # aggregate output commitment, including this final layer.
                    raw = response.activations.get(str(end-1), b'')
                    if len(raw) != shard.config.hidden_size * 4:
                        raise ValueError('missing committed pre-head boundary')
                    actual = struct.unpack('<'+'f'*shard.config.hidden_size, raw)
                else:
                    committed = bytes_tensor(response.hidden_bf16, response.hidden_shape, 'cpu', shard.config.hidden_size)
                    if tuple(committed.shape) != tuple(output.shape):
                        raise ValueError('range output shape differs from commitment')
                    actual = committed[0, -1].float().tolist()
                value = relative_distance(actual, output[0, -1].float().tolist())
                if not math.isfinite(value):
                    raise ValueError('invalid committed layer evidence')
                distances['layer'] += value
                counts['layer'] += 1
            if 'head' in thresholds:
                if start == end:
                    head_input = hidden[:, -1:, :]
                else:
                    raw = response.activations.get(str(end-1), b'')
                    if len(raw) != shard.config.hidden_size * 4:
                        raise ValueError('missing committed head input')
                    values = struct.unpack('<'+'f'*shard.config.hidden_size, raw)
                    if not all(math.isfinite(v) for v in values):
                        raise ValueError('invalid committed head input')
                    head_input = torch.tensor(values, dtype=torch.bfloat16, device=shard.device).reshape(1, 1, -1)
                    # BF16->FP32 activation evidence must roundtrip byte-exactly.
                    if head_input.float().cpu().numpy().tobytes() != raw:
                        raise ValueError('head input is not a BF16 boundary opening')
                with torch.inference_mode():
                    scores = torch.log_softmax(shard.lm_head(shard.norm(head_input))[0, -1].float(), dim=-1)
                    top = torch.topk(scores, 16)
                actual = list(zip(response.topk_ids, response.topk_logits))
                if len(actual) != 16 or any(v > 0 for _, v in actual):
                    raise ValueError('missing committed terminal top-k logprobs')
                expected = list(zip(top.indices.tolist(), top.values.tolist()))
                # Existing sparse comparison: token union, kth-score interval
                # censoring for absent entries; no fabricated measured logits.
                value, _, _ = union_distance(actual, expected)
                distances['head'] += value
                counts['head'] += 1
        distances = {route: total / counts[route] for route, total in distances.items()}
        route = max(distances, key=lambda k: distances[k]/thresholds[k][0])
        job['_range_result'] = dict(distances=distances, route=route,
                                    length_class=length_class(d, route))
        failed = any(distances[r] > thresholds[r][0] for r in distances)
        return ('dispute' if failed else 'accept'), distances[route], 'single-range replay ' + str(distances), ([d['node_id']] if failed else [])
    except (ValueError, TypeError, KeyError, IndexError, struct.error, InvalidSignature):
        job.pop('_range_result', None)
        return 'reject', math.inf, 'invalid range evidence', []


def range_offense(job, verdict, distance):
    if verdict == 'reject' and math.isfinite(distance) and job.get('_range_result'):
        return OffenseGrade(**{'class': 'numerical', 'score': distance,
            'band': BAND, 'length_class': job['_range_result']['length_class']})
    return None


def load_range_reference(job):
    """Local worker loads only its registered capability (or the drawn slice)."""
    from engine.shard import Qwen2ShardEngine
    d = job['drawn_stage']
    return Qwen2ShardEngine(job['model_path'], d['start_layer'], d['end_layer'],
                           d['has_embedding'], d['has_lm_head'], job.get('device', 'auto'))


def confirm_range_results(job, first, thresholds):
    """Local double replay must grade the same route with its own distance."""
    second = job.get('_range_result')
    if not first or not second:
        return None
    common = [r for r in thresholds if first['distances'][r] > thresholds[r][0]
              and second['distances'][r] > thresholds[r][0]]
    if not common:
        return None
    route = max(common, key=lambda r: max(first['distances'][r], second['distances'][r])/thresholds[r][0])
    second.update(route=route, length_class=length_class(job['drawn_stage'], route))
    return max(first['distances'][route], second['distances'][route])
