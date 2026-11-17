"""Opt-in forwarding measurements. No token text or prompt is logged."""
from collections import deque
import sys
import time


class ForwardTrace:
    def __init__(self, request_id):
        self.request_id = request_id
        self.started = time.monotonic_ns()
        self.first = self.last = 0
        self.arrival = 0
        self.tokens = self.sent = self.work = self.queue = 0
        self.queued = deque()

    def engine_token(self):
        self.arrival = time.monotonic_ns()
        if not self.first:
            self.first = self.arrival
        self.last = self.arrival
        self.tokens += 1

    def enqueue(self):
        now = time.monotonic_ns()
        self.work += now - self.arrival
        self.queued.append(now)

    def dequeue(self):
        self.queue += time.monotonic_ns() - self.queued.popleft()
        self.sent += 1

    def finish(self, kind):
        end = time.monotonic_ns()
        divisor = max(1, self.tokens) * 1e6
        cadence = (self.last - self.first) / (max(1, self.tokens - 1) * 1e6)
        print(f'FORWARD_TRACE hop=agent request={self.request_id} tokens={self.tokens} '
              f'sent={self.sent} started_ns={self.started} first_ns={self.first} '
              f'last_ns={self.last} end_ns={end} elapsed_ms={(end-self.started)/1e6:.3f} '
              f'engine_ms_per_token={cadence:.3f} agent_ms_per_token={self.work/divisor:.3f} '
              f'queue_ms_per_token={self.queue/divisor:.3f} terminal={kind}',
              file=sys.stderr, flush=True)
