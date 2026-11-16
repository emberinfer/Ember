"""Certificate-fingerprint-pinned gRPC channel creation."""
import hashlib
import socket
import ssl

import grpc
from cryptography import x509


def _pinned_options(der, options):
    """Use the pinned certificate's own name for gRPC's redundant name check."""
    cert = x509.load_der_x509_certificate(der)
    try:
        san = cert.extensions.get_extension_for_class(x509.SubjectAlternativeName).value
    except x509.ExtensionNotFound:
        san = None
    names = san.get_values_for_type(x509.DNSName) if san is not None else []
    ips = san.get_values_for_type(x509.IPAddress) if san is not None else []
    if not names and not ips:
        raise ValueError('certificate cannot be used for a pinned channel: '
                         'regenerate it with a DNS or IP SAN (emberd cert --host)')
    result = list(options or ())
    if not any(key == 'grpc.ssl_target_name_override' for key, _ in result):
        result.append(('grpc.ssl_target_name_override', names[0] if names else str(ips[0])))
    return result


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
    options = _pinned_options(der, options)
    pem = ssl.DER_cert_to_PEM_cert(der).encode()
    return grpc.secure_channel(address, grpc.ssl_channel_credentials(root_certificates=pem),
                               options=options)
