"""Local demo audit worker; consumes a JSON job, produces a machine-readable verdict."""
import json
import math
import gc
import sys
import resource
from pathlib import Path

sys.path[:0] = [str(Path(__file__).resolve().parents[2] / 'proto'),
                str(Path(__file__).resolve().parents[2] / 'core-py'),
                str(Path(__file__).resolve().parents[1])]
from ember_pb2 import ExecutionReceipt, ChainGenesis
from auditor import audit
from auditor.policy import load_job_policy, select_band, profile_engine_id, grade
from auditor.pipeline import audit_pipeline, load_pipeline_policy
from engine import load_engine


def main():
    job = json.load(sys.stdin)
    if job.get('kind') == 'pipeline':
        return pipeline_main(job)
    receipt = ExecutionReceipt.FromString(bytes.fromhex(job['receipt']))
    def load():
        return load_engine(job['engine'], model_path=job.get('model_path'),
                           device=job.get('device', 'auto'),
                           endpoint=job.get('engine_endpoint'), model=job.get('engine_model'))
    policy, producer, policy_ref = load_job_policy(job, receipt)
    first = load()
    band = (select_band(policy, profile_engine_id(producer), producer.actual_device, first)
            if policy is not None else None)
    comparison = ({'metric': policy.metric, 'required_layers': policy.layers, 'required_top_k': policy.top_k}
                  if policy is not None else {})
    if job.get("genesis"):
        genesis = ChainGenesis.FromString(bytes.fromhex(job["genesis"]))
        comparison["verify_input_tokens"] = bool(genesis.price_input or genesis.price_context)
    tolerance = band.tolerance if band is not None else job['tolerance']
    verdict, distance, reason = audit(job['prompt'], job['seed'], receipt, job['openings'],
                                      first, tolerance, **comparison)
    actual_device = first.device
    result_grade = 'legacy'
    if policy is not None:
        verdict, result_grade = grade(verdict, distance, band)
    if verdict == 'dispute':
        first.close()
        del first
        gc.collect()
        second = load()
        second_band = (select_band(policy, profile_engine_id(producer), producer.actual_device, second)
                       if policy is not None else None)
        if policy is not None and second_band != band:
            verdict, reason, result_grade = 'inconclusive', 'reference runtime changed', 'second-reference-runtime-mismatch'
        else:
            second_verdict, second_distance, _ = audit(job['prompt'], job['seed'], receipt,
                                                       job['openings'], second, tolerance, **comparison)
            if second_verdict == 'dispute' and (policy is None or math.isfinite(second_distance)):
                verdict, distance, reason = 'reject', max(distance, second_distance), 'two local reference executions'
            else:
                verdict, reason = 'inconclusive', 'reference disagreement'
    if not math.isfinite(distance):
        distance = 1e30
    print(json.dumps({'verdict': verdict, 'distance': distance, 'reason': reason,
                      'device': actual_device, 'grade': result_grade,
                      'policy_ref': policy_ref, 'metric': policy.metric if policy is not None else 'legacy-ranked-v1',
                      'reference_engine': band.reference_engine if band is not None else None,
                      'peak_rss_bytes': resource.getrusage(resource.RUSAGE_SELF).ru_maxrss * 1024}))


def pipeline_main(job):
    def load():
        return load_engine(job['engine'], model_path=job.get('model_path'),
                           device=job.get('device', 'auto'))
    first = load()
    metric, tolerance, policy_ref = load_pipeline_policy(job, first)
    verdict, distance, reason, faults = audit_pipeline(job, first, tolerance, metric)
    actual_device = first.device
    if verdict == 'dispute':
        first.close()
        del first
        gc.collect()
        second = load()
        try:
            second_metric, second_tolerance, second_policy_ref = load_pipeline_policy(job, second)
        except (ValueError, KeyError):
            second_verdict, second_distance, second_faults = 'inconclusive', math.inf, []
        else:
            if (second_policy_ref != policy_ref or second_tolerance != tolerance or
                    second_metric != metric):
                second_verdict, second_distance, second_faults = 'inconclusive', math.inf, []
            else:
                second_verdict, second_distance, _, second_faults = audit_pipeline(
                    job, second, second_tolerance, metric)
        if (second_verdict == 'dispute' and math.isfinite(second_distance) and
                faults and faults == second_faults):
            verdict, distance, reason = 'reject', max(distance, second_distance), 'two local reference executions'
        else:
            verdict, reason, faults = 'inconclusive', 'reference disagreement', []
    if not math.isfinite(distance):
        distance = 1e30
    print(json.dumps({'verdict': verdict, 'distance': distance, 'reason': reason,
                      'device': actual_device, 'grade': 'full-model-stage-signed',
                      'policy_ref': policy_ref, 'metric': metric,
                      'fault_node_ids': faults,
                      'peak_rss_bytes': resource.getrusage(resource.RUSAGE_SELF).ru_maxrss * 1024}))


if __name__ == '__main__':
    main()
