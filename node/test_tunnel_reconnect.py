from contextlib import nullcontext
from datetime import datetime
import random
import re
import threading
from types import SimpleNamespace

from agent import Agent
from agent.multihome import Sessions
from agent.reconnect import DisconnectLog, ReconnectBackoff
from ember_pb2 import ResourceProfile, TunnelFrame


def lifecycle_messages(output):
    messages = []
    for line in output.splitlines():
        timestamp, message = line.split(' ', 1)
        assert re.fullmatch(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[+-]\d{2}:\d{2}', timestamp)
        local_time = datetime.fromisoformat(timestamp)
        assert local_time.utcoffset() == local_time.astimezone().utcoffset()
        messages.append(message)
    return messages


def test_backoff_progression_with_equal_jitter(monkeypatch):
    sample = random.Random(42).uniform
    bounds = []

    def uniform(low, high):
        bounds.append((low, high))
        return sample(low, high)

    monkeypatch.setattr('agent.reconnect.random.uniform', uniform)
    backoff = ReconnectBackoff()
    ceilings = [1, 2, 4, 8, 16, 32, 60, 60, 60]
    delays = [backoff.next_delay() for _ in ceilings]
    assert bounds == [(ceiling / 2, ceiling) for ceiling in ceilings]
    assert all(low <= delay <= high for delay, (low, high) in zip(delays, bounds))


def test_disconnect_suppression_is_per_peer_and_reports_counts(capsys):
    first, second = DisconnectLog(), DisconnectLog()
    first.emit('TUNNEL disconnected peer=first reconnecting: refused', 0)
    first.emit('TUNNEL disconnected peer=first reconnecting: refused', 1)
    second.emit('TUNNEL disconnected peer=second reconnecting: refused', 1)
    first.emit('TUNNEL disconnected peer=first reconnecting: refused', 59.999)
    first.emit('TUNNEL disconnected peer=first reconnecting: timed out', 60)
    first.emit('TUNNEL disconnected peer=first reconnecting: refused', 120)
    assert lifecycle_messages(capsys.readouterr().out) == [
        'TUNNEL disconnected peer=first reconnecting: refused',
        'TUNNEL disconnected peer=second reconnecting: refused',
        'TUNNEL disconnected peer=first reconnecting: timed out suppressed=2',
        'TUNNEL disconnected peer=first reconnecting: refused suppressed=0',
    ]


def test_tunnel_backoff_resets_only_after_stable_ready(monkeypatch, capsys):
    now = 0
    waits = []
    ready_durations = iter([None, None, 59.999, None, 60, None])

    def connect(messages):
        nonlocal now
        duration = next(ready_durations)
        assert next(messages).kind == 'hello'
        if duration is not None:
            now += 60
            yield TunnelFrame(kind='ready')
            now += 30
            assert next(messages).kind == 'heartbeat'
            now += duration - 30
            if duration == 60:
                return
        raise ConnectionRefusedError('coordinator unavailable')

    def wait(delay):
        nonlocal now
        waits.append(delay)
        now += delay
        return len(waits) == 6

    monkeypatch.setattr('agent.time', SimpleNamespace(monotonic=lambda: now))
    monkeypatch.setattr('agent.reconnect.random.uniform', lambda low, high: high)
    monkeypatch.setattr('agent.NodeTunnelStub', lambda channel: SimpleNamespace(Connect=connect))
    agent = Agent('test-node', None)
    agent.profile = ResourceProfile
    agent.serve('test-peer', stop=SimpleNamespace(is_set=lambda: False, wait=wait),
                channel_factory=nullcontext)

    assert waits == [1, 2, 4, 8, 1, 2]
    messages = lifecycle_messages(capsys.readouterr().out)
    assert [line for line in messages if line.startswith('JOIN READY')] == [
        'JOIN READY node=test-node peer=test-peer failed_attempts=2',
        'JOIN READY node=test-node peer=test-peer failed_attempts=1',
    ]
    assert messages.count('HEARTBEAT node=test-node peer=test-peer') == 2
    assert 'TUNNEL disconnected peer=test-peer reconnecting: stream ended suppressed=1' in messages


def test_first_disconnect_is_logged_if_stop_is_set(capsys):
    stop = threading.Event()

    def refused(address):
        stop.set()
        raise ConnectionRefusedError(f'{address} refused')

    Agent('test-node', None).serve('test-peer', stop=stop, channel_factory=refused)
    assert lifecycle_messages(capsys.readouterr().out) == [
        'TUNNEL disconnected peer=test-peer reconnecting: test-peer refused',
    ]


def test_multihome_logs_only_state_changes_and_keeps_aggregate_readiness(capsys):
    sessions = Sessions(['dead', 'live'])
    sessions.update('dead', False)
    sessions.update('dead', False)
    sessions.update('live', True)
    sessions.update('dead', False)
    assert sessions.is_ready()
    sessions.update('live', False)
    assert not sessions.is_ready()
    assert lifecycle_messages(capsys.readouterr().out) == [
        "MULTIHOME connected=0/2 peer=dead ready=False missing=['dead', 'live']",
        "MULTIHOME connected=1/2 peer=live ready=True missing=['dead']",
        "MULTIHOME connected=0/2 peer=live ready=False missing=['dead', 'live']",
    ]
