"""Stage message limits are independent of the per-tensor safety bound."""
from concurrent import futures
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

sys.path[:0] = [str(Path(__file__).resolve().parents[1] / 'proto'),
                str(Path(__file__).resolve().parents[1] / 'core-py'),
                str(Path(__file__).resolve().parent)]

import grpc
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
import moe_rpc


class MoeMessageLimitTests(unittest.TestCase):
    def test_client_options_and_tensor_bound(self):
        with patch.object(moe_rpc, 'pinned_channel') as channel, \
                patch.object(moe_rpc, 'ShardPipelineStub'):
            client = moe_rpc._BaseClient('127.0.0.1:1@fingerprint',
                                         Ed25519PrivateKey.generate())
            options = dict(channel.call_args.args[2])
            self.assertEqual(options, {
                'grpc.max_send_message_length': 335_544_320,
                'grpc.max_receive_message_length': 335_544_320})
            self.assertEqual(moe_rpc.MAX_MESSAGE_BYTES, 320 * 1024 * 1024)
            self.assertEqual(moe_rpc.MAX_TENSOR_BYTES, 64 * 1024 * 1024)
            # The design's largest stage frame fits without allocating it.
            largest_frame = 561_308 * 512 - 2_090_000
            self.assertGreater(largest_frame, moe_rpc.MAX_TENSOR_BYTES)
            self.assertLess(largest_frame, min(options.values()))
            client.close()

    def test_local_grpc_crosses_old_message_limit(self):
        # A loopback-only wire probe exercises both directions, with one 65MiB
        # buffer rather than a full-size model or 320MiB activation fixture.
        options = tuple((name, moe_rpc.MAX_MESSAGE_BYTES) for name in (
            'grpc.max_send_message_length', 'grpc.max_receive_message_length'))
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=1),
                             options=options)
        server.add_generic_rpc_handlers((grpc.method_handlers_generic_handler(
            'limit.Probe', {'Echo': grpc.unary_unary_rpc_method_handler(
                lambda raw, context: raw)}),))
        port = server.add_insecure_port('127.0.0.1:0')
        self.assertNotEqual(port, 0)
        server.start()
        try:
            raw = b'x' * (65 * 1024 * 1024)
            with grpc.insecure_channel(f'127.0.0.1:{port}', options=options) as channel:
                echoed = channel.unary_unary('/limit.Probe/Echo')(raw, timeout=20)
                self.assertEqual(echoed, raw)
                del echoed
            old_options = tuple((name, 64 * 1024 * 1024) for name, _ in options)
            with grpc.insecure_channel(f'127.0.0.1:{port}', options=old_options) as channel:
                with self.assertRaises(grpc.RpcError) as rejected:
                    channel.unary_unary('/limit.Probe/Echo')(raw, timeout=20)
                self.assertEqual(rejected.exception.code(), grpc.StatusCode.RESOURCE_EXHAUSTED)
        finally:
            server.stop(0).wait()


if __name__ == '__main__':
    unittest.main()
