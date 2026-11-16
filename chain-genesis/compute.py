"""Per-token dense Qwen2 compute estimates, in integer FLOPs."""


def compute_table(config):
    if config.get('model_type') != 'qwen2':
        raise ValueError('compute table supports dense Qwen2 only')
    h = int(config['hidden_size'])
    intermediate = int(config['intermediate_size'])
    heads = int(config['num_attention_heads'])
    kv_heads = int(config.get('num_key_value_heads', heads))
    layers = int(config['num_hidden_layers'])
    vocab = int(config['vocab_size'])
    if min(h, intermediate, heads, kv_heads, layers, vocab) <= 0 or h % heads:
        raise ValueError('invalid dense Qwen2 dimensions')
    kv = h // heads * kv_heads
    # FLOPs/token ~= 2 * parameters: Q/K/V/O projections, gated MLP,
    # QKV biases and two RMS norms. Reference sequence length is 1;
    # sequence-dependent QK/AV attention products are omitted, not a runtime
    # performance claim. MoE expert branches are deliberately unsupported.
    parameters = 2*h*h + 2*h*kv + 3*h*intermediate + h + 2*kv + 2*h
    # Embedding is a lookup (~0 FLOPs). Consensus applies a one-unit floor
    # to the embedding atom (also when bundled), so a pure lookup is payable.
    # Tied output weights still require the full vocabulary projection.
    return [2 * parameters] * layers, 0, 2 * (h*vocab + h)


def set_compute_table(manifest, config):
    layers, embedding, head = compute_table(config)
    manifest.layer_flops.extend(layers)
    manifest.embedding_flops = embedding
    manifest.head_flops = head
