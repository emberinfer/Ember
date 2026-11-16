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
        from tokenization import artifact_version
        self.tokenizer_path = path
        self._tokenizer_version = artifact_version(path)
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

    def measure_contribution(self, context_tokens):
        """Measure loaded tensor storage and a real maximum-context KV forward.

        CUDA peak includes allocator workspace; unsupported backends never invent
        measurements. The resulting context bound is enforced on every request.
        """
        if type(context_tokens) is not int or not 1 <= context_tokens <= self.model.config.max_position_embeddings:
            raise ValueError('invalid contribution context limit')
        torch = self.torch
        if self.device != 'cuda':
            return None
        torch.cuda.synchronize()
        torch.cuda.empty_cache()
        torch.cuda.reset_peak_memory_stats()
        weight_storage = {t.untyped_storage().data_ptr(): t.untyped_storage().nbytes()
                          for t in list(self.model.parameters()) + list(self.model.buffers())}
        weights = sum(weight_storage.values())
        ids = torch.zeros((1, context_tokens), dtype=torch.long, device=self.device)
        with torch.inference_mode():
            output = self.model(input_ids=ids, use_cache=True)
        torch.cuda.synchronize()
        cache = output.past_key_values
        if hasattr(cache, 'to_legacy_cache'):
            cache = cache.to_legacy_cache()
        storages = {t.untyped_storage().data_ptr(): t.untyped_storage().nbytes()
                    for layer in cache for t in layer if torch.is_tensor(t)}
        if not storages:
            raise RuntimeError('engine returned no measurable KV tensors')
        result = dict(weight_bytes=weights, kv_bytes=sum(storages.values()),
                      peak_bytes=max(torch.cuda.max_memory_allocated(),
                                     torch.cuda.max_memory_reserved()), context_tokens=context_tokens)
        del output, cache, ids
        torch.cuda.empty_cache()
        self.contribution_context_tokens = context_tokens
        return result

    def set_contribution_budget(self, budget):
        if self.device != 'cuda' or not budget:
            return
        self.contribution_budget_bytes = budget
        total = self.torch.cuda.get_device_properties(0).total_memory
        self.torch.cuda.set_per_process_memory_fraction(
            min(1.0, min(budget, self.memory_budget[1]) / total), 0)
        self.check_contribution_memory()

    def check_contribution_memory(self):
        budget = getattr(self, 'contribution_budget_bytes', 0)
        if budget and self.torch.cuda.memory_allocated() > budget:
            raise MemoryError('operator contribution memory budget exceeded')

    def check_contribution_context(self, prompt, output_tokens):
        limit = getattr(self, 'contribution_context_tokens', None)
        if limit is not None and self.count_input_tokens(prompt) + output_tokens > limit:
            raise ValueError('request exceeds measured contribution context limit')

    @property
    def tokenizer_version(self):
        from tokenization import artifact_version, TokenizerUnavailable
        if artifact_version(self.tokenizer_path) != self._tokenizer_version:
            raise TokenizerUnavailable('tokenizer artifacts changed since load')
        return self._tokenizer_version

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
        self.check_contribution_memory()
        torch = self.torch
        self.hidden.clear()
        with torch.inference_mode():
            logits = self.model(input_ids=ids, use_cache=False).logits[0, -1].float()
            top = torch.topk(logits, 16)
        self.check_contribution_memory()
        pairs = [(int(i), float(v)) for i, v in zip(top.indices.tolist(), top.values.tolist())]
        activations = dict(self.hidden)
        return pairs, activations

    def thinking_prompt(self, prompt, enabled):
        return self.tokenizer.apply_chat_template(
            [{'role': 'user', 'content': prompt}], tokenize=False,
            add_generation_prompt=True, enable_thinking=enabled)

    def _generate(self, prompt, seed, count):
        self.check_contribution_context(prompt, count)
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
        self.check_contribution_context(prompt, len(tokens))
        torch = self.torch
        ids = self.tokenizer(prompt, return_tensors='pt').input_ids.to(self.device)
        result = []
        for token in tokens:
            check_memory(self.memory_budget)
            top, activations = self._step(ids)
            check_memory(self.memory_budget)
            result.append(Step(token, self.tokenizer.decode([token]), top, activations))
            ids = torch.cat((ids, torch.tensor([[token]], device=self.device)), dim=1)
        return result

    def score_forced(self, prompt, seed, tokens):
        """Score the committed sequence in one batched causal forward pass.

        Logits at input position p predict token p+1, so a single forward
        over prompt+committed[:-1] yields the distribution for every
        committed position under its committed prefix. Mirrors _generate's
        stopping rule: the step whose greedy token is EOS is the last one.
        """
        self.check_contribution_context(prompt, len(tokens))
        torch = self.torch
        if not tokens:
            return []
        ids = self.tokenizer(prompt, return_tensors='pt').input_ids.to(self.device)
        committed = torch.tensor([list(tokens)], device=self.device)
        full = torch.cat((ids, committed), dim=1)
        self.hidden.clear()
        check_memory(self.memory_budget)
        with torch.inference_mode():
            logits = self.model(input_ids=full[:, :-1], use_cache=False).logits[0].float()
        check_memory(self.memory_budget)
        start = ids.shape[1] - 1
        result = []
        for index in range(len(tokens)):
            top = torch.topk(logits[start + index], 16)
            pairs = [(int(i), float(v)) for i, v in zip(top.indices.tolist(), top.values.tolist())]
            token = pairs[0][0]
            text = '' if token == self.tokenizer.eos_token_id else self.tokenizer.decode([token])
            result.append(Step(token, text, pairs, {}))
            if token == self.tokenizer.eos_token_id:
                break
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
