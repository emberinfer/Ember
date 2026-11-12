"""Self-contained synthetic signed profiles for tests; not network releases."""
import hashlib
import json
import tempfile
from functools import lru_cache
from pathlib import Path

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from google.protobuf.json_format import MessageToDict
from ember_core import digest, sign
from ember_pb2 import ChainGenesis, ModelManifest, NumericAuditPolicy


@lru_cache(maxsize=1)
def profiles():
    # Retain the owner so generated fixtures live until the interpreter exits.
    owner = tempfile.TemporaryDirectory(prefix='ember-test-profiles-')
    profiles.owner = owner
    root = Path(owner.name)
    cpu = 'transformers/4.45.2|torch=2.5.1'
    cuda = 'transformers/4.45.2|torch=2.5.1+cuda'
    policy = NumericAuditPolicy(metric='union-interval-v1', top_k=16,
        layers=['0', '14', '27'], calibration_ref=hashlib.sha256(b'synthetic test calibration').digest())
    policy.bands.add(producer_engine=cpu, producer_device='cpu', reference_engine=cpu,
                     reference_device='cpu', tolerance=.001, mode='enforce')
    policy.bands.add(producer_engine=cpu, producer_device='cpu', reference_engine=cuda,
                     reference_device='cuda', tolerance=.25, mode='observe')
    variants = [('', 'transformers-qwen2.5-v1'),
                ('-union-v1', 'transformers-qwen2.5-union-v1'),
                ('-cpu-strict-v1', 'transformers-qwen2.5-cpu-strict-v1'),
                ('-cpu-preview-v1', 'transformers-qwen2.5-cpu-preview-v1'),
                ('-cpu-v1', 'transformers-qwen2.5-cpu-v1'),
                ('-publisher-cpu-v1', 'publisher-cpu-v1'),
                ('-gpu-v3', 'transformers-qwen2.5-gpu-v3')]
    for suffix, execution in variants:
        ident = 'ember-qwen2.5-1.5b' + suffix
        output = root / ident
        output.mkdir()
        key = Ed25519PrivateKey.generate()
        manifest = ModelManifest(chain_id=ident, license='Apache-2.0')
        manifest.artifacts.add(name='config.json', sha256=hashlib.sha256(b'{}').digest(), size=2)
        sign(manifest, key)
        g = ChainGenesis(chain_id=ident, execution_profile_ver=execution,
                         model_manifest_root=digest(manifest))
        if suffix:
            g.numeric_audit_policy.CopyFrom(policy)
        if suffix == '-cpu-v1':
            p = g.numeric_audit_policy
            p.ClearField('bands')
            for runtime in [cpu, cpu + '+cpu']:
                p.bands.add(producer_engine=runtime, producer_device='cpu',
                            reference_engine=runtime, reference_device='cpu',
                            tolerance=.001, mode='enforce')
        if suffix == '-gpu-v3':
            p = g.numeric_audit_policy
            p.metric = 'rank-probability-v1'
            p.ClearField('bands')
            runtimes = {name: 'transformers/4.45.2|torch=2.5.1|cuda=99.9|gpu=' + name
                        for name in ['gb10', 'thor', 'orin']}
            for producer, pengine in runtimes.items():
                for reference, rengine in runtimes.items():
                    p.bands.add(producer_engine=pengine, producer_device='cuda',
                                reference_engine=rengine, reference_device='cuda',
                                tolerance=.001 if producer == reference else .5, mode='enforce')
            capture = b'{"synthetic": true}\n'
            (output / 'calibration.json').write_bytes(capture)
            p.calibration_ref = hashlib.sha256(capture).digest()
        sign(g, key)
        (output / 'manifest.pb').write_bytes(manifest.SerializeToString(deterministic=True))
        (output / 'genesis.pb').write_bytes(g.SerializeToString(deterministic=True))
        (output / 'genesis.json').write_text(json.dumps({
            'genesis': MessageToDict(g, preserving_proto_field_name=True),
            'manifest': MessageToDict(manifest, preserving_proto_field_name=True)}))
    return root
