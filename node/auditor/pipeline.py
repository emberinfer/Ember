"""Full-model audit of independently signed shard-stage invocations."""
import hashlib
import math

from ember_core import node_id, verify
from ember_pb2 import (ChainGenesis, PipelineExecutionReceipt, ResourceProfile,
                       ShardForwardRequest, ShardForwardResponse)
from engine import Step
from evidence import canonical, encode_step, opening
from shard_rpc import verify_stage_response
from sharding import profile_span, execution_span
from .numeric import LEGACY, UNION, RANK_PROB, compare
from .gpu import gpu_engine_id
from .policy import engine_id, profile_engine_id, validate_policy


def reconstruct(job):
    receipt = PipelineExecutionReceipt.FromString(bytes.fromhex(job['pipeline_receipt']))
    if (not receipt.request_id or receipt.input_tokens < 1 or
            receipt.output_tokens < 1 or receipt.elapsed_ms < 1 or
            len(receipt.output_commitment) != 32 or
            len(job['invocations']) != len(receipt.stage_receipts)):
        raise ValueError('invalid pipeline aggregate')
    by_step = {}
    anchor = None
    for index, item in enumerate(job['invocations']):
        request = ShardForwardRequest.FromString(bytes.fromhex(item['request']))
        response = ShardForwardResponse.FromString(bytes.fromhex(item['response']))
        profile = ResourceProfile.FromString(bytes.fromhex(item['profile']))
        verified = verify_stage_response(request, response, profile)
        if verified.SerializeToString(deterministic=True) != receipt.stage_receipts[
                index].SerializeToString(deterministic=True):
            raise ValueError('pipeline aggregate receipt mismatch')
        if request.request_id != receipt.request_id or request.step >= receipt.output_tokens:
            raise ValueError('pipeline request binding mismatch')
        profile_anchor = (profile.chain_id, profile.manifest_root,
                          profile.execution_profile_ver,
                          profile.shard_total_layers)
        if anchor is None:
            anchor = profile_anchor
        elif profile_anchor != anchor:
            raise ValueError('inconsistent pipeline profile anchor')
        by_step.setdefault(request.step, []).append((request, profile, response))
    steps = []
    initial_tokens = None
    generated_tokens = []
    for step in range(receipt.output_tokens):
        values = sorted(by_step.get(step, []), key=lambda item: execution_span(item[0],item[1])[0])
        cursor = 0
        activations = {}
        terminal = None
        total = None
        previous = None
        for index, (request, profile, response) in enumerate(values):
            start, end = execution_span(request,profile)
            if start != cursor:
                raise ValueError('pipeline coverage gap or overlap')
            if total is not None and profile.shard_total_layers != total:
                raise ValueError('inconsistent pipeline layer count')
            cursor = end
            total = profile.shard_total_layers
            if index == 0:
                expected = list(initial_tokens or []) + generated_tokens
                if (not request.input_ids or request.hidden_bf16 or
                        request.hidden_shape or len(request.input_ids) !=
                        receipt.input_tokens + step):
                    raise ValueError('invalid first-stage input')
                if initial_tokens is None:
                    initial_tokens = list(request.input_ids)
                elif list(request.input_ids) != expected:
                    raise ValueError('pipeline token prefix mismatch')
            elif (request.input_ids or not _valid_hidden(
                    request.hidden_bf16, request.hidden_shape) or previous is None or
                    request.hidden_bf16 != previous.hidden_bf16 or
                    list(request.hidden_shape) != list(previous.hidden_shape)):
                raise ValueError('pipeline hidden-state chain mismatch')
            for layer, raw in response.activations.items():
                if layer in activations:
                    raise ValueError('duplicate layer evidence')
                activations[layer] = raw
            if profile.shard_has_lm_head and response.receipt.end_layer == profile.shard_total_layers:
                if (response.hidden_bf16 or response.hidden_shape or
                        not response.topk_ids or
                        len(response.topk_ids) != len(response.topk_logits) or
                        response.token_id != response.topk_ids[0] or
                        not all(math.isfinite(value) for value in response.topk_logits)):
                    raise ValueError('invalid terminal evidence')
                terminal = response
            elif (not _valid_hidden(response.hidden_bf16, response.hidden_shape) or
                  response.topk_ids or response.topk_logits):
                raise ValueError('invalid nonterminal output')
            previous = response
        if not total or cursor != total + 2 or terminal is None:
            raise ValueError('incomplete pipeline coverage')
        topk = list(zip(terminal.topk_ids, terminal.topk_logits))
        steps.append(Step(terminal.token_id, '', topk, activations))
        generated_tokens.append(terminal.token_id)
    expected_openings = [opening(step) for step in steps]
    if canonical(expected_openings) != canonical(job['openings']):
        raise ValueError('pipeline openings differ from signed responses')
    if hashlib.sha256(canonical([encode_step(step) for step in steps])).digest() != receipt.output_commitment:
        raise ValueError('pipeline output commitment mismatch')
    return receipt, steps


def _valid_hidden(raw, shape):
    if not raw or len(shape) != 3 or any(dimension < 1 for dimension in shape):
        return False
    count = 1
    for dimension in shape:
        count *= dimension
    return count * 2 == len(raw)


def load_pipeline_policy(job, reference):
    raw = bytes.fromhex(job.get('genesis', ''))
    genesis = ChainGenesis.FromString(raw)
    verify(genesis)
    policy_ref = hashlib.sha256(raw).hexdigest()
    if (job.get('policy_ref') != policy_ref or
            not genesis.execution_profile_ver.startswith(
                'transformers-qwen2.5-pipeline') or
            not genesis.HasField('numeric_audit_policy')):
        raise ValueError('invalid pipeline audit policy anchor')
    policy = genesis.numeric_audit_policy
    validate_policy(policy, allow_pipeline=True)
    profiles = []
    seen = set()
    for item in job['invocations']:
        profile = ResourceProfile.FromString(bytes.fromhex(item['profile']))
        verify(profile)
        if (profile.node_id != node_id(profile.signer) or
                profile.chain_id != genesis.chain_id or
                profile.manifest_root != genesis.model_manifest_root or
                profile.execution_profile_ver != genesis.execution_profile_ver or
                'shard-layer' not in profile.engine_capabilities.evidence_levels or
                'shard-stage' not in profile.engine_capabilities.roles):
            raise ValueError('pipeline producer profile does not match signed genesis')
        if profile.shard_dynamic_range and profile.shard_total_layers != len(reference.model.model.layers):
            raise ValueError('executed range profile differs from reference layer count')
        if profile.node_id not in seen:
            seen.add(profile.node_id)
            profiles.append(profile)
    if policy.metric == RANK_PROB:
        reference_engine = gpu_engine_id(reference.capabilities(), reference.resource_metadata())
        tolerances = set()
        for profile in profiles:
            if profile.requested_device != 'cuda' or profile.actual_device != 'cuda':
                raise ValueError('GPU pipeline requires explicit CUDA for every stage')
            producer_engine = profile_engine_id(profile)
            matches = [band for band in policy.bands if
                (band.producer_engine, band.producer_device, band.reference_engine, band.reference_device) ==
                (producer_engine, 'cuda', reference_engine, reference.device) and band.mode == 'enforce']
            if len(matches) != 1:
                raise ValueError('uncalibrated GPU stage/reference pair')
            tolerances.add(matches[0].tolerance)
        # A single aggregate threshold cannot safely widen a stricter stage's
        # signed band. Heterogeneous thresholds need per-component attribution.
        if len(tolerances) != 1:
            raise ValueError('GPU pipeline needs a common enforced stage/reference band')
        return policy.metric, next(iter(tolerances)), policy_ref
    producer_engines = sorted(set(profile_engine_id(profile) for profile in profiles))
    producer_devices = set(profile.actual_device for profile in profiles)
    if len(producer_engines) != 1 or len(producer_devices) != 1:
        raise ValueError('mixed uncalibrated pipeline runtime')
    producer_engine = f'pipeline[{producer_engines[0]}]'
    producer_device = next(iter(producer_devices))
    reference_engine = engine_id(reference.capabilities(),
                                 reference.resource_metadata())
    for band in policy.bands:
        if ((band.producer_engine, band.producer_device,
             band.reference_engine, band.reference_device) ==
                (producer_engine, producer_device, reference_engine,
                 reference.device)):
            if band.mode != 'enforce':
                raise ValueError('pipeline audit band is not enforceable')
            return policy.metric, band.tolerance, policy_ref
    raise ValueError('uncalibrated pipeline runtime pair')


def audit_pipeline(job, engine, tolerance, metric=LEGACY):
    try:
        receipt, actual = reconstruct(job)
    except (ValueError, TypeError, KeyError):
        return 'reject', math.inf, 'invalid signed shard evidence', []
    genesis = ChainGenesis.FromString(bytes.fromhex(job["genesis"]))
    if (genesis.price_input or genesis.price_context) and engine.count_input_tokens(job["prompt"]) != receipt.input_tokens:
        return "reject", math.inf, "input token count mismatch", []
    boundaries = set()
    for item in job['invocations']:
        profile = ResourceProfile.FromString(bytes.fromhex(item['profile']))
        response = ShardForwardResponse.FromString(bytes.fromhex(item['response']))
        if profile.shard_dynamic_range and response.hidden_bf16:
            boundaries.add(response.receipt.end_layer)
    if boundaries:
        from shard_rpc import bytes_tensor
        for item in job['invocations']:
            response = ShardForwardResponse.FromString(bytes.fromhex(item['response']))
            profile = ResourceProfile.FromString(bytes.fromhex(item['profile']))
            if profile.shard_dynamic_range and response.hidden_bf16:
                hidden = bytes_tensor(response.hidden_bf16,response.hidden_shape,'cpu')
                actual[response.receipt.step].activations['boundary:'+str(response.receipt.end_layer)] = hidden[0,-1].float().numpy().tobytes()
        reference = engine.replay_boundaries(job['prompt'],job['seed'],[step.token for step in actual],boundaries)
    else:
        reference = engine.replay(job['prompt'], job['seed'], [step.token for step in actual])
    distance, details = compare(actual, reference, metric)
    faults = [] if distance <= tolerance else fault_nodes(job, details, tolerance)
    return ('accept' if distance <= tolerance else 'dispute'), distance, details['reason'] + ('; replayed executed boundaries ' + str(sorted(boundaries)) if boundaries else ''), faults


def fault_nodes(job, details, tolerance):
    owners = {}
    terminal = set()
    for item in job['invocations']:
        profile = ResourceProfile.FromString(bytes.fromhex(item['profile']))
        response = ShardForwardResponse.FromString(bytes.fromhex(item['response']))
        receipt = response.receipt
        for layer in range(receipt.start_layer,receipt.end_layer):
            owners.setdefault(str(layer),set()).add(profile.node_id)
        if response.hidden_bf16:
            owners.setdefault('boundary:'+str(receipt.end_layer),set()).add(profile.node_id)
        if response.topk_ids:
            terminal.add(profile.node_id)
    faults = set()
    for layer, value in details.get('activation_by_layer', {}).items():
        if value > tolerance:
            faults.update(owners.get(layer, ()))
    if details.get('logit_max', 0.0) > tolerance:
        faults.update(terminal)
    return sorted(faults)


def profile_layer_names(profile):
    return {str(layer) for layer in range(profile.shard_start_layer,
                                          profile.shard_end_layer)}
