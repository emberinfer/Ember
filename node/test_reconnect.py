import contextlib
import threading
import unittest
from unittest.mock import patch
import grpc
from agent import Agent
from engine import MockEngine
from ember_pb2 import TunnelFrame

class ReconnectQueueTest(unittest.TestCase):
    def test_cancelled_iterator_cannot_consume_replacement_hello(self):
        stop=threading.Event();old=[];calls=[]
        class Disconnected(grpc.RpcError):
            def code(self):return grpc.StatusCode.UNAVAILABLE
        class Stub:
            def Connect(self,messages):
                calls.append(True)
                if len(calls)==1:
                    self.assert_hello(messages)
                    old.append(messages)
                    yield TunnelFrame(kind='ready')
                    raise Disconnected()
                with self_test.assertRaises(StopIteration):
                    next(old[0])
                self.assert_hello(messages)
                yield TunnelFrame(kind='ready')
                stop.set()
            def assert_hello(self,messages):
                self_test.assertEqual(next(messages).kind,'hello')
        self_test=self
        with patch('agent.NodeTunnelStub',lambda _:Stub()):
            Agent('test',MockEngine()).serve('unused',stop=stop,channel_factory=lambda _:contextlib.nullcontext())
        self.assertEqual(len(calls),2)
