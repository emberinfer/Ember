"""Consensus-facing exact input counts require identical pinned tokenizer artifacts."""
import base64
import json
import queue
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from agent import Agent
from auditor import audit
from auditor.grading import offense_grade
from ember_core import sign
from ember_pb2 import ExecutionCommitment, ExecutionReceipt, TunnelFrame
from engine import MockEngine
from tokenization import count, artifact_version, TokenizerUnavailable


class ExactEngine(MockEngine):
    tokenizer_version = 'exact-test-v1'
    def count_input_tokens(self,prompt): return 128


class TokenizationTests(unittest.TestCase):
    def test_both_directions_are_objective_with_pinned_evidence(self):
        engine=ExactEngine();agent=Agent('f7',engine)
        for claimed in (127,129):
            c=ExecutionCommitment(input_tokens=claimed,tokenizer_version=engine.tokenizer_version)
            hold=dict(token_fee=dict(commitment=base64.b64encode(c.SerializeToString()).decode()))
            r=ExecutionReceipt(input_tokens=claimed);sign(r,agent.key)
            outcome,distance,reason=audit('prompt',1,r,[],engine,.001,
                verify_input_tokens=True,tokenizer_version=c.tokenizer_version)
            verdict,g=offense_grade(outcome,distance,reason,None,hold,
                tokenizer_version=engine.tokenizer_version,recomputed_input_tokens=128)
            self.assertEqual((verdict,getattr(g,'class'),g.fault,g.recomputed_input_tokens),
                             ('reject','objective','n_in',128))
            self.assertEqual(g.tokenizer_version,c.tokenizer_version)

    def test_version_mismatch_never_recounts(self):
        engine=ExactEngine()
        with patch.object(engine,'count_input_tokens',side_effect=AssertionError('must not recount')) as run:
            with self.assertRaises(TokenizerUnavailable): count(engine,'prompt','old-version')
            run.assert_not_called()
        r=ExecutionReceipt(input_tokens=128);sign(r,Agent('f7',engine).key)
        self.assertEqual(audit('prompt',1,r,[],engine,.001,verify_input_tokens=True,
            tokenizer_version='old-version')[::2],('inconclusive','tokenizer unavailable'))

    def test_unavailable_rejects_quote_and_run_before_generation(self):
        for kind,pricing in (('tokenize',True),('run',True),('tokenize',False),('run',False)):
            engine=ExactEngine();agent=Agent('f7',engine);out=queue.Queue()
            job=dict(pricing=pricing,prompt='prompt',seed=1,max_tokens=1)
            with patch.object(engine,'count_input_tokens',side_effect=OSError('missing tokenizer')), \
                 patch.object(engine,'generate_for_request',side_effect=AssertionError('generation must not start')) as run:
                agent._run(TunnelFrame(kind=kind,request_id='q',payload=json.dumps(job).encode()),out)
                run.assert_not_called()
            self.assertEqual([f.kind for f in out.queue],['error'])
            self.assertEqual(agent.completed_requests,0)
            self.assertEqual(agent.sessions,{})

    def test_missing_version_is_unavailable(self):
        engine=ExactEngine();engine.tokenizer_version=''
        with self.assertRaises(TokenizerUnavailable): count(engine,'prompt')

    def test_artifact_and_runtime_changes_change_identity(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp);(path/'tokenizer.json').write_text('{}')
            first=artifact_version(path)
            (path/'tokenizer_config.json').write_text('{"add_bos_token":true}')
            self.assertNotEqual(first,artifact_version(path))
            (path/'tokenizer.json').unlink()
            with self.assertRaises(TokenizerUnavailable):artifact_version(path)

    def test_pipeline_recount_uses_same_pin_in_both_directions(self):
        from auditor.pipeline import audit_pipeline
        from ember_pb2 import ChainGenesis, PipelineExecutionReceipt
        engine=ExactEngine()
        for claimed in (127,129):
            c=ExecutionCommitment(input_tokens=claimed,tokenizer_version=engine.tokenizer_version)
            job=dict(prompt='prompt',genesis=ChainGenesis(price_input=1).SerializeToString().hex(),
                token_fee=dict(commitment=base64.b64encode(c.SerializeToString()).decode()))
            with patch('auditor.pipeline.reconstruct',return_value=(PipelineExecutionReceipt(input_tokens=claimed),[])):
                result=audit_pipeline(job,engine,.001)
                self.assertEqual((result[0],result[2]),('reject','input token count mismatch'))
                c.tokenizer_version='other-version'
                job['token_fee']['commitment']=base64.b64encode(c.SerializeToString()).decode()
                result=audit_pipeline(job,engine,.001)
                self.assertEqual((result[0],result[2]),('inconclusive','tokenizer unavailable'))

    def test_missing_private_artifact_is_service_failure(self):
        from types import SimpleNamespace
        from privacy.executor import audit as private_audit
        from privacy.crypto import public
        from privacy.escrow import identity
        agent=Agent('f7',ExactEngine());sent=[]
        chain=SimpleNamespace(request=lambda _:dict(status='assigned',auditor=identity(public(agent.key))),
                              submit=lambda op,**fields:sent.append((op,fields)))
        verdict,stats=private_audit(chain,agent.key,None,dict(request_id='missing'),[],None,agent.engine)
        self.assertEqual((verdict,stats['reason']),('inconclusive','evidence unavailable'))
        self.assertEqual(sent[0][1]['verdict'],'inconclusive')
        self.assertNotIn('offense',sent[0][1])
