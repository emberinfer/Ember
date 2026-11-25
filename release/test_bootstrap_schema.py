"""Unpublished anchors remain explicit; ready bootstrap cannot be partial."""
import copy
import json
from pathlib import Path
import pytest
from bootstrap_schema import validate_bootstrap


def pending():
    return dict(status='pending-publication',genesis_url=None,genesis_sha256=None,
        snapshot_peers=[],statesync=dict(trust_height=None,trust_hash=None,trust_period=None,
        trusted_header_time=None,expires_at=None,rpc_servers=[]))


def ready():
    return dict(status='ready',genesis_url='https://example.org/genesis.json',genesis_sha256='a'*64,
        snapshot_peers=['b'*40+'@peer.example.org:26656'],statesync=dict(trust_height=123,
        trust_hash='c'*64,trust_period='168h0m0s',trusted_header_time='2026-11-25T00:00:00Z',
        expires_at='2026-12-02T00:00:00Z',rpc_servers=['https://a.example.org','https://b.example.org']))


def test_pending_and_complete_schema():
    validate_bootstrap(pending());validate_bootstrap(ready())


def test_public_http_rpcs_keep_genesis_https_only():
    b = ready()
    b['statesync']['rpc_servers'] = ['http://relay.example.org:39657', 'http://relay2.example.org:39661']
    validate_bootstrap(b)
    b['genesis_url'] = 'http://example.org/genesis.json'
    with pytest.raises(ValueError):
        validate_bootstrap(b)


@pytest.mark.parametrize('url', ['ftp://relay.example.org', 'http://user@relay.example.org', 'http://relay.example.org/#fragment'])
def test_invalid_rpc_urls_rejected(url):
    b = ready()
    b['statesync']['rpc_servers'][0] = url
    with pytest.raises(ValueError):
        validate_bootstrap(b)


@pytest.mark.parametrize('mutation', [
    lambda b:b.pop('genesis_sha256'),
    lambda b:b.update(genesis_sha256='placeholder'),
    lambda b:b.update(genesis_url=None),
    lambda b:b.update(snapshot_peers=[]),
    lambda b:b['statesync'].update(trust_height=0),
    lambda b:b['statesync'].update(trust_hash=None),
    lambda b:b['statesync'].update(trust_period='0h'),
    lambda b:b['statesync'].update(expires_at='2026-11-24T00:00:00Z'),
    lambda b:b['statesync'].update(rpc_servers=['https://a.example.org']*2),
])
def test_partial_ready_rejected(mutation):
    b=ready();mutation(b)
    with pytest.raises(ValueError):validate_bootstrap(b)


def test_pending_must_not_imply_usable_anchor():
    b=pending();b['statesync']['trust_height']=123
    with pytest.raises(ValueError):validate_bootstrap(b)
