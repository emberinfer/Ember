"""Local demo audit worker; consumes a JSON job, produces a machine-readable verdict."""
import json
import hashlib
import math
import gc
import sys
import resource
from pathlib import Path

sys.path[:0] = [str(Path(__file__).resolve().parents[2] / 'proto'),
                str(Path(__file__).resolve().parents[2] / 'core-py'),
                str(Path(__file__).resolve().parents[1])]
from ember_pb2 import ExecutionCommitment, ExecutionReceipt, ChainGenesis, ModelManifest
from auditor import audit
from auditor.policy import load_job_policy, select_band, profile_engine_id, grade
from auditor.pipeline import audit_pipeline, load_pipeline_policy, pipeline_offense
from engine import load_engine


def main():
    job = json.load(sys.stdin)
    if job.get('evidence_unavailable'):
        print(json.dumps(dict(verdict='inconclusive', distance=1e30,
            reason='evidence unavailable', grade='evidence-unavailable',
            policy_ref=hashlib.sha256(bytes.fromhex(job['genesis'])).hexdigest() if job.get('genesis') else None)))
        return
    if job.get('kind') == 'pipeline':
        return pipeline_main(job)
    receipt = ExecutionReceipt.FromString(bytes.fromhex(job['receipt']))
    if receipt.output_tokens and not job.get('openings'):
        print(json.dumps(dict(verdict='inconclusive',distance=1e30,reason='evidence unavailable',
            grade='evidence-unavailable',policy_ref=hashlib.sha256(bytes.fromhex(job['genesis'])).hexdigest() if job.get('genesis') else None)))
        return
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
        if job.get('manifest'):
            from content_policy import from_manifest
            comparison['content_policy'] = from_manifest(genesis,
                ModelManifest.FromString(bytes.fromhex(job['manifest'])))
        comparison["verify_input_tokens"] = bool(genesis.price_input or genesis.price_context)
        if comparison["verify_input_tokens"]:
            import base64
            try:
                c = ExecutionCommitment.FromString(base64.b64decode(job['token_fee']['commitment']))
                comparison['tokenizer_version'] = c.tokenizer_version
            except Exception:
                comparison['tokenizer_version'] = ''
    tolerance = band.tolerance if band is not None else job['tolerance']
    verdict, distance, reason = audit(job['prompt'], job['seed'], receipt, job['openings'],
                                      first, tolerance, **comparison)
    offense = None
    if reason == 'input token count mismatch':
        from tokenization import count, TokenizerUnavailable
        from auditor.grading import offense_grade
        from google.protobuf.json_format import MessageToDict
        try:
            n, pinned = count(first,job['prompt'],comparison.get('tokenizer_version'))
            verdict, signed_grade = offense_grade(verdict,distance,reason,band,job,
                tokenizer_version=pinned,recomputed_input_tokens=n)
            offense = MessageToDict(signed_grade,preserving_proto_field_name=True) if signed_grade else None
        except TokenizerUnavailable:
            verdict, reason = 'inconclusive','tokenizer unavailable'
    actual_device = first.device
    result_grade = 'legacy'
    if policy is not None:
        verdict, result_grade = (('accept', reason) if reason == 'profile-policy-compliant-refusal'
                                else grade(verdict, distance, band))
    if reason == 'profile-policy-compliant-refusal':
        result_grade = reason
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
    print(json.dumps({'offense':offense,'verdict': verdict, 'distance': distance, 'reason': reason,
                      'device': actual_device, 'grade': result_grade,
                      'policy_ref': policy_ref, 'metric': policy.metric if policy is not None else 'legacy-ranked-v1',
                      'reference_engine': band.reference_engine if band is not None else None,
                      'peak_rss_bytes': resource.getrusage(resource.RUSAGE_SELF).ru_maxrss * 1024}))


def pipeline_main(job):
    def load():
        if job.get("drawn_stage") is not None:
            from .range_pipeline import load_range_reference
            return load_range_reference(job)
        return load_engine(job['engine'], model_path=job.get('model_path'),
                           device=job.get('device', 'auto'))
    first = load()
    metric, tolerance, policy_ref = load_pipeline_policy(job, first)
    verdict, distance, reason, faults = audit_pipeline(job, first, tolerance, metric)
    offense = None
    if reason == 'input token count mismatch':
        import base64
        from tokenization import count, TokenizerUnavailable
        from auditor.grading import offense_grade
        from google.protobuf.json_format import MessageToDict
        from ember_core import node_id
        c = ExecutionCommitment.FromString(base64.b64decode(job['token_fee']['commitment']))
        try:
            n,pinned = count(first,job['prompt'],c.tokenizer_version)
            verdict,g = offense_grade(verdict,distance,reason,None,job,
                tokenizer_version=pinned,recomputed_input_tokens=n)
            offense = MessageToDict(g,preserving_proto_field_name=True) if g else None
            faults = [node_id(c.signer)] if g else []
        except TokenizerUnavailable:
            verdict,reason,faults = 'inconclusive','tokenizer unavailable',[]
    actual_device = first.device
    first_range_result = job.get("_range_result")
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
        if verdict == 'reject' and job.get('drawn_stage') is not None:
            from .range_pipeline import confirm_range_results
            confirmed = confirm_range_results(job, first_range_result, tolerance)
            if confirmed is None:
                verdict, reason, faults = 'inconclusive', 'reference route disagreement', []
            else:
                distance = confirmed
    offense = offense or (pipeline_offense(verdict, distance, job) if job.get("drawn_stage") is not None else None)
    if not math.isfinite(distance):
        distance = 1e30
    if job.get("drawn_stage") is None:
        offense = offense or pipeline_offense(verdict, distance)
    from google.protobuf.json_format import MessageToDict
    print(json.dumps({'verdict': verdict, 'distance': distance, 'reason': reason,
                      'offense': (offense if isinstance(offense, dict) else MessageToDict(offense)) if offense is not None else None,
                      'device': actual_device, 'grade': ('pipeline-range-v2' if job.get('drawn_stage') is not None else 'full-model-stage-signed'),
                      'policy_ref': policy_ref, 'metric': metric,
                      'fault_node_ids': faults,
                      'peak_rss_bytes': resource.getrusage(resource.RUSAGE_SELF).ru_maxrss * 1024}))


if __name__ == '__main__':
    main()
