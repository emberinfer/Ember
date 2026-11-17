"""Local-only synthetic engine through the actual Agent.serve forwarding path."""
import sys
import time
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from agent import Agent
from ember_core import node_id
from cryptography.hazmat.primitives import serialization
from engine import MockEngine

class PacedEngine(MockEngine):
    def _generate(self, prompt, seed, count):
        deadline = time.monotonic()
        for step in super()._generate(prompt, seed, count):
            deadline += 1 / 200
            time.sleep(max(0, deadline - time.monotonic()))
            yield step

key = Ed25519PrivateKey.from_private_bytes(bytes.fromhex(sys.argv[2]))
Agent(node_id(key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)), PacedEngine(), key=key).serve(sys.argv[1])
