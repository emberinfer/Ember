"""Local transport-test peer: real Agent/gRPC, deterministic replay substitute."""
import hashlib
import json
import sys
from types import SimpleNamespace

from agent import Agent
from auditor import worker
from ember_pb2 import EvidenceChunk


def execute(agent, frame, job):
    raw = json.dumps(job, separators=(',', ':')).encode()
    return json.dumps({'size': len(raw), 'sha256': hashlib.sha256(raw).hexdigest(),
                       'retried': agent.retried}).encode()


class Peer(Agent):
    dropped = False
    retried = False

    def _run(self, frame, outgoing):
        if frame.kind == 'audit_chunk' and EvidenceChunk.FromString(frame.payload).index == 1:
            if not self.dropped:
                self.dropped = True
                class LostAck:
                    def put(_, reply):
                        if reply.kind != 'audit_chunk_ack':
                            outgoing.put(reply)
                return super()._run(frame, LostAck())
            self.retried = True
        return super()._run(frame, outgoing)


if __name__ == '__main__':
    worker.execute = execute
    agent = Peer('audit-probe', None, authorized_schedulers=[sys.argv[2]])
    # Audit-only nodes must still receive all transfer operations.
    agent.chain_args = SimpleNamespace(no_exec=True, no_audit=False)
    agent.serve(sys.argv[1])
