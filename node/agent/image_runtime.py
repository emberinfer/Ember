"""T2 image worker over the existing agent tunnel and signed TX envelopes.

The gateway (T3) injects image_transport: spool(png,binding,hold_id,hold),
opening(job,context). The host submits returned envelopes. No gateway URL is inferred from jobs.
"""
import base64
import json
from pathlib import Path

from image_client import ImageClient, canonical, sha, chain_verdict_action


def execute(agent, frame, job):
    from .join import _chain_json
    from auditor.spot_worker import envelope_result
    args = agent.chain_args
    if frame.kind not in ('run', 'audit', 'audit_commit'):
        raise ValueError('unsupported image operation')
    if frame.kind != 'run' and getattr(args, 'no_audit', False):
        raise ValueError('image audit opted out')
    if not getattr(args, 'chain_rpc', None):
        raise ValueError('image runtime requires finalized chain queries')
    def query(path):
        return _chain_json(args.chain_cli, args.chain_rpc, path)
    hold = query('/hold/' + frame.request_id)
    if 'image' not in hold or hold['model_id'] != args.chain:
        raise ValueError('image hold differs from joined profile')
    transport = getattr(agent, 'image_transport', None)
    if transport is None:
        raise ValueError('operator-pinned image spool transport required')
    client = ImageClient(agent.engine, Path(args.state_dir)/'image-evidence',
                         getattr(args, 'image_retention_seconds', 3600))
    model = query('/model/' + hold['model_id'])
    profile = model['image_profile']
    resolutions = tuple((r['height'], r['width']) for r in profile['allowed_resolutions'])
    if resolutions != agent.engine.resolutions:
        raise ValueError('published image resolutions differ from JOIN profile')
    r, request_id = hold['image']['request'], hold['request_id']
    if frame.kind == 'run':
        if hold['node'] != agent.node_id or hold['state'] != 'frozen':
            raise ValueError('not finalized image executor')
        actions = []
        def spool(png, binding):
            from image_client import verify_spool_receipt
            raw = transport.spool(png, binding, frame.request_id, hold)
            return verify_spool_receipt(raw, transport.gateway_public_key, frame.request_id,
                                        hold['freeze_height'], binding, query('/state')['height'])
        meta = client.generate(request_id, hold['model_id'], r,
            spool,
            actions.append, hold_id=frame.request_id)
        return envelope_result(agent, actions[0], dict(kind='image', root=meta['root'],
                                                     submission=meta['submission']))
    challenge = hold['pipeline']['spot']
    if hold['state'] != 'dispute' or hold['auditor'] != agent.node_id or challenge['auditor'] != agent.node_id:
        raise ValueError('not finalized image auditor')
    state = query('/state')
    if state['height'] > challenge['verdict_deadline']:
        raise ValueError('image challenge expired')
    sub = hold['image']['submission']
    ctx = dict(request=r, request_id=request_id, model_id=hold['model_id'],
               root=base64.b64decode(hold['pipeline']['commitment_root']).hex(),
               **{k: sub[k] for k in ('image_sha256','pixel_sha256','image_bytes','final_latent_hash')})
    # Pixel policy is an operator-pinned T4 artifact, never accepted in a job.
    policy = json.loads(Path(args.image_pixel_policy).read_text())
    steps = challenge['step_set']
    try:
        opened = transport.opening(job, ctx)
        signed = client.audit(ctx, opened, steps, profile['range_band'], policy, agent.key.sign)
        result = signed['result']
    except (ValueError, KeyError) as exc:
        result = dict(request_id=request_id, root=ctx['root'], challenge_steps=steps,
            distance=0.0, verdict='reject', pixels=None, binding_error=str(exc),
            per_step=[dict(step_index=i, distance=0.0, outcome='binding_fail') for i in steps])
    evidence = canonical(dict(context=ctx, result=result))
    directory = Path(args.state_dir)/'image-audit'/request_id
    directory.mkdir(parents=True, exist_ok=True)
    from image_client import durable
    durable(directory/f"{challenge['challenge_height']}.json", evidence)
    action = chain_verdict_action(agent, frame.request_id, hold, result, sha(evidence))
    return envelope_result(agent, action, result)
