"""SD1.5 components only; generation and replay share the frozen T0 step.

No diffusers pipeline/scheduler execution, RNG, truncation or implicit scaling.
Image operations deliberately have a separate interface from text token streams.
"""
import hashlib
import json
from pathlib import Path
import struct

from ember_core.t2i import f32, sqrt32, init_latent, serialize_latent, encode_prompts

PROFILE = 'diffusers-sd1.5-fp32-v1'
ASSETS = Path(__file__).resolve().parents[1] / 'assets/sd15-fp32-v1.json'
RESOLUTIONS = ((512, 512), (512, 768), (768, 512))


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def validate_request(r):
    for key in ('steps', 'guidance', 'resolution', 'seed'):
        if type(r[key]) is not int:
            raise ValueError('integer request field required: ' + key)
    if not (1 <= r['steps'] <= 100 and 1000 <= r['guidance'] <= 30000
            and 0 <= r['resolution'] < 3 and 0 <= r['seed'] < 2**64
            and r['sampler'] in ('ddim', 'euler')):
        raise ValueError('request outside frozen execution profile')
    for k in ('prompt', 'negative_prompt'):
        if len(r[k].encode('utf-8')) > 65536:
            raise ValueError('prompt too long')


def divide(x, scalar):
    # A full device tensor prevents CUDA's CPU-scalar reciprocal-multiply
    # optimization: T0 requires one rounded division, not rounded reciprocal
    # followed by multiplication. Do not replace this with x / Python_scalar.
    return x / x.new_full(x.shape, scalar)


class Formula:
    """Eager fp32 tensor operations: each operator materializes, no FMA/JIT.

    Scalars use the T0 finite sqrt and exact f32 operation boundaries.
    """
    def __init__(self, steps, sampler, guidance):
        if type(steps) is not int or not 1 <= steps <= 100 or sampler not in ('ddim', 'euler'):
            raise ValueError('unsupported scheduler')
        if type(guidance) is not int or not 1000 <= guidance <= 30000:
            raise ValueError('unsupported guidance')
        self.sampler, self.g = sampler, f32(f32(guidance) / f32(1000))
        self.ts = [999] if steps == 1 else [(steps-1-i)*999//(steps-1) for i in range(steps)]
        a, accum = [], 1.0
        b0, b1 = struct.unpack('<ff', bytes.fromhex('89d25e3aa69b443c'))
        for j in range(1000):
            beta = f32(b0 + f32(f32(b1-b0) * f32(f32(j)/f32(999))))
            accum = f32(accum * f32(1-beta))
            a.append(accum)
        self.a = [a[t] for t in self.ts] + [1.0]
        self.s = [sqrt32(f32(f32(1-v)/v)) for v in self.a]

    def model_input(self, x, i):
        if self.sampler == 'ddim':
            return x
        s = self.s[i]
        return divide(x, sqrt32(f32(f32(s*s)+1)))

    def update(self, x, i, uncond, cond):
        eps = uncond + self.g * (cond - uncond)
        if self.sampler == 'ddim':
            a, b = self.a[i:i+2]
            pred = divide(x - sqrt32(f32(1-a)) * eps, sqrt32(a))
            return sqrt32(b) * pred + sqrt32(f32(1-b)) * eps
        s, sn = self.s[i:i+2]
        pred = x - s * eps
        d = divide(x - pred, s)
        return x + d * f32(sn-s)


class DiffusersT2I:
    execution_profile = PROFILE
    resolutions = RESOLUTIONS

    def __init__(self, model_path, device='cpu', resolutions=RESOLUTIONS):
        from importlib.metadata import version
        for package, pinned in {'diffusers': '0.32.2', 'transformers': '4.46.3',
                                'ftfy': '6.3.1', 'regex': '2026.11.9'}.items():
            if version(package) != pinned:
                raise RuntimeError('execution profile library version mismatch: ' + package)
        import torch
        from diffusers import UNet2DConditionModel, AutoencoderKL
        from transformers import CLIPTextModel, CLIPTokenizer
        self.root, self.device, self.torch = Path(model_path), device, torch
        self.ready = False
        self.resolutions = tuple(tuple(r) for r in resolutions)
        if not self.resolutions or len(set(self.resolutions)) != len(self.resolutions) or any(r not in RESOLUTIONS for r in self.resolutions):
            raise ValueError('invalid published resolutions')
        self.assets = json.loads(ASSETS.read_text())
        self.verify_assets()
        torch.backends.cuda.matmul.allow_tf32 = False
        torch.backends.cudnn.allow_tf32 = False
        torch.backends.cudnn.benchmark = False
        torch.use_deterministic_algorithms(True)
        torch.set_flush_denormal(False)
        self.tokenizer = CLIPTokenizer.from_pretrained(self.root/'tokenizer', local_files_only=True)
        if (self.tokenizer.bos_token_id, self.tokenizer.eos_token_id, self.tokenizer.pad_token_id) != (49406, 49407, 49407):
            raise ValueError('tokenizer special IDs differ from T0')
        self.encoder = CLIPTextModel.from_pretrained(self.root/'text_encoder', local_files_only=True,
                                                    use_safetensors=True, torch_dtype=torch.float32).to(device).eval()
        self.unet = UNet2DConditionModel.from_pretrained(self.root/'unet', local_files_only=True,
                                                       use_safetensors=True, torch_dtype=torch.float32).to(device).eval()
        self.vae = AutoencoderKL.from_pretrained(self.root/'vae', local_files_only=True,
                                                use_safetensors=True, torch_dtype=torch.float32).to(device).eval()
        # Bound peak CPU attention memory without changing scheduler semantics.
        self.vae.enable_slicing()
        self.unet.set_attention_slice('auto')

    def capabilities(self):
        from ..spi import EngineCapabilities
        return EngineCapabilities(engine_name='diffusers-t2i', engine_version=PROFILE,
            device_class=self.device, numerical_format='f32', evidence_levels=('image-latent',),
            score_kind='latent', max_top_k=0, forced_prefix_replay=False,
            native_incremental=False, roles=('model', 'auditor'))

    def resource_metadata(self):
        return dict(actual_device=self.device, requested_device=self.device,
                    torch_version=self.torch.__version__,
                    cuda_version=self.torch.version.cuda or '')

    def verify_assets(self):
        expected = self.assets['files']
        actual_tokenizer = {str(p.relative_to(self.root)) for p in (self.root/'tokenizer').iterdir() if p.is_file()}
        if actual_tokenizer != {n for n in expected if n.startswith('tokenizer/')}:
            raise ValueError('unexpected tokenizer asset set')
        for name, digest in expected.items():
            with (self.root/name).open('rb') as f:
                if hashlib.file_digest(f, 'sha256').hexdigest() != digest:
                    raise ValueError('pinned asset mismatch: ' + name)

    def tokens(self, request):
        return encode_prompts(request['prompt'], request['negative_prompt'],
                              lambda s: self.tokenizer(s, add_special_tokens=False,
                                  padding=False, truncation=False)['input_ids'])

    def embeddings(self, request):
        positive, negative = self.tokens(request)
        for key, ids in (('prompt_tokens', positive), ('negative_tokens', negative)):
            if key in request and request[key] != ids:
                raise ValueError('binding failure: request tokenizer mismatch')
        with self.torch.inference_mode(), self.torch.autocast(device_type=self.device.split(':')[0], enabled=False):
            cond = self.encoder(self.torch.tensor([positive], device=self.device))[0]
            uncond = self.encoder(self.torch.tensor([negative], device=self.device))[0]
        return uncond, cond

    def initial(self, request):
        h, w = self.resolutions[request['resolution']]
        shape = (1, 4, h//8, w//8)
        return self.torch.tensor(init_latent(request['seed'], shape), dtype=self.torch.float32,
                                 device=self.device).reshape(shape)

    def step(self, x, formula, index, embeddings):
        with self.torch.inference_mode(), self.torch.autocast(device_type=self.device.split(':')[0], enabled=False):
            model_input = formula.model_input(x, index)
            u, c = [self.unet(model_input, formula.ts[index], encoder_hidden_states=e).sample
                    for e in embeddings]
            result = formula.update(x, index, u, c)
        if result.dtype != self.torch.float32 or not self.torch.isfinite(result).all():
            raise ValueError('invalid execution tensor')
        return result

    def decode(self, x):
        import numpy as np
        with self.torch.inference_mode(), self.torch.autocast(device_type=self.device.split(':')[0], enabled=False):
            decoded = self.vae.decode(divide(x, f32(0.18215))).sample
            pixels = ((decoded / 2 + 0.5).clamp(0, 1).permute(0, 2, 3, 1)
                      .cpu().numpy()[0] * np.float32(255)).round().astype('uint8')
        return pixels

    def raw(self, x):
        return serialize_latent(x.detach().cpu().reshape(-1).tolist(), tuple(x.shape))

    def load(self, raw, shape):
        import numpy as np
        if raw[:38] != serialize_latent([0.0] * __import__('math').prod(shape), shape)[:38]:
            raise ValueError('binding failure: tensor format')
        if len(raw) != 38 + __import__('math').prod(shape)*4:
            raise ValueError('binding failure: tensor length')
        a = np.frombuffer(raw[38:], dtype='<f4').copy().reshape(shape)
        if not np.isfinite(a).all():
            raise ValueError('binding failure: nonfinite tensor')
        return self.torch.from_numpy(a).to(self.device)

    def trajectory(self, request):
        validate_request(request)
        embeddings = self.embeddings(request)
        formula = Formula(request['steps'], request['sampler'], request['guidance'])
        x = self.initial(request)
        yield x
        for i in range(request['steps']):
            x = self.step(x, formula, i, embeddings)
            yield x

    def self_test(self, calibration, vectors=None):
        """Mandatory before admission; reference files and tolerances come from signed profile.

        calibration: request, latent_hashes, reference_paths, max_distances.
        Caller must authenticate the profile containing these values.
        """
        self.ready = False
        self.verify_assets()
        vectors = vectors or Path(__file__).resolve().parents[3]/'testdata/vectors/t2i-v1.json'
        for v in json.loads(Path(vectors).read_text())['vectors']:
            if sha(serialize_latent(init_latent(int(v['seed']), v['shape']), v['shape'])) != v['sha256']:
                raise ValueError('T0 conformance failed')
        from image_client import relative_l2
        r = calibration['request']
        if len(calibration['reference_paths']) != r['steps']+1 or len(calibration['max_distances']) != r['steps']+1:
            raise ValueError('incomplete calibration')
        distances = []
        for i, x in enumerate(self.trajectory(r)):
            raw = Path(calibration['reference_paths'][i]).read_bytes()
            if sha(raw) != calibration['latent_hashes'][i]:
                raise ValueError('calibration reference hash mismatch')
            ref = self.load(raw, tuple(x.shape))
            d = relative_l2(x.cpu().numpy(), ref.cpu().numpy())
            limit = calibration['max_distances'][i]
            if not __import__('math').isfinite(limit) or limit < 0 or d > limit:
                raise ValueError('calibration replay outside profile band')
            if i == 0 and self.raw(x) != raw:
                raise ValueError('calibration init mismatch')
            distances.append(d)
        self.ready = True
        return distances
