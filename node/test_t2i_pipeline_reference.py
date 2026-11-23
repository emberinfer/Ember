"""Real pipeline regression; T2I_REFERENCE_MODEL=/path/to/pinned/sd15 pytest this file.

Default three-step runs exercise every transition and decode, on CPU. Set
T2I_REFERENCE_STEPS=20 for acceptance. No downloaded assets or mock UNet.
"""
import os
import numpy as np
import pytest

@pytest.mark.parametrize('sampler', ['ddim', 'euler'])
def test_real_pipeline_reference(sampler):
    root = os.environ.get('T2I_REFERENCE_MODEL')
    if not root:
        pytest.skip('set T2I_REFERENCE_MODEL to pinned SD1.5 assets')
    import torch
    from diffusers import StableDiffusionPipeline, DDIMScheduler, EulerDiscreteScheduler
    from engine.adapters.diffusers_t2i import DiffusersT2I
    torch.set_num_threads(4)
    steps = int(os.environ.get('T2I_REFERENCE_STEPS', '3'))
    r = dict(prompt='a photograph of an astronaut riding a horse on Mars, detailed',
             negative_prompt='', seed=42, sampler=sampler, steps=steps,
             guidance=7500, resolution=0)
    e = DiffusersT2I(root)
    trajectory = [x.detach().clone() for x in e.trajectory(r)]
    image = e.decode(trajectory[-1])
    # Reuse fixed components to avoid duplicating model memory, but execute the
    # actual pipeline text/CFG/UNet/decode and actual reference scheduler.
    cls = DDIMScheduler if sampler == 'ddim' else EulerDiscreteScheduler
    scheduler = cls.from_pretrained(root, subfolder='scheduler', local_files_only=True)
    p = StableDiffusionPipeline(vae=e.vae, text_encoder=e.encoder, tokenizer=e.tokenizer,
        unet=e.unet, scheduler=scheduler, safety_checker=None, feature_extractor=None,
        requires_safety_checker=False)
    distances = []
    def callback(pipe, i, t, kw):
        a, b = trajectory[i+1].double(), kw['latents'].double()
        distances.append(float(torch.linalg.vector_norm(a-b)/torch.linalg.vector_norm(b)))
        return kw
    with torch.inference_mode():
        result = p(r['prompt'], negative_prompt='', latents=trajectory[0].clone(),
            num_inference_steps=steps, guidance_scale=7.5, height=512, width=512,
            callback_on_step_end=callback)
    ref = np.asarray(result.images[0])
    mse = np.mean((image.astype('float64')-ref.astype('float64'))**2)
    psnr = float('inf') if mse == 0 else float(10*np.log10(255**2/mse))
    assert len(distances) == steps
    assert max(distances) <= 3e-4, distances
    assert psnr >= 40, psnr
    print(dict(sampler=sampler, steps=steps, relative_l2=distances, psnr=psnr))
