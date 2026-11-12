"""Standalone Ember Engine SPI v1 authoring types; no Ember core imports."""
from dataclasses import dataclass


@dataclass
class Step:
    token: int
    text: str
    topk: list
    activations: dict
    score_kind: str = 'raw-logits'


@dataclass(frozen=True)
class EngineCapabilities:
    engine_name: str
    engine_version: str
    device_class: str
    numerical_format: str
    evidence_levels: tuple
    score_kind: str
    max_top_k: int
    forced_prefix_replay: bool
    native_incremental: bool = True
    roles: tuple = ('model',)
    spi_version: str = 'ember-engine-spi-v1'


class EngineAdapter:
    def capabilities(self):
        raise NotImplementedError

    def generate(self, prompt, seed, count):
        raise NotImplementedError

    def replay(self, prompt, seed, tokens):
        raise NotImplementedError

    def close(self):
        pass
