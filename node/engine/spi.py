"""Engine SPI v1. Evidence values always carry their numerical meaning."""
from dataclasses import dataclass, asdict
import math
import time


class UnsupportedCapability(ValueError):
    pass


class EngineProtocolError(RuntimeError):
    pass


class EngineUnavailable(RuntimeError):
    pass


class EngineTimeout(TimeoutError):
    pass


@dataclass
class Step:
    token: int
    text: str
    topk: list[tuple[int, float]]
    activations: dict[str, bytes]
    score_kind: str = 'raw-logits'


@dataclass(frozen=True)
class EngineCapabilities:
    engine_name: str
    engine_version: str
    device_class: str
    numerical_format: str
    evidence_levels: tuple[str, ...]
    score_kind: str
    max_top_k: int
    forced_prefix_replay: bool
    native_incremental: bool = True
    roles: tuple[str, ...] = ('model',)
    spi_version: str = 'ember-engine-spi-v1'

    def to_proto(self):
        from ember_pb2 import EngineCapabilities as Message
        return Message(**asdict(self))


@dataclass(frozen=True)
class SamplingParams:
    seed: int = 0
    max_tokens: int = 8
    temperature: float = 0
    top_k: int = 16
    timeout_seconds: float = 300

    def validate(self, caps):
        if type(self.seed) is not int or not -(2**63) <= self.seed < 2**63:
            raise ValueError('seed must be signed 64-bit')
        if type(self.max_tokens) is not int or not 1 <= self.max_tokens <= 65536:
            raise ValueError('max_tokens out of range')
        if self.temperature != 0:
            raise UnsupportedCapability('SPI v1 supports greedy sampling only')
        if type(self.top_k) is not int or not 1 <= self.top_k <= caps.max_top_k:
            raise UnsupportedCapability('requested evidence top_k unavailable')
        if not math.isfinite(self.timeout_seconds) or self.timeout_seconds <= 0:
            raise ValueError('timeout_seconds must be finite and positive')


@dataclass
class DecodeSession:
    owner: object
    iterator: object
    deadline: float
    closed: bool = False
    count: int = 0
    limit: int = 0

    def close(self):
        if not self.closed:
            self.closed = True
            close = getattr(self.iterator, 'close', None)
            if close:
                close()


class EngineAdapter:
    """Single-consumer lifecycle shared by adapters; backend internals stay here."""
    generation_timeout_seconds = 300

    def capabilities(self):
        raise NotImplementedError

    def resource_metadata(self):
        return {'actual_device': self.device}

    def count_input_tokens(self, prompt):
        raise EngineUnavailable("adapter does not expose exact prompt tokenization")

    def prefill(self, prompt, sampling):
        sampling.validate(self.capabilities())
        if not isinstance(prompt, str) or not prompt:
            raise ValueError('prompt must be nonempty text')
        active = getattr(self, '_session', None)
        if active is not None and not active.closed:
            raise EngineUnavailable('adapter already has an active session')
        session = DecodeSession(self, iter(self._generate_request(prompt, sampling)),
                                time.monotonic() + sampling.timeout_seconds,
                                limit=sampling.max_tokens)
        self._session = session
        return session

    def _generate_request(self, prompt, sampling):
        # Legacy full-evidence profiles always produce their pinned k, even when
        # a caller requests fewer candidates; no committed bytes are rewritten.
        return self._generate(prompt, sampling.seed, sampling.max_tokens)

    def decode(self, session):
        if session.owner is not self:
            raise ValueError('session belongs to another adapter')
        if session.closed:
            return None
        if time.monotonic() >= session.deadline:
            session.close()
            raise EngineTimeout('engine deadline expired')
        try:
            step = next(session.iterator)
            if time.monotonic() >= session.deadline:
                raise EngineTimeout('engine deadline expired')
            session.count += 1
            if session.count > session.limit:
                raise EngineProtocolError('server exceeded token limit')
            return step
        except StopIteration:
            session.close()
            return None
        except Exception:
            session.close()
            raise

    def generate(self, prompt, seed, count):
        session = self.prefill(prompt, SamplingParams(seed=seed, max_tokens=count,
                              top_k=min(16, self.capabilities().max_top_k),
                              timeout_seconds=self.generation_timeout_seconds))
        try:
            while (step := self.decode(session)) is not None:
                yield step
        finally:
            session.close()

    def generate_for_request(self, prompt, seed, count, request_id):
        return self.generate(prompt, seed, count)

    def replay(self, prompt, seed, tokens):
        raise UnsupportedCapability('forced-prefix replay is unavailable')

    def close(self):
        session = getattr(self, '_session', None)
        if session is not None:
            session.close()


def require_full_evidence(adapter):
    caps = adapter.capabilities()
    if ('full' not in caps.evidence_levels or caps.score_kind != 'raw-logits'
            or caps.max_top_k < 16 or not caps.forced_prefix_replay):
        raise UnsupportedCapability('profile requires raw top-16 logits, layer openings and replay')
