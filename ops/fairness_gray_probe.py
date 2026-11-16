"""Produce signed, delivered outputs and measured numerical gray evidence.

Synthetic runtime perturbation on an isolated test chain; no hardware claim.
Writes JSON to stdout for TestFairnessGrayRefundReproduction.
"""
import hashlib
import json
import queue
import struct
import time

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from agent import Agent
from auditor import audit
from auditor.numeric import UNION
from ember_core import node_id
from ember_pb2 import ExecutionReceipt, TunnelFrame
from engine import MockEngine, Step
from evidence import opening


PROMPT = 'numerical-gray-fixture: near-boundary activation; repeat identical input'


class PerturbedExecutor(MockEngine):
    def _generate(self, prompt, seed, count):
        for _ in range(count):
            yield Step(15, 'output ', [(i, float(i)) for i in range(15, -1, -1)],
                       {k: struct.pack('<ff', 1.45, 0) for k in ('0', '14', '27')})


class IndependentReference:
    def replay(self, prompt, seed, tokens):
        return [Step(t, '', [(i, float(i)) for i in range(15, -1, -1)],
                     {k: struct.pack('<ff', 1, 0) for k in ('0', '14', '27')}) for t in tokens]


def main():
    key = Ed25519PrivateKey.from_private_bytes(bytes([2]) + bytes(31))
    executor = Agent(node_id(key.public_key().public_bytes_raw()), PerturbedExecutor(), key=key)
    rows = []
    for index in range(25):
        request = f'gray-repeat-{index + 1}'
        out = queue.Queue()
        start = time.perf_counter_ns()
        executor._run(TunnelFrame(request_id=request,
            payload=json.dumps(dict(prompt=PROMPT, seed=7, max_tokens=4)).encode()), out)
        elapsed = time.perf_counter_ns() - start
        frames = list(out.queue)
        receipt = ExecutionReceipt.FromString(next(f.payload for f in frames if f.kind == 'receipt'))
        opened = [opening(step) for step in executor.sessions[request][1]]
        references = [audit(PROMPT, 7, receipt, opened, IndependentReference(), .001,
            metric=UNION, required_layers=['0', '14', '27'], required_top_k=16) for _ in range(2)]
        assert all(v == 'dispute' and .4 <= score < .5 for v, score, _ in references), references
        rows.append(dict(request=request, receipt=receipt.SerializeToString().hex(),
            scores=[score for _, score, _ in references],
            output=b''.join(f.payload for f in frames if f.kind == 'token').decode(),
            delivered_tokens=receipt.output_tokens, executor_elapsed_ns=elapsed))
    print(json.dumps(dict(kind='synthetic-numerical-perturbation', prompt=PROMPT,
        prompt_sha256=hashlib.sha256(PROMPT.encode()).hexdigest(), executor=executor.node_id, rows=rows)))


if __name__ == '__main__':
    main()
