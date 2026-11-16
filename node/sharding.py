"""Canonical execution order for layer, embedding and head stages."""
def stage_span(start, end, total, embedding, head):
    if (total < 1 or not 0 <= start <= end <= total
            or (embedding and start != 0) or (head and end != total)
            or (start == end and not ((start == 0 and embedding and not head)
                                     or (start == total and head and not embedding)))):
        raise ValueError('invalid stage unit boundaries')
    return (0 if embedding else start + 1, total + 2 if head else end + 1)


def profile_span(profile):
    return stage_span(profile.shard_start_layer, profile.shard_end_layer,
                      profile.shard_total_layers, profile.shard_has_embedding,
                      profile.shard_has_lm_head)


def activation_hash(manifest, layer, raw, shape):
    import hashlib
    import struct
    return hashlib.sha256(b'ember/activation-boundary/v1\0' + manifest +
        struct.pack('>II', layer, len(shape)) +
        b''.join(struct.pack('>I', n) for n in shape) + raw).digest()


def range_parameters(root, start, end):
    import hashlib, struct
    return hashlib.sha256(b'ember/capability-range/v1\0' + root + struct.pack('>II', start, end)).digest()


def execution_span(request, profile):
    if profile.shard_dynamic_range:
        a, b = request.executed_start_layer, request.executed_end_layer
        if not request.dynamic_range or not profile.shard_start_layer <= a < b <= profile.shard_end_layer:
            raise ValueError('execution exceeds dynamic capability')
        return stage_span(a, b, profile.shard_total_layers,
                          a == 0 and profile.shard_has_embedding,
                          b == profile.shard_total_layers and profile.shard_has_lm_head)
    if request.dynamic_range:
        raise ValueError('dynamic request for pinned stage')
    return profile_span(profile)
