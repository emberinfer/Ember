"""GPU runtime identities. Device names are classes, never host identities."""
import re

GPU_PROFILE = 'transformers-qwen2.5-gpu-v1'
SMOLLM_GPU_PROFILE = 'transformers-smollm2-gpu-v1'
GPU_PROFILES = frozenset((GPU_PROFILE, SMOLLM_GPU_PROFILE))
GPU_CLASSES = {
    'NVIDIA GB10': 'gb10',
    'NVIDIA Thor': 'thor',
    'Orin': 'orin',
    'NVIDIA Orin': 'orin',
}
FINGERPRINT = re.compile(
    r'transformers/[^|\s]+\|torch=[^|\s]+\|cuda=[0-9]+\.[0-9]+\|gpu=(gb10|thor|orin)')


def gpu_engine_id(capabilities, runtime):
    gpu_class = GPU_CLASSES.get(runtime.get('accelerator_name'))
    if (capabilities.engine_name != 'transformers' or capabilities.device_class != 'cuda'
            or capabilities.numerical_format != 'bfloat16'
            or runtime.get('actual_device') != 'cuda' or not gpu_class):
        raise ValueError('GPU profile requires a recognized CUDA BF16 Transformers runtime')
    result = (f'transformers/{capabilities.engine_version}'
              f'|torch={runtime.get("torch_version", "")}'
              f'|cuda={runtime.get("cuda_version", "")}|gpu={gpu_class}')
    if not FINGERPRINT.fullmatch(result):
        raise ValueError('incomplete GPU runtime fingerprint')
    return result
