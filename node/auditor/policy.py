"""Immutable numerical audit policy selected from a verified signed genesis."""
import hashlib
import math

from ember_core import verify, node_id
from ember_pb2 import ChainGenesis, ResourceProfile
from .numeric import UNION, RANK_PROB
from .gpu import GPU_PROFILES, FINGERPRINT, gpu_engine_id


NUMERIC_EXECUTION_PROFILES = frozenset((
    'transformers-qwen2.5-union-v1',
    'transformers-qwen2.5-cpu-strict-v1',
    'transformers-qwen2.5-cpu-preview-v1',
    'transformers-qwen2.5-cpu-v1',
)) | GPU_PROFILES


def engine_id(capabilities, runtime):
    # Versions matter: Transformers over different torch builds is a different
    # calibration pair even when both operators call their engine 'transformers'.
    return f'{capabilities.engine_name}/{capabilities.engine_version}|torch={runtime.get("torch_version", "unknown")}'


def profile_engine_id(profile):
    c = profile.engine_capabilities
    if profile.execution_profile_ver in GPU_PROFILES or profile.execution_profile_ver.startswith('transformers-qwen2.5-pipeline-gpu'):
        return gpu_engine_id(c, {'torch_version': profile.torch_version,
            'cuda_version': profile.cuda_version, 'actual_device': profile.actual_device,
            'accelerator_name': profile.accelerator_name})
    return engine_id(c, {'torch_version': profile.torch_version})


def validate_policy(policy, allow_pipeline=False):
    gpu = policy.metric == RANK_PROB
    if policy.metric not in (UNION, RANK_PROB) or policy.top_k != 16 or list(policy.layers) != ['0', '14', '27'] or len(policy.calibration_ref) != 32 or not policy.bands:
        raise ValueError('unknown/incomplete numeric policy')
    seen = set()
    for band in policy.bands:
        pair = (band.producer_engine, band.producer_device, band.reference_engine, band.reference_device)
        if not all(pair) or pair in seen or not math.isfinite(band.tolerance) or band.tolerance <= 0 or band.mode not in ('enforce', 'observe'):
            raise ValueError('invalid/duplicate tolerance band')
        if gpu and (band.producer_device != 'cuda' or band.reference_device != 'cuda'
                    or not FINGERPRINT.fullmatch(band.producer_engine)
                    or not FINGERPRINT.fullmatch(band.reference_engine)
                    or band.tolerance >= 1):
            raise ValueError('GPU bands require pinned CUDA/class fingerprints and tolerance < 1')
        pipeline_pair = (allow_pipeline and band.producer_engine.startswith('pipeline[')
                         and band.reference_engine.startswith('transformers/')
                         and band.producer_device == band.reference_device)
        if (band.mode == 'enforce' and
                (band.producer_engine != band.reference_engine or
                 band.producer_device != band.reference_device) and
                not pipeline_pair and not gpu):
            raise ValueError('uncalibrated cross-runtime slashing is forbidden in v1')
        seen.add(pair)


def select_band(policy, producer_engine, producer_device, reference):
    validate_policy(policy)
    try:
        reference_engine = (gpu_engine_id(reference.capabilities(), reference.resource_metadata())
                            if policy.metric == RANK_PROB else
                            engine_id(reference.capabilities(), reference.resource_metadata()))
    except ValueError:
        return None  # incompatible reference cannot authorize a numerical slash
    for band in policy.bands:
        if (band.producer_engine, band.producer_device, band.reference_engine, band.reference_device) == (
                producer_engine, producer_device, reference_engine, reference.device):
            return band
    return None


def load_job_policy(job, receipt):
    """No unsigned tolerance/device overrides for new profiles."""
    if not job.get('genesis'):
        return None, None, None
    raw = bytes.fromhex(job['genesis'])
    genesis = ChainGenesis.FromString(raw)
    verify(genesis)
    if not genesis.HasField('numeric_audit_policy'):
        if genesis.execution_profile_ver in NUMERIC_EXECUTION_PROFILES:
            raise ValueError('numeric execution profile requires signed policy')
        return None, None, None
    if genesis.chain_id == 'ember-qwen2.5-1.5b' or not genesis.execution_profile_ver:
        raise ValueError('legacy or empty profile cannot select a new numeric policy')
    validate_policy(genesis.numeric_audit_policy)
    if ((genesis.execution_profile_ver in GPU_PROFILES) !=
            (genesis.numeric_audit_policy.metric == RANK_PROB)):
        raise ValueError('execution profile/metric mismatch')
    profile = ResourceProfile.FromString(bytes.fromhex(job['producer_profile']))
    verify(profile)
    if (profile.node_id != receipt.node_id or profile.node_id != node_id(profile.signer)
            or profile.signer != receipt.signer or profile.chain_id != genesis.chain_id
            or profile.manifest_root != genesis.model_manifest_root
            or profile.execution_profile_ver != genesis.execution_profile_ver
            or not profile.HasField('engine_capabilities')):
        raise ValueError('producer/profile/receipt binding mismatch')
    c = profile.engine_capabilities
    if ('full' not in c.evidence_levels or c.score_kind != 'raw-logits'
            or c.device_class != profile.actual_device or c.max_top_k < 16 or not c.forced_prefix_replay):
        raise ValueError('producer lacks required full evidence')
    if genesis.execution_profile_ver in GPU_PROFILES:
        if profile.requested_device != 'cuda':
            raise ValueError('GPU profile requires explicit CUDA')
        profile_engine_id(profile)  # validate all signed runtime facts
    return genesis.numeric_audit_policy, profile, hashlib.sha256(raw).hexdigest()


def grade(verdict, distance, band):
    if verdict == 'reject':
        return verdict, 'invalid-commitment-or-evidence'
    if not math.isfinite(distance):
        return 'inconclusive', 'invalid-or-incompatible-reference'
    if band is None:
        return 'inconclusive', 'uncalibrated-pair'
    if band.mode == 'observe':
        return 'inconclusive', ('compatible-observation' if distance <= band.tolerance else 'outside-observation-band')
    return verdict, ('within-enforced-band' if distance <= band.tolerance else 'outside-enforced-band')
