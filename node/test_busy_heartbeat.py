"""A long token stream must not starve authenticated tunnel heartbeats."""
from collections import deque
from contextlib import nullcontext
import threading
import unittest
from unittest.mock import patch
from agent import Agent
from ember_pb2 import TunnelFrame, ResourceProfile
from engine import MockEngine


class BusyHeartbeatTest(unittest.TestCase):
    def test_busy_queue_still_sends_heartbeat(self):
        stop = threading.Event()
        seen = []
        agent = Agent('busy', MockEngine())
        agent.profile = lambda: ResourceProfile(node_id='busy')
        class BusyQueue:
            def __init__(self):
                self.frames = deque()
            def put(self, frame):
                self.frames.append(frame)
            def get(self, timeout):
                return self.frames.popleft() if self.frames else TunnelFrame(kind='token')
        class Stub:
            def __init__(self, channel):
                pass
            def Connect(self, messages):
                yield TunnelFrame(kind='ready')
                for _ in range(20):
                    seen.append(next(messages).kind)
                stop.set()
        ticks = iter(range(0, 1000, 10))
        with patch('agent.queue.Queue', BusyQueue), patch('agent.NodeTunnelStub', Stub), \
             patch('agent.time.monotonic', side_effect=lambda: next(ticks)):
            agent.serve('unused', stop=stop, channel_factory=lambda _: nullcontext(None))
        self.assertIn('heartbeat', seen)
        self.assertIn('token', seen)


if __name__ == '__main__':
    unittest.main()
