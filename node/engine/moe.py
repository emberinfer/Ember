"""MoE segment engines: backbone attention stages and expert units.

The attention stage owns the router: per layer it produces A_att (the
post-attention-norm activations), the routing decision (top-k experts and
weights per position), fans the routed rows out to expert units through an
injected caller, and combines the results. Expert units are pure FFNs.
Numerics replicate transformers' OlmoeDecoderLayer / OlmoeSparseMoeBlock
exactly (QK-norm on the full projection, RoPE, softmax routing, no top-k
renormalization unless the config says so), in the manifest's segment
dtype.
"""
import json
import math
import threading
from pathlib import Path

import torch
import torch.nn.functional as F
from safetensors import safe_open

from ember_core import moe as moe_core
from engine.fp8 import expert_ffn_fp8


class MoeWeights:
    """Lazy safetensors access over a Hugging Face model directory."""

    def __init__(self, model_dir):
        self.dir = Path(model_dir)
        self.config = json.loads((self.dir / 'config.json').read_text())
        self._readers = {}
        self._index = {}
        for path in sorted(self.dir.glob('*.safetensors')):
            reader = safe_open(str(path), framework='pt', device='cpu')
            self._readers[path.name] = reader
            for key in reader.keys():
                self._index[key] = reader

    def get(self, key):
        return self._index[key].get_tensor(key)

    def has(self, key):
        return key in self._index

    def keys_with_prefix(self, prefix):
        return sorted(k for k in self._index if k.startswith(prefix))

    def parts(self, keys):
        parts = []
        for key in sorted(keys):
            tensor = self.get(key).contiguous()
            if tensor.dtype == torch.bfloat16:
                raw = tensor.view(torch.uint16).numpy().tobytes()
            elif tensor.dtype == torch.float8_e4m3fn:
                # numpy has no float8 dtype; hash the raw code bytes.
                # chain-genesis/build_moe.py uses the identical rule.
                raw = tensor.view(torch.uint8).numpy().tobytes()
            else:
                raw = tensor.numpy().tobytes()
            parts.append((key, tuple(tensor.shape), str(tensor.dtype), raw))
        return parts

    def params_hash(self, keys):
        return moe_core.params_hash(self.parts(keys))


def stage_tensor_keys(weights, layer):
    return moe_core.stage_tensor_keys(weights.has, layer)


def _rms_norm(x, weight, eps):
    variance = x.float().pow(2).mean(-1, keepdim=True)
    return (x.float() * torch.rsqrt(variance + eps)).to(x.dtype) * weight


class ExpertUnit:
    """One (layer, expert) FFN: down(silu(gate(x)) * up(x))."""

    def __init__(self, weights: MoeWeights, layer, expert, device='cpu',
                 dtype=torch.bfloat16):
        self.layer, self.expert = layer, expert
        self.device, self.dtype = device, dtype
        prefix = f'model.layers.{layer}.mlp.experts.{expert}.'
        self.keys = [prefix + f'{proj}.weight' for proj in ('gate_proj', 'up_proj', 'down_proj')]
        self.params_hash = weights.params_hash(self.keys)
        self.gate = weights.get(self.keys[0]).to(device=device, dtype=dtype)
        self.up = weights.get(self.keys[1]).to(device=device, dtype=dtype)
        self.down = weights.get(self.keys[2]).to(device=device, dtype=dtype)

    def forward(self, rows):
        """rows: (n, hidden) in engine dtype -> (n, hidden)."""
        with torch.no_grad():
            hidden = F.silu(F.linear(rows, self.gate)) * F.linear(rows, self.up)
            return F.linear(hidden, self.down)


class Fp8ExpertUnit:
    """One (layer, expert) FFN in native block-FP8 arithmetic.

    For segment_dtype "fp8-e4m3-block128" profiles, both serving and
    audit replay run in the production caliber itself (engine/fp8.py):
    native e4m3 weights with their 128x128 block scales and dynamically
    group-quantized activations — never a bf16 dequantization, whose
    replay distance to production measured 2.7e-2..4.2e-2 in MOE-0.
    The params hash covers the weight tensors AND their block scales
    (the scales are weight material; chain-genesis/build_moe.py hashes
    exactly the same key set).
    """

    def __init__(self, weights: MoeWeights, layer, expert, device='cpu'):
        self.layer, self.expert = layer, expert
        self.device = device
        prefix = None
        for candidate in (f'model.layers.{layer}.mlp.experts.{expert}.',
                          f'model.language_model.layers.{layer}.mlp.experts.{expert}.'):
            if weights.has(candidate + 'gate_proj.weight'):
                prefix = candidate
                break
        if prefix is None:
            raise KeyError(f'no FP8 expert tensors for layer {layer} expert {expert}')
        self.keys = []
        self.projs = {}
        for proj in ('gate_proj', 'up_proj', 'down_proj'):
            wkey = prefix + f'{proj}.weight'
            skey = prefix + f'{proj}.weight_scale_inv'
            self.keys += [wkey, skey]
            self.projs[proj] = (weights.get(wkey).to(device),
                                weights.get(skey).to(device))
        self.params_hash = weights.params_hash(self.keys)

    def forward(self, rows):
        """rows: (n, hidden) activations -> (n, hidden) in rows' dtype."""
        with torch.no_grad():
            out = expert_ffn_fp8(rows, self.projs['gate_proj'],
                                 self.projs['up_proj'], self.projs['down_proj'])
            return out.to(rows.dtype)


def make_expert_unit(weights, layout, layer, expert, device='cpu'):
    """Expert unit in the profile's segment caliber: FP8-native for
    fp8-e4m3-block128 profiles, bf16 otherwise."""
    if (layout.segment_dtype or '').startswith('fp8'):
        return Fp8ExpertUnit(weights, layer, expert, device=device)
    return ExpertUnit(weights, layer, expert, device=device)


class AttentionStage:
    """Backbone stage over layers [start, end) with per-request KV state.

    expert_call(layer, expert, rows) -> rows is injected by the service
    layer; it may be a local ExpertUnit or a remote RPC client.
    """

    def __init__(self, weights: MoeWeights, layout, start, end, device='cpu',
                 dtype=torch.bfloat16, expert_call=None):
        self.weights = weights
        self.layout = layout
        self.start, self.end = start, end
        self.device, self.dtype = device, dtype
        self.expert_call = expert_call
        cfg = weights.config
        self.hidden = cfg['hidden_size']
        self.heads = cfg['num_attention_heads']
        self.kv_heads = cfg.get('num_key_value_heads', self.heads)
        self.head_dim = self.hidden // self.heads
        self.experts = cfg['num_experts']
        self.top_k = cfg['num_experts_per_tok']
        self.norm_topk = bool(cfg.get('norm_topk_prob', False))
        self.eps = cfg.get('rms_norm_eps', 1e-5)
        self.rope_theta = cfg.get('rope_theta', 10000.0)
        self.has_embedding = start == 0
        self.has_head = end == layout.num_layers
        self._load()
        self._kv = {}  # request_id -> {layer: (k, v)}, plus 'pos'
        self._kv_lock = threading.RLock()

    def _load(self):
        w, dev, dt = self.weights, self.device, self.dtype
        self.layers = []
        for layer in range(self.start, self.end):
            p = f'model.layers.{layer}.'
            entry = {
                'in_norm': w.get(p + 'input_layernorm.weight').to(dev, dt),
                'q': w.get(p + 'self_attn.q_proj.weight').to(dev, dt),
                'k': w.get(p + 'self_attn.k_proj.weight').to(dev, dt),
                'v': w.get(p + 'self_attn.v_proj.weight').to(dev, dt),
                'o': w.get(p + 'self_attn.o_proj.weight').to(dev, dt),
                'q_norm': w.get(p + 'self_attn.q_norm.weight').to(dev, dt),
                'k_norm': w.get(p + 'self_attn.k_norm.weight').to(dev, dt),
                'post_norm': w.get(p + 'post_attention_layernorm.weight').to(dev, dt),
                'gate': w.get(p + 'mlp.gate.weight').to(dev, dt),
            }
            self.layers.append(entry)
        if self.has_embedding:
            self.embed = w.get('model.embed_tokens.weight').to(dev, dt)
        if self.has_head:
            self.final_norm = w.get('model.norm.weight').to(dev, dt)
            head_key = 'lm_head.weight' if w.has('lm_head.weight') else 'model.embed_tokens.weight'
            self.head = w.get(head_key).to(dev, dt)
        # params hash over exactly the stage's manifest tensor set
        keys = []
        for layer in range(self.start, self.end):
            keys.extend(stage_tensor_keys(w, layer))
        if self.has_embedding:
            keys.append('model.embed_tokens.weight')
        if self.has_head:
            keys.extend(['model.norm.weight', 'lm_head.weight'])
        self.params_hash = w.params_hash(sorted(set(k for k in keys if w.has(k))))

    def _rope(self, positions):
        inv_freq = 1.0 / (self.rope_theta ** (
            torch.arange(0, self.head_dim, 2, device=self.device).float() / self.head_dim))
        freqs = positions.float()[:, None] * inv_freq[None, :]
        emb = torch.cat((freqs, freqs), dim=-1)
        return emb.cos(), emb.sin()

    @staticmethod
    def _rotate_half(x):
        half = x.shape[-1] // 2
        return torch.cat((-x[..., half:], x[..., :half]), dim=-1)

    def _attention(self, entry, x, request_id, layer):
        """x: (S, hidden) normalized input rows for this frame."""
        S = x.shape[0]
        state = self._kv.setdefault(request_id, {})
        pos0 = state.get('pos', 0)
        positions = torch.arange(pos0, pos0 + S, device=self.device)
        q = _rms_norm(F.linear(x, entry['q']), entry['q_norm'], self.eps)
        k = _rms_norm(F.linear(x, entry['k']), entry['k_norm'], self.eps)
        v = F.linear(x, entry['v'])
        q = q.view(S, self.heads, self.head_dim).transpose(0, 1)
        k = k.view(S, self.kv_heads, self.head_dim).transpose(0, 1)
        v = v.view(S, self.kv_heads, self.head_dim).transpose(0, 1)
        cos, sin = self._rope(positions)
        cos = cos.to(q.dtype)[None, :, :]
        sin = sin.to(q.dtype)[None, :, :]
        q = q * cos + self._rotate_half(q) * sin
        k = k * cos + self._rotate_half(k) * sin
        if layer in state:
            pk, pv = state[layer]
            k_all = torch.cat([pk, k], dim=1)
            v_all = torch.cat([pv, v], dim=1)
        else:
            k_all, v_all = k, v
        state[layer] = (k_all, v_all)
        if layer == self.end - 1:
            state['pos'] = pos0 + S
        L = k_all.shape[1]
        if self.kv_heads != self.heads:
            repeat = self.heads // self.kv_heads
            k_all = k_all.repeat_interleave(repeat, dim=0)
            v_all = v_all.repeat_interleave(repeat, dim=0)
        scores = (q.float() @ k_all.float().transpose(1, 2)) / math.sqrt(self.head_dim)
        if L > 1:
            mask = torch.full((S, L), float('-inf'), device=self.device)
            for i in range(S):
                mask[i, :pos0 + i + 1] = 0.0
            scores = scores + mask
        probs = torch.softmax(scores, dim=-1)
        out = (probs @ v_all.float()).to(self.dtype)
        out = out.transpose(0, 1).reshape(S, self.hidden)
        return F.linear(out, entry['o'])

    def forward_frame(self, request_id, frame, input_ids=None, hidden=None,
                      frame_context=None):
        # Protect KV advancement even for direct engine callers. On failure,
        # discard partial updates; tensors are replaced, never mutated in place.
        with self._kv_lock:
            previous = dict(self._kv.get(request_id, {}))
            try:
                return self._forward_frame(request_id, frame, input_ids, hidden,
                                           frame_context)
            except BaseException:
                if previous:
                    self._kv[request_id] = previous
                else:
                    self._kv.pop(request_id, None)
                raise

    def _forward_frame(self, request_id, frame, input_ids=None, hidden=None,
                       frame_context=None):
        """Process one frame. Returns (out_rows, records, a_att_map, logits,
        input_rows).

        records: list of dicts {layer, experts (positions*k ids), positions}
        — hashes are computed by the service layer from a_att_map. logits:
        (vocab,) for the last row when this stage has the head, else None.
        input_rows is the frame's input boundary (post-embedding for the
        first stage), for receipt hashing by the service layer.
        """
        if self.expert_call is None:
            raise RuntimeError('attention stage needs an expert caller')
        with torch.no_grad():
            if self.has_embedding and input_ids is not None:
                ids = torch.tensor(input_ids, dtype=torch.long, device=self.device)
                x = self.embed[ids]
            else:
                x = hidden.to(device=self.device, dtype=self.dtype)
            input_rows = x
            S = x.shape[0]
            a_att_map = {}
            records = []
            for offset, entry in enumerate(self.layers):
                layer = self.start + offset
                h = x + self._attention(entry, _rms_norm(x, entry['in_norm'], self.eps),
                                        request_id, layer)
                a_att = _rms_norm(h, entry['post_norm'], self.eps)
                a_att_map[layer] = a_att
                logits_r = F.linear(a_att.float(), entry['gate'].float())
                probs = torch.softmax(logits_r, dim=-1)
                top_w, top_i = probs.topk(self.top_k, dim=-1)
                if self.norm_topk:
                    top_w = top_w / top_w.sum(dim=-1, keepdim=True)
                # Exposed for the service layer: expert receipts bind the
                # routing record of the layer currently fanning out.
                routing = {
                    'layer': layer,
                    'experts': top_i.flatten().tolist(),
                    'positions': S,
                }
                if frame_context is not None:
                    frame_context.routing = routing
                    frame_context.a_att = a_att_map
                combined = torch.zeros_like(a_att)
                # Construct and consume in serial expert/row order. Only the
                # independent RPC work between these barriers may overlap.
                jobs = []
                for expert in sorted(set(top_i.flatten().tolist())):
                    mask = top_i == expert
                    rows_idx = mask.any(dim=-1).nonzero(as_tuple=True)[0]
                    jobs.append((expert, rows_idx, a_att[rows_idx], mask))
                batch = getattr(self, 'expert_calls', None)
                if batch is not None and frame_context is not None:
                    outputs = batch(frame_context, layer,
                                    [(e, rows) for e, _, rows, _ in jobs])
                else:
                    outputs = [(self.expert_call(frame_context, layer, e, rows)
                                if frame_context is not None else
                                self.expert_call(layer, e, rows))
                               for e, _, rows, _ in jobs]
                for (_, rows_idx, _, mask), out_rows in zip(jobs, outputs):
                    slot_weights = (top_w * mask).sum(dim=-1)[rows_idx]
                    combined[rows_idx] += out_rows * slot_weights.to(out_rows.dtype)[:, None]
                records.append({
                    'layer': layer,
                    'experts': top_i.flatten().tolist(),
                    'positions': S,
                })
                x = h + combined
            logits = None
            if self.has_head:
                last = _rms_norm(x[-1:], self.final_norm, self.eps)
                logits = F.linear(last.float(), self.head.float())[0]
            return x, records, a_att_map, logits, input_rows

    def drop_request(self, request_id):
        with self._kv_lock:
            self._kv.pop(request_id, None)
