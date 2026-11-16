"""Certificate-fingerprint-pinned gRPC channel creation."""
import hashlib
import socket
import ssl

import grpc


def pinned_channel(address, fingerprint, options=None):
    host, port = address.rsplit(':', 1)
    if len(fingerprint) != 64:
        raise ValueError('expected SHA-256 TLS fingerprint')
    with socket.create_connection((host, int(port)), timeout=10) as sock:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE
        with context.wrap_socket(sock, server_hostname=host) as tls:
            der = tls.getpeercert(binary_form=True)
    if hashlib.sha256(der).hexdigest() != fingerprint.lower():
        raise ValueError('coordinator TLS fingerprint mismatch')
    pem = ssl.DER_cert_to_PEM_cert(der).encode()
    return grpc.secure_channel(address, grpc.ssl_channel_credentials(root_certificates=pem),
                               options=options)
