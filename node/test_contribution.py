import json
import queue
import threading
import unittest
from agent.contribution import Contribution, resolve
from agent import Agent
from engine import MockEngine
from ember_pb2 import TunnelFrame


class ContributionTest(unittest.TestCase):
    def test_preset_mapping_and_rounding(self):
        self.assertEqual(resolve('low', ['small','large'], 7, 101),
                         {'profiles':{'small':1}, 'global':1, 'budget_bytes':0})
        self.assertEqual(resolve('balanced',['small','large'],7,101),
                         {'profiles':{'small':3,'large':3},'global':3,'budget_bytes':50})
        self.assertEqual(resolve('balanced',['small'],1,10)['global'],1)
        self.assertEqual(resolve('full',['small','large'],7,101)['profiles'],{'small':0,'large':0})
        self.assertEqual(resolve('low',['small'],7,101,{'profiles':{},'global':2,'budget_bytes':8}),
                         {'profiles':{},'global':2,'budget_bytes':8})
        for overrides in ({'global':-1},{'profiles':{'x':True}},{'profiles':None},{'budget_bytes':-1}):
            with self.assertRaises(ValueError): resolve('full',['x'],1,100,overrides)

    def test_measured_budget_and_unknown_fail_closed(self):
        policy={'profiles':{'small':2},'global':0,'budget_bytes':100}
        # Synthetic fixtures test boundaries only; production values come from CUDA.
        c=Contribution(policy,'small',dict(weight_bytes=60,kv_bytes=20,peak_bytes=90,context_tokens=8))
        self.assertFalse(c.signal().unavailable)
        c.policy['budget_bytes']=99
        self.assertTrue(c.signal().unavailable)
        self.assertTrue(Contribution(policy,'small').signal().unavailable)
        self.assertTrue(Contribution(policy,'large',c.measurement).signal().unavailable)

    def test_parallel_local_admission_and_nonfault_refusal(self):
        c=Contribution({'profiles':{'x':1},'global':1,'budget_bytes':0},'x')
        barrier=threading.Barrier(16);results=[]
        def run():
            barrier.wait();results.append(c.acquire())
        threads=[threading.Thread(target=run) for _ in range(16)]
        for t in threads:t.start()
        for t in threads:t.join()
        self.assertEqual(sum(results),1)
        agent=Agent('node',MockEngine());agent.contribution=c
        outgoing=queue.Queue()
        agent._run(TunnelFrame(kind='run',payload=json.dumps({'prompt':'hi','seed':1,'max_tokens':1}).encode()),outgoing)
        self.assertEqual(outgoing.get().kind,'unavailable')
        self.assertEqual(agent.failed_requests,0)
        c.release();self.assertTrue(c.acquire());c.release()
