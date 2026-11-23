"""Isolated real-weight T2 conformance; writes local evidence, never network TX.

Reference bootstrap is a local repeatability check, not a T4 cross-device
certification. Operational callers must use authenticated published references.
"""
import argparse
import json
from pathlib import Path
import time

from engine.adapters.diffusers_t2i import DiffusersT2I, Formula, PROFILE, sha
from image_client import ImageClient, canonical


def main(argv=None):
    p = argparse.ArgumentParser()
    p.add_argument('--model', required=True)
    p.add_argument('--output', required=True)
    p.add_argument('--steps', type=int, default=2)
    p.add_argument('--sampler', choices=('ddim', 'euler'), default='ddim')
    p.add_argument('--threads', type=int, default=4)
    a = p.parse_args(argv)
    import torch
    torch.set_num_threads(a.threads)
    out = Path(a.output)
    out.mkdir(parents=True, exist_ok=True)
    e = DiffusersT2I(a.model)
    r = dict(prompt='a red cube on a blue table', negative_prompt='', seed=42,
             sampler=a.sampler, steps=a.steps, guidance=7500, resolution=0)
    start = time.time()
    paths, hashes = [], []
    for i, x in enumerate(e.trajectory(r)):
        path = out/f'reference-{i}.etv'
        raw = e.raw(x)
        path.write_bytes(raw)
        paths.append(str(path.resolve())); hashes.append(sha(raw))
        print('reference',i,hashes[-1],flush=True)
    calibration = dict(request=r, reference_paths=paths, latent_hashes=hashes,
                       max_distances=[0.0]*(a.steps+1))
    (out/'local-calibration.json').write_bytes(canonical(calibration))
    distances = e.self_test(calibration)
    print('self_test',distances,flush=True)
    c = ImageClient(e, out/'evidence', safety_seconds=3600)
    submitted = []
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
    from ember_core import node_id
    from image_client import verify_spool_receipt
    import base64
    gateway = Ed25519PrivateKey.generate()
    pub = gateway.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    def spool_for(hold_id):
        def spool(png, binding):
            # Local gateway double uses the real T1 signed receipt format.
            receipt = dict(request_id=hold_id, challenge_height=1,
                step_set_hash=base64.b64encode(bytes.fromhex(binding)).decode(),
                gateway=node_id(pub), received=True, height=2)
            receipt['signature'] = base64.b64encode(gateway.sign(
                b'EMBER-SPOT-SPOOL-V1\0'+canonical(receipt))).decode()
            return verify_spool_receipt(canonical(receipt), pub, hold_id, 1, binding, 2)
        return spool
    first = c.generate('11'*32, 'ember-stable-diffusion-1.5-v1', r, spool_for('11'*32), submitted.append)
    second = c.generate('22'*32, 'ember-stable-diffusion-1.5-v1', r, spool_for('22'*32), submitted.append)
    assert first['submission']['pixel_sha256'] == second['submission']['pixel_sha256']
    assert first['submission']['final_latent_hash'] == second['submission']['final_latent_hash']
    context = dict(first, **{k: first['submission'][k] for k in
                 ('image_sha256', 'image_bytes', 'pixel_sha256', 'final_latent_hash')})
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    key = Ed25519PrivateKey.generate()
    steps = list(range(a.steps+1))
    verdict = c.audit(context, c.opening(first['request_id'], steps), steps,
        dict(base=0.0, step_coef=0.0, hard_multiplier=1.0),
        dict(mode='both', max_relative_l2=0.0, min_within_one_fraction=1.0), key.sign)
    assert verdict['result']['verdict'] == 'accept'
    assert verdict['result']['distance'] == 0
    import base64
    key.public_key().verify(base64.b64decode(verdict['signature']),
                           b'EMBER-IMAGE-AUDIT-V1\0'+canonical(verdict['result']))
    # Same prompt, different real CLIP vocabulary must fail before model replay.
    import copy
    correct = e.tokenizer
    wrong = copy.deepcopy(correct)
    content = e.tokens(r)[0][1]
    piece = wrong.convert_ids_to_tokens(content)
    wrong.encoder[piece] = (content+1) % 49406
    e.tokenizer = wrong
    try:
        c.audit(context, c.opening(first['request_id'], steps), steps,
            dict(base=0.0, step_coef=0.0, hard_multiplier=1.0),
            dict(mode='both', max_relative_l2=0.0, min_within_one_fraction=1.0), key.sign)
    except ValueError as error:
        assert 'tokenizer' in str(error)
        wrong_tokenizer_detected = True
    else:
        raise AssertionError('wrong real tokenizer was not detected')
    finally:
        e.tokenizer = correct
    result = dict(execution_profile=PROFILE, request=r, local_only=True,
        wrong_tokenizer_detected=wrong_tokenizer_detected, calibration_distances=distances, pixel_hash=first['submission']['pixel_sha256'],
        repeat_pixel_hash=second['submission']['pixel_sha256'], audit=verdict,
        latent_hashes=hashes, seconds=time.time()-start, torch=torch.__version__)
    (out/'result.json').write_bytes(canonical(result))
    print(json.dumps(result, indent=2),flush=True)


if __name__ == '__main__':
    main()
