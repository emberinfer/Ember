from ..spi import EngineAdapter, EngineCapabilities, Step
from ..device import available_bytes, select_device, memory_budget, check_memory


class TransformersEngine(EngineAdapter):
    """Single-request inference; replay uses the committed prefix, never free generation."""
    def __init__(self, path, device='auto', layers=(0, 14, 27), tamper=False):
        import torch
        import transformers
        from transformers import AutoTokenizer, AutoModelForCausalLM
        self.torch, self.layers, self.tamper = torch, tuple(layers), tamper
        self.requested_device = device
        self.device, self.device_reason = select_device(torch, device)
        self.memory_budget = memory_budget(torch, self.device)
        self.available_memory_bytes = available_bytes()
        self.torch_version = torch.__version__
        self.transformers_version = transformers.__version__
        self.cuda_version = torch.version.cuda or ''
        self.accelerator_name = ''
        if self.device == 'cuda':
            # Cap this process at the measured allocator and RSS budgets.
            total = torch.cuda.get_device_properties(0).total_memory
            torch.cuda.set_per_process_memory_fraction(min(1.0, self.memory_budget[1] / total), 0)
            self.accelerator_name = torch.cuda.get_device_name(0)
        self.tokenizer = AutoTokenizer.from_pretrained(path, local_files_only=True, trust_remote_code=False)
        self.model = AutoModelForCausalLM.from_pretrained(
            path, local_files_only=True, trust_remote_code=False,
            torch_dtype=torch.bfloat16,
            low_cpu_mem_usage=False).to(self.device).eval()
        check_memory(self.memory_budget)
        self.hooks = []
        self.hidden = {}
        for index in self.layers:
            if index >= len(self.model.model.layers):
                raise ValueError('sampled layer is not in model')
            self.hooks.append(self.model.model.layers[index].register_forward_hook(self._hook(index)))

    def count_input_tokens(self, prompt):
        return len(self.tokenizer(prompt).input_ids)

    def resource_metadata(self):
        return {
            'requested_device': self.requested_device,
            'actual_device': self.device,
            'device_reason': self.device_reason,
            'torch_version': self.torch_version,
            'transformers_version': self.transformers_version,
            'cuda_version': self.cuda_version,
            'accelerator_name': self.accelerator_name,
            'available_memory_bytes': self.available_memory_bytes,
            'memory_budget_bytes': dict(zip(('minimum_available', 'maximum_cuda', 'maximum_rss', 'reserve'), self.memory_budget)),
        }

    def _hook(self, index):
        def capture(_module, _input, output):
            tensor = output[0] if isinstance(output, tuple) else output
            self.hidden[str(index)] = tensor[0, -1].detach().float().cpu().numpy().tobytes()
        return capture

    def _step(self, ids):
        torch = self.torch
        self.hidden.clear()
        with torch.inference_mode():
            logits = self.model(input_ids=ids, use_cache=False).logits[0, -1].float()
            top = torch.topk(logits, 16)
        pairs = [(int(i), float(v)) for i, v in zip(top.indices.tolist(), top.values.tolist())]
        activations = dict(self.hidden)
        return pairs, activations

    def _generate(self, prompt, seed, count):
        torch = self.torch
        ids = self.tokenizer(prompt, return_tensors='pt').input_ids.to(self.device)
        for _ in range(count):
            top, activations = self._step(ids)
            check_memory(self.memory_budget)
            token = top[0][0]
            if self.tamper:
                top[0] = (top[0][0], top[0][1] + 10.0)
            text = '' if token == self.tokenizer.eos_token_id else self.tokenizer.decode([token])
            yield Step(token, text, top, activations)
            ids = torch.cat((ids, torch.tensor([[token]], device=self.device)), dim=1)
            if token == self.tokenizer.eos_token_id:
                break
            check_memory(self.memory_budget)

    def replay(self, prompt, seed, tokens):
        torch = self.torch
        ids = self.tokenizer(prompt, return_tensors='pt').input_ids.to(self.device)
        result = []
        for token in tokens:
            top, activations = self._step(ids)
            result.append(Step(token, self.tokenizer.decode([token]), top, activations))
            ids = torch.cat((ids, torch.tensor([[token]], device=self.device)), dim=1)
        return result

    def replay_boundaries(self, prompt, seed, tokens, boundaries):
        hooks = []
        try:
            for boundary in sorted(boundaries):
                if not 0 < boundary <= len(self.model.model.layers):
                    raise ValueError('invalid executed replay boundary')
                hooks.append(self.model.model.layers[boundary-1].register_forward_hook(
                    self._hook('boundary:'+str(boundary))))
            return self.replay(prompt,seed,tokens)
        finally:
            for hook in hooks:
                hook.remove()

    def capabilities(self):
        return EngineCapabilities('transformers', self.transformers_version,
                                  self.device, 'bfloat16', ('full',), 'raw-logits',
                                  16, True)

    def close(self):
        super().close()
        for hook in self.hooks:
            hook.remove()
        self.hooks.clear()
