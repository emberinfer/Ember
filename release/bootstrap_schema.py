"""Validate public bootstrap anchors; null placeholders never authorize sync."""
import re
from datetime import datetime
from urllib.parse import urlparse


def validate_bootstrap(value):
    if not isinstance(value, dict) or value.get('status') not in ('pending-publication', 'ready'):
        raise ValueError('bootstrap.status must be pending-publication or ready')
    fields = {'status', 'genesis_url', 'genesis_sha256', 'statesync', 'snapshot_peers'}
    if set(value) != fields:
        raise ValueError('bootstrap has missing or unknown fields')
    sync = value['statesync']
    sync_fields = {'trust_height', 'trust_hash', 'trust_period', 'trusted_header_time', 'expires_at', 'rpc_servers'}
    if not isinstance(sync, dict) or set(sync) != sync_fields:
        raise ValueError('bootstrap.statesync has missing or unknown fields')
    if value['status'] == 'pending-publication':
        if (value['genesis_url'] is not None or value['genesis_sha256'] is not None or value['snapshot_peers'] != [] or
                any(sync[k] is not None for k in sync_fields - {'rpc_servers'}) or sync['rpc_servers'] != []):
            raise ValueError('pending bootstrap must use null anchors and empty peer/RPC lists')
        return
    def https_url(url):
        if not isinstance(url, str): return False
        parsed = urlparse(url)
        return parsed.scheme == 'https' and bool(parsed.hostname) and not parsed.username and not parsed.fragment
    for key in ('genesis_sha256',):
        if not isinstance(value[key], str) or not re.fullmatch('[0-9a-fA-F]{64}', value[key]):
            raise ValueError('bootstrap.' + key + ' must be an exact-byte 64-hex hash')
    if not https_url(value['genesis_url']):
        raise ValueError('bootstrap.genesis_url must be immutable HTTPS')
    if type(sync['trust_height']) is not int or sync['trust_height'] <= 0:
        raise ValueError('bootstrap.statesync.trust_height must be positive')
    if not isinstance(sync['trust_hash'], str) or not re.fullmatch('[0-9a-fA-F]{64}', sync['trust_hash']):
        raise ValueError('bootstrap.statesync.trust_hash must be a 64-hex header hash')
    if not isinstance(sync['trust_period'], str) or not re.fullmatch(r'(?:[0-9]+(?:\.[0-9]+)?(?:h|m|s))+', sync['trust_period']) or not any(c in '123456789' for c in sync['trust_period']):
        raise ValueError('bootstrap.statesync.trust_period must be a positive duration')
    try:
        trusted = datetime.fromisoformat(sync['trusted_header_time'].replace('Z', '+00:00'))
        expires = datetime.fromisoformat(sync['expires_at'].replace('Z', '+00:00'))
        if trusted.utcoffset() is None or expires.utcoffset() is None or expires <= trusted:
            raise ValueError('invalid timestamp ordering')
    except (AttributeError, TypeError, ValueError) as exc:
        raise ValueError('bootstrap statesync needs ordered timezone-aware trusted_header_time/expires_at') from exc
    rpcs = sync['rpc_servers']
    if not isinstance(rpcs, list) or len(rpcs) < 2 or not all(https_url(url) for url in rpcs) or len(set(rpcs)) != len(rpcs):
        raise ValueError('bootstrap.statesync.rpc_servers needs at least two distinct HTTPS endpoints')
    peers = value['snapshot_peers']
    if not isinstance(peers, list) or not peers or not all(isinstance(p, str) and re.fullmatch(r'[0-9a-fA-F]{40}@[^\s:]+:[0-9]+', p) for p in peers):
        raise ValueError('bootstrap.snapshot_peers requires public node-id@host:port suppliers')
