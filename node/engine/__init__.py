"""Engine SPI and backwards-compatible adapter imports."""
from .spi import (EngineAdapter, EngineCapabilities, SamplingParams, Step,
                  UnsupportedCapability, EngineProtocolError, EngineTimeout,
                  EngineUnavailable, require_full_evidence)
from .device import available_bytes, rss_bytes, select_device
from .adapters.mock import MockEngine
from .adapters.transformers import TransformersEngine
from .adapters.http import LlamaCppEngine, VLLMEngine


def load_engine(name, **config):
    if name == 'diffusers-t2i':
        from .adapters.diffusers_t2i import DiffusersT2I, RESOLUTIONS
        engine = DiffusersT2I(config['model_path'], config.get('device', 'cpu'),
                              config.get('resolutions', RESOLUTIONS))
        engine.self_test(config['calibration'])
        return engine
    if name == 'mock':
        return MockEngine()
    if name == 'transformers':
        return TransformersEngine(config['model_path'], config.get('device', 'auto'),
                                  layers=config.get('layers'), tamper=config.get('tamper', False))
    if name == 'pipeline':
        from shard_rpc import PipelineEngine
        return PipelineEngine(config['model_path'], config['stages'], config['scheduler_key_file'],
                              manifest=config['manifest'])
    if name in ('llamacpp', 'vllm'):
        adapter = LlamaCppEngine if name == 'llamacpp' else VLLMEngine
        extra = {}
        if name == 'vllm' and config.get('eos_token_ids'):
            extra['eos_token_ids'] = config['eos_token_ids']
        return adapter(config['endpoint'], config['model'],
                       api_key=config.get('api_key'),
                       server_version=config.get('server_version', 'unknown'), **extra)
    raise UnsupportedCapability('unknown engine: ' + name)
