"""Engine SPI and backwards-compatible adapter imports."""
from .spi import (EngineAdapter, EngineCapabilities, SamplingParams, Step,
                  UnsupportedCapability, EngineProtocolError, EngineTimeout,
                  EngineUnavailable, require_full_evidence)
from .device import available_bytes, rss_bytes, select_device
from .adapters.mock import MockEngine
from .adapters.transformers import TransformersEngine
from .adapters.http import LlamaCppEngine, VLLMEngine


def load_engine(name, **config):
    if name == 'mock':
        return MockEngine()
    if name == 'transformers':
        return TransformersEngine(config['model_path'], config.get('device', 'auto'),
                                  tamper=config.get('tamper', False))
    if name == 'pipeline':
        from shard_rpc import PipelineEngine
        return PipelineEngine(config['model_path'], config['stages'], config['scheduler_key_file'],
                              manifest=config['manifest'])
    if name in ('llamacpp', 'vllm'):
        adapter = LlamaCppEngine if name == 'llamacpp' else VLLMEngine
        return adapter(config['endpoint'], config['model'],
                       api_key=config.get('api_key'),
                       server_version=config.get('server_version', 'unknown'))
    raise UnsupportedCapability('unknown engine: ' + name)
