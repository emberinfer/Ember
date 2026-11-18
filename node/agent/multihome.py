"""Process-wide task ownership and registration coordination.

Claims intentionally survive failures and reconnects. No eviction permits an old
request to execute twice; restart starts a new process ownership lifetime.
"""
import json
import threading


class Claims:
    def __init__(self):
        self.lock = threading.Lock()
        self.ids = set()

    def claim(self, kind, ident):
        with self.lock:
            key = kind, ident
            if key in self.ids:
                return False
            self.ids.add(key)
            return True


class Registration:
    def __init__(self):
        self.lock = threading.Lock()
        self.pending = None
        self.confirmed = False
        self.result = None

    def invalidate(self):
        with self.lock:
            self.confirmed = False

    def ensure(self, register, valid=None):
        while True:
            with self.lock:
                if self.confirmed and (valid is None or valid(self.result)):
                    return self.result
                leader = self.pending is None
                if leader:
                    self.pending = threading.Event()
                pending = self.pending
            if not leader:
                pending.wait()
                continue
            try:
                result = register()  # Network calls are outside the critical section.
            except BaseException:
                with self.lock:
                    self.pending = None
                    pending.set()
                raise
            with self.lock:
                self.confirmed = True
                self.result = result
                self.pending = None
                pending.set()
            return result


def coordinators(value, fingerprint=None):
    """Accept a legacy address, a list, or a JSON list from generated env config."""
    if isinstance(value, str):
        value = json.loads(value) if value.lstrip().startswith('[') else [value]
    if not isinstance(value, list) or not value:
        raise ValueError('coordinator must be an address or a nonempty list')
    result = []
    seen = set()
    for item in value:
        if isinstance(item, str):
            address, pin = item, fingerprint
        elif isinstance(item, dict):
            address, pin = item.get('endpoint'), item.get('tls_fingerprint')
        else:
            raise ValueError('invalid coordinator entry')
        if not isinstance(address, str) or not address or ':' not in address:
            raise ValueError('invalid coordinator endpoint')
        if pin is not None:
            if not isinstance(pin, str) or len(pin) != 64:
                raise ValueError('invalid coordinator TLS fingerprint')
            try:
                bytes.fromhex(pin)
            except ValueError:
                raise ValueError('invalid coordinator TLS fingerprint') from None
        if address in seen:
            raise ValueError('duplicate coordinator endpoint')
        seen.add(address)
        result.append((address, pin))
    return result


class Sessions:
    def __init__(self, peers):
        self.peers = peers
        self.connected = set()
        self.lock = threading.Lock()

    def update(self, peer, ready):
        with self.lock:
            if ready:
                self.connected.add(peer)
            else:
                self.connected.discard(peer)
            print(f'MULTIHOME connected={len(self.connected)}/{len(self.peers)} '
                  f'peer={peer} ready={ready} '
                  f'missing={sorted(set(self.peers) - self.connected)}', flush=True)
