"""Selective Qwen2 layer loading and deterministic pipeline-stage execution."""
from pathlib import Path


class Qwen2ShardEngine:
    def __init__(self, path, start_layer, end_layer, has_embedding=False,
                 has_lm_head=False, device='auto'):
        import torch
        from safetensors import safe_open
        from transformers import AutoConfig
        from transformers.models.qwen2.modeling_qwen2 import (
            Qwen2DecoderLayer, Qwen2RMSNorm, Qwen2RotaryEmbedding,
        )
        from . import available_bytes, rss_bytes, select_device
        from .device import memory_budget, check_memory

        self.torch = torch
        self.path = Path(path)
        self.config = AutoConfig.from_pretrained(path, local_files_only=True,
                                                 trust_remote_code=False)
        # AutoModel selects SDPA on supported PyTorch versions. Pin the same
        # implementation because eager and SDPA diverge enough in BF16 to
        # change top-k ordering and audit outcomes.
        self.config._attn_implementation = 'sdpa'
        total = self.config.num_hidden_layers
        if not 0 <= start_layer <= end_layer <= total or (start_layer == end_layer and not ((start_layer == 0 and has_embedding and not has_lm_head) or (start_layer == total and has_lm_head and not has_embedding))):
            raise ValueError('invalid shard layer range')
        if has_embedding and start_layer != 0:
            raise ValueError('only the first shard may own embeddings')
        if has_lm_head and end_layer != total:
            raise ValueError('only the last shard may own the LM head')
        self.start_layer, self.end_layer = start_layer, end_layer
        self.has_embedding, self.has_lm_head = has_embedding, has_lm_head
        self.device, self.device_reason = select_device(torch, device)
        self.requested_device = device
        self.available_memory_bytes = available_bytes()
        self.rss_bytes = rss_bytes
        self.memory_budget = memory_budget(torch, self.device)
        self.check_memory = check_memory
        self.dtype = torch.bfloat16
        self.loaded_keys = set()
        self.last_activations = {}

        files = sorted(self.path.glob('*.safetensors'))
        if not files:
            raise ValueError('safetensors weights missing')
        readers = [safe_open(str(file), framework='pt', device='cpu') for file in files]
        if self.device == 'cuda':
            total_memory = torch.cuda.get_device_properties(0).total_memory
            torch.cuda.set_per_process_memory_fraction(
                min(1.0, self.memory_budget[1] / total_memory), 0)
        key_reader = {key: reader for reader in readers for key in reader.keys()}

        def tensor(key):
            if key not in key_reader:
                raise ValueError(f'missing shard parameter {key}')
            self.loaded_keys.add(key)
            return key_reader[key].get_tensor(key)

        def load(module, prefix):
            module = module.to(device=self.device, dtype=self.dtype)
            state = {}
            for name in module.state_dict():
                state[name] = tensor(prefix + name)
            module.load_state_dict(state, strict=True)
            return module.eval()

        self.embedding = None
        if has_embedding:
            self.embedding = torch.nn.Embedding(self.config.vocab_size,
                                                self.config.hidden_size,
                                                self.config.pad_token_id)
            load(self.embedding, 'model.embed_tokens.')
        self.layers = torch.nn.ModuleList()
        for index in range(start_layer, end_layer):
            layer = Qwen2DecoderLayer(self.config, index)
            self.layers.append(load(layer, f'model.layers.{index}.'))
        self.rotary = Qwen2RotaryEmbedding(config=self.config).to(self.device)
        self.norm = None
        self.lm_head = None
        if has_lm_head:
            self.norm = load(Qwen2RMSNorm(self.config.hidden_size,
                                         eps=self.config.rms_norm_eps), 'model.norm.')
            self.lm_head = torch.nn.Linear(self.config.hidden_size,
                                           self.config.vocab_size, bias=False)
            # Qwen2.5 ties the output projection to the token embedding.
            self.lm_head.load_state_dict({'weight': tensor('model.embed_tokens.weight')})
            self.lm_head = self.lm_head.to(device=self.device, dtype=self.dtype).eval()
        self.check_memory(self.memory_budget, self.rss_bytes())

    def resource_metadata(self):
        import transformers
        accelerator = (self.torch.cuda.get_device_name(0)
                       if self.device == 'cuda' else '')
        return {'requested_device': self.requested_device,
                'actual_device': self.device,
                'device_reason': self.device_reason,
                'torch_version': self.torch.__version__,
                'transformers_version': transformers.__version__,
                'cuda_version': self.torch.version.cuda or '',
                'accelerator_name': accelerator,
                'available_memory_bytes': self.available_memory_bytes}

    def forward(self, input_ids=None, hidden_states=None, start_layer=None, end_layer=None,
                has_embedding=None, has_lm_head=None):
        torch = self.torch
        start = self.start_layer if start_layer is None else start_layer
        end = self.end_layer if end_layer is None else end_layer
        embedding = self.has_embedding if has_embedding is None else has_embedding
        head = self.has_lm_head if has_lm_head is None else has_lm_head
        if start < self.start_layer or end > self.end_layer or end < start or (embedding and not self.has_embedding) or (head and not self.has_lm_head):
            raise ValueError('execution exceeds loaded capability')
        if embedding:
            if input_ids is None or hidden_states is not None:
                raise ValueError('first shard requires input_ids')
            hidden_states = self.embedding(input_ids.to(self.device))
        elif hidden_states is None or input_ids is not None:
            raise ValueError('non-first shard requires hidden_states')
        else:
            hidden_states = hidden_states.to(self.device, dtype=self.dtype)

        length = hidden_states.shape[1]
        position_ids = torch.arange(length, device=self.device).unsqueeze(0)
        cache_position = torch.arange(length, device=self.device)
        position_embeddings = self.rotary(hidden_states, position_ids)
        with torch.inference_mode():
            self.last_activations = {}
            for global_index in range(start, end):
                layer = self.layers[global_index - self.start_layer]
                output = layer(
                    # Match Qwen2Model's no-padding SDPA path. Supplying an
                    # explicit 4-D mask selects a different BF16 kernel.
                    hidden_states, attention_mask=None,
                    position_ids=position_ids, use_cache=False,
                    cache_position=cache_position,
                    position_embeddings=position_embeddings,
                )
                hidden_states = output[0] if isinstance(output, tuple) else output
                if global_index in (0, 14, 27):
                    self.last_activations[str(global_index)] = (
                        hidden_states[0, -1].detach().float().cpu().numpy().tobytes())
            if head:
                result = self.lm_head(self.norm(hidden_states)).float()
                self.check_memory(self.memory_budget, self.rss_bytes())
                return result
        self.check_memory(self.memory_budget, self.rss_bytes())
        return hidden_states
