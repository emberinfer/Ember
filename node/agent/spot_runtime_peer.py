"""Synthetic runtime integration peer. Real transfer, spool, binding and verdict."""
import json
import struct
import sys
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import patch

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from agent import Agent
from auditor import worker
from auditor.spot_worker import execute as execute_spot
from engine import Step
from ember_core import node_id


class Reference:
    """Small deterministic model with two generated tokens and two layer values."""
    calls = 0

    def replay(self, prompt, seed, tokens):
        self.calls += 1
        assert self.calls == 1, 'cached challenge recomputed the prefix'
        assert prompt == 'fixture' and seed == 0
        assert tokens in ([7], [7,8])
        return [Step(7+i, '', [(7+i,1.0)], {'0':struct.pack('<f',1), '1':struct.pack('<f',2)})
                for i in range(len(tokens))]


def execute(agent, frame, job):
    hold = json.loads((Path(sys.argv[3])/'hold.json').read_text())
    def query(path):
        if path == '/parameters': return {}
        if path == '/state': return {'height':20}
        raise ValueError(path)
    with patch('auditor.worker._fresh_audit_nonce', return_value=0):
        return execute_spot(agent,frame,job,hold,query)


if __name__=='__main__':
    root=Path(sys.argv[3])
    key=Ed25519PrivateKey.from_private_bytes(bytes.fromhex((root/'seed').read_text()))
    from cryptography.hazmat.primitives.serialization import Encoding,PublicFormat
    identity=node_id(key.public_key().public_bytes(Encoding.Raw,PublicFormat.Raw))
    worker.execute=execute
    worker._fresh_audit_nonce=lambda agent: 0
    agent=Agent(identity,Reference(),key=key,authorized_schedulers=[sys.argv[2]])
    agent.chain_args=NS(no_exec=True,no_audit=False,state_dir=str(root),spot_context_dir=str(root/'spot-context'),
                        spot_spool_dir=str(root/'mirror'),ledger_chain_id='spot-harness')
    agent.serve(sys.argv[1])
