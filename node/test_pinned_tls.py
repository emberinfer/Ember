"""Real TLS transport coverage for certificate-fingerprint-pinned channels."""
import datetime
import hashlib
import ipaddress
import sys
import unittest
from concurrent import futures
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))

import grpc
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.x509.oid import NameOID

from pinned_tls import _pinned_options, pinned_channel


def make_cert(sans):
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, 'test')])
    now = datetime.datetime.now(datetime.timezone.utc)
    builder = (x509.CertificateBuilder().subject_name(name).issuer_name(name)
               .public_key(key.public_key()).serial_number(x509.random_serial_number())
               .not_valid_before(now - datetime.timedelta(minutes=1))
               .not_valid_after(now + datetime.timedelta(days=1)))
    if sans is not None:
        builder = builder.add_extension(x509.SubjectAlternativeName(sans), critical=False)
    cert = builder.sign(key, hashes.SHA256())
    return (key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
                              serialization.NoEncryption()),
            cert.public_bytes(serialization.Encoding.PEM),
            cert.public_bytes(serialization.Encoding.DER))


class PinnedTLSTest(unittest.TestCase):
    def server(self, sans):
        key, pem, der = make_cert(sans)
        self.calls = 0

        def echo(request, context):
            self.calls += 1
            return request

        server = grpc.server(futures.ThreadPoolExecutor(max_workers=2))
        server.add_generic_rpc_handlers((grpc.method_handlers_generic_handler(
            'test.Echo', {'Echo': grpc.unary_unary_rpc_method_handler(echo)}),))
        port = server.add_secure_port('127.0.0.1:0', grpc.ssl_server_credentials(((key, pem),)))
        self.assertNotEqual(port, 0)
        server.start()
        self.addCleanup(lambda: server.stop(0).wait())
        return f'127.0.0.1:{port}', pem, der

    def echo(self, channel):
        self.addCleanup(channel.close)
        self.assertEqual(channel.unary_unary('/test.Echo/Echo')(b'hello', timeout=5), b'hello')

    def test_pinned_hostname_mismatch(self):
        address, _, der = self.server([x509.DNSName('seed.internal.example')])
        self.echo(pinned_channel(address, hashlib.sha256(der).hexdigest().upper()))
        self.assertEqual(self.calls, 1)

    def test_fingerprint_mismatch_before_channel_creation(self):
        address, _, _ = self.server([x509.DNSName('seed.internal.example')])
        _, _, other = make_cert([x509.DNSName('seed.internal.example')])
        with patch('pinned_tls.grpc.secure_channel') as create:
            with self.assertRaisesRegex(ValueError, 'fingerprint mismatch'):
                pinned_channel(address, hashlib.sha256(other).hexdigest())
            create.assert_not_called()
        self.assertEqual(self.calls, 0)

    def test_pinned_hostname_match_ip(self):
        address, _, der = self.server([x509.IPAddress(ipaddress.ip_address('127.0.0.1'))])
        self.echo(pinned_channel(address, hashlib.sha256(der).hexdigest()))

    def test_unpinned_hostname_mismatch_rejected(self):
        address, pem, _ = self.server([x509.DNSName('seed.internal.example')])
        with grpc.secure_channel(address, grpc.ssl_channel_credentials(root_certificates=pem)) as channel:
            with self.assertRaises(grpc.RpcError) as error:
                channel.unary_unary('/test.Echo/Echo')(b'hello', timeout=3)
            self.assertEqual(error.exception.code(), grpc.StatusCode.UNAVAILABLE)
        self.assertEqual(self.calls, 0)

    def test_options_preserved_and_dns_preferred(self):
        _, _, der = make_cert([x509.IPAddress(ipaddress.ip_address('127.0.0.1')),
                               x509.DNSName('first.example'), x509.DNSName('second.example')])
        options = [('grpc.max_send_message_length', 123), ('grpc.max_receive_message_length', 456)]
        self.assertEqual(_pinned_options(der, options), options + [
            ('grpc.ssl_target_name_override', 'first.example')])
        self.assertEqual(len(options), 2)
        override = options + [('grpc.ssl_target_name_override', 'caller.example')]
        self.assertEqual(_pinned_options(der, override), override)

    def test_no_san_fails_closed(self):
        address, _, der = self.server(None)
        with patch('pinned_tls.grpc.secure_channel') as create:
            with self.assertRaisesRegex(ValueError, 'regenerate.*SAN'):
                pinned_channel(address, hashlib.sha256(der).hexdigest())
            create.assert_not_called()
        self.assertEqual(self.calls, 0)
        with self.assertRaisesRegex(ValueError, 'regenerate.*SAN'):
            _pinned_options(der, [('grpc.ssl_target_name_override', 'caller.example')])


if __name__ == '__main__':
    unittest.main()
