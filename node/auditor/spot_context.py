"""Derive pipeline replay anchors from finalized receipts, never job assertions."""
import base64
import hashlib
import json
import logging
import math
from pathlib import Path

from ember_core import verify
from ember_pb2 import ChainGenesis, PipelineExecutionReceipt, ShardForwardRequest
from engine import Step
from evidence import canonical, encode_step
from tokenization import version


class ContextDerivationError(ValueError):
    """Missing or inconsistent trusted context; no verdict may be signed."""


def derive_context(agent, frame, job, hold, query):
    try:
        return _derive(agent, frame, job, hold, query)
    except Exception as exc:
        # Do not log prompt, tokens, job contents, or operator configuration.
        logging.getLogger(__name__).error('spot context derivation failed (%s)', type(exc).__name__)
        raise ContextDerivationError('spot context derivation failed: ' + str(exc)) from exc


def _derive(agent, frame, job, hold, query):
    config_path = getattr(agent.chain_args, 'spot_profile_config', None)
    if not config_path:
        raise ValueError('spot profile configuration required')
    config = json.loads(Path(config_path).read_text())
    local = config['profiles'][hold['model_id']]
    profile_id = local['profile_id']
    if not isinstance(profile_id, str) or not profile_id:
        raise ValueError('missing local profile identity')
    genesis_raw = bytes.fromhex(local['genesis'])
    genesis = ChainGenesis.FromString(genesis_raw)
    verify(genesis)
    model = query('/model/' + hold['model_id'])
    # Legacy publications carry the genesis only inside the publication
    # record; mirror the privacy path's fallback. Either source must decode
    # to exactly the locally pinned genesis bytes, else fail closed.
    profile_b64 = model.get('signed_profile') or model['publication']['genesis']
    if (genesis.chain_id != hold['model_id'] or genesis_raw != agent.genesis_bytes
            or base64.b64decode(profile_b64, validate=True) != genesis_raw):
        raise ValueError('profile differs from finalized model')
    metric = genesis.numeric_audit_policy.metric
    if metric not in ('legacy-ranked-v1', 'union-interval-v1', 'rank-probability-v1', 'relative-l2-v1'):
        raise ValueError('missing or unsupported registered metric')
    equality = query('/parameters')['audit_equality']
    epsilon = equality['epsilon']
    if not equality['enabled'] or type(epsilon) not in (int, float) or not math.isfinite(epsilon) or epsilon <= 0:
        raise ValueError('enabled chain equality epsilon required')
    # A deployment key is pinned locally; never take it or file paths from jobs.
    public = bytes.fromhex(config['gateway_public'])
    if len(public) != 32:
        raise ValueError('invalid deployment gateway key')
    pipeline = hold['pipeline']
    raw = base64.b64decode(pipeline['receipt'], validate=True)
    receipt = PipelineExecutionReceipt.FromString(raw)
    if (receipt.request_id != frame.request_id or hold['id'] != frame.request_id
            or receipt.output_tokens != pipeline['spot_step_count']
            or receipt.input_tokens < 1 or receipt.output_tokens < 1
            or len(receipt.output_commitment) != 32):
        raise ValueError('receipt differs from finalized request/count')
    receipt_ref = hashlib.sha256(b'ember/pipeline-receipt/v1\0' +
        receipt.SerializeToString(deterministic=True)).hexdigest()
    if receipt_ref != hold['receipt_ref']:
        raise ValueError('receipt reference differs from finalized hold')
    if bytes.fromhex(job['pipeline_receipt']) != raw:
        raise ValueError('header receipt differs from finalized receipt')
    prompt, seed = job['prompt'], job['seed']
    if not isinstance(prompt, str) or type(seed) is not int:
        raise ValueError('invalid header prompt/seed')
    if job['metric'] != metric:
        raise ValueError('header metric differs from registered metric')
    # Header tolerance is required for a complete header, but cannot override chain epsilon.
    if type(job['tolerance']) not in (int, float) or not math.isfinite(job['tolerance']):
        raise ValueError('invalid header tolerance')
    expected_version = local['tokenizer_version']
    if not expected_version or version(agent.engine) != expected_version:
        raise ValueError('pinned tokenizer unavailable')
    tokens = agent.engine.tokenizer(prompt, return_tensors='pt').input_ids[0].tolist()
    if (not tokens or any(type(t) is not int or t < 0 for t in tokens)
            or len(tokens) != receipt.input_tokens or version(agent.engine) != expected_version):
        raise ValueError('request token count/version differs from receipt/profile')
    opened = job['openings']
    if not isinstance(opened, list) or len(opened) != receipt.output_tokens:
        raise ValueError('complete output openings required')
    steps = []
    for item in opened:
        if type(item['token']) is not int or not 0 <= item['token'] <= 0xffffffff:
            raise ValueError('invalid output token')
        steps.append(Step(item['token'], '', item['topk'],
            {k: bytes.fromhex(v) for k, v in item['activations'].items()},
            item.get('score_kind', 'raw-logits')))
    if hashlib.sha256(canonical([encode_step(s) for s in steps])).digest() != receipt.output_commitment:
        raise ValueError('pipeline output commitment mismatch')
    req = ShardForwardRequest.FromString(bytes.fromhex(job['first_invocation_request']))
    if (req.request_id != receipt.request_id or req.step != 0 or not req.input_ids
            or req.hidden_bf16 or req.hidden_shape):
        raise ValueError('invocation request differs from finalized hold')
    # Exact input_ids equality is intentionally checked by verify_pipeline_binding,
    # against the first request proven under the independent spot commitment root.
    return dict(kind='pipeline', request_id=frame.request_id, profile_id=profile_id,
        total_steps=receipt.output_tokens, prompt=prompt, seed=seed,
        request_tokens=tokens, claimed_tokens=[s.token for s in steps],
        invocation_request_id=req.request_id, metric=metric, epsilon=epsilon,
        gateway_public=public.hex())
