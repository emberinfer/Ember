"""Retry and lifecycle logging policy for outbound sessions."""
from datetime import datetime
import random


class ReconnectBackoff:
    def __init__(self):
        self.ceiling = 1

    def next_delay(self, ready_seconds=0):
        if ready_seconds >= 60:
            self.ceiling = 1
        ceiling = self.ceiling
        self.ceiling = min(2 * ceiling, 60)
        # Equal jitter keeps each delay between half and all of its ceiling.
        return random.uniform(ceiling / 2, ceiling)


class DisconnectLog:
    def __init__(self):
        self.last_logged = None
        self.suppressed = 0

    def emit(self, message, now):
        if self.last_logged is not None and now - self.last_logged < 60:
            self.suppressed += 1
            return
        suffix = f' suppressed={self.suppressed}' if self.last_logged is not None else ''
        lifecycle_print(message + suffix)
        self.last_logged = now
        self.suppressed = 0


def lifecycle_print(message, *, file=None):
    timestamp = datetime.now().astimezone().isoformat(timespec='seconds')
    print(f'{timestamp} {message}', file=file, flush=True)
