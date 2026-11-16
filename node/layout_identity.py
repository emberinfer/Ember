"""Scheme identity matches the chain's deterministic protobuf v1 definition."""
import hashlib
from ember_pb2 import ModelManifest

def scheme_hash(scheme):
    return hashlib.sha256(b'ember-layout-v1\0'+scheme.SerializeToString(deterministic=True)).digest()

def select_scheme(manifest, pinned):
    if not manifest.schemes:
        if pinned:raise ValueError('unknown scheme hash')
        return manifest
    matching=[s for s in manifest.schemes if scheme_hash(s)==pinned]
    if len(matching)!=1:raise ValueError('missing or unknown scheme hash')
    s=matching[0]
    result=ModelManifest();result.CopyFrom(manifest);result.ClearField('schemes');result.ClearField('shards')
    result.total_layers=s.total_layers;result.min_stage_layers=s.min_stage_layers
    result.dynamic_ranges=True;result.allowed_layer_cuts[:]=s.allowed_layer_cuts
    result.layer_flops[:]=s.layer_flops;result.embedding_flops=s.embedding_flops;result.head_flops=s.head_flops
    if s.HasField('moe_layout'):result.moe_layout.CopyFrom(s.moe_layout)
    return result
