"""Route GPU jobs only through explicit signed runtime/class tolerance bands.

An operator supplies candidate command argv and their expected runtime IDs in
EMBER_GPU_AUDIT_ROUTES. The worker independently measures its runtime and
checks the signed band; a mistaken route cannot widen a numerical tolerance.
"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

from ember_pb2 import ExecutionReceipt
from .gpu import GPU_PROFILES
from .policy import load_job_policy, profile_engine_id


def choose_route(policy, producer, candidates):
    producer_id = profile_engine_id(producer)
    choices = []
    for candidate in candidates:
        if not candidate.get('node_id') or candidate['node_id'] == producer.node_id:
            continue
        for band in policy.bands:
            if ((band.producer_engine, band.producer_device, band.reference_engine, band.reference_device) ==
                    (producer_id, producer.actual_device, candidate.get('engine_id'), 'cuda')
                    and band.mode == 'enforce'):
                # Prefer the identical class/runtime; cross-class requires its
                # own signed enforced band. Unknown/observe pairs never route.
                choices.append((band.reference_engine != producer_id,
                                candidate['node_id'], candidate, band))
    if not choices:
        return None, None
    _, _, candidate, band = min(choices, key=lambda row: row[:2])
    return candidate, band


def route_job(job, candidates, run=subprocess.run):
    receipt = ExecutionReceipt.FromString(bytes.fromhex(job['receipt']))
    policy, producer, policy_ref = load_job_policy(job, receipt)
    if producer is None or producer.execution_profile_ver not in GPU_PROFILES:
        raise ValueError('GPU router requires an authenticated GPU execution profile')
    candidate, band = choose_route(policy, producer, candidates)
    if candidate is None:
        return {'verdict': 'inconclusive', 'distance': 1e30,
                'reason': 'no calibrated GPU auditor', 'device': 'none',
                'grade': 'uncalibrated-pair', 'policy_ref': policy_ref, 'metric': policy.metric}
    command = candidate['command']
    if not isinstance(command, list) or not command or not all(isinstance(s, str) and s for s in command):
        raise ValueError('auditor command must be a nonempty argv list')
    routed = dict(job, engine='transformers', device='cuda', model_path=candidate['model_path'])
    try:
        result = run(command, input=json.dumps(routed), capture_output=True, text=True,
                     timeout=600, check=True)
    except subprocess.CalledProcessError as error:
        # Preserve model/device guard failures for the coordinator's retry log.
        if error.stderr:
            print(error.stderr, file=sys.stderr, end='')
        raise
    if result.stderr:
        print(result.stderr, file=sys.stderr, end='')
    verdict = json.loads(result.stdout)
    if (verdict.get('policy_ref') != policy_ref or verdict.get('metric') != policy.metric
            or (verdict.get('verdict') in ('accept', 'reject') and
                (verdict.get('device') != 'cuda' or verdict.get('reference_engine') != band.reference_engine))):
        raise ValueError('auditor result does not match selected GPU policy/runtime')
    verdict['audit_route'] = candidate['node_id']
    print(json.dumps({'event': 'gpu-audit-route', 'producer': producer.node_id,
                      'reference': candidate['node_id'], 'reference_engine': band.reference_engine,
                      'request_id': receipt.request_id, 'mode': band.mode,
                      'job_sha256': hashlib.sha256(json.dumps(job, sort_keys=True).encode()).hexdigest()}),
          file=sys.stderr)
    return verdict


def main():
    candidates = json.loads(Path(os.environ['EMBER_GPU_AUDIT_ROUTES']).read_text())
    print(json.dumps(route_job(json.load(sys.stdin), candidates), allow_nan=False))


if __name__ == '__main__':
    main()
