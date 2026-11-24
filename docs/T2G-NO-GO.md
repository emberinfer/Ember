# T2G — NO-GO: The frozen numeric specification is wrong and is coupled to the on-chain initial commitment

2026-11-23 UTC; 684 CPU, torch 2.5.1+cu124 (CUDA hidden), diffusers 0.32.2. Independent directory `/root/autodl-tmp/t2i/work2`, created from a local shared clone with an independent Git index and branch, then the HEAD of `train/2026-11-23-t2i` was verified by fetching from the real origin as `5e37ebfe51cccaf096353eec582932b90b2d070d`. `/root/autodl-tmp/t2i/work` was not modified; its branch was not reset.

## Conclusion first / NO-GO

The adapter faithfully implemented a wrong frozen specification; this is not merely an implementation error in some adapter operator. Specification §2.4 multiplies the Gaussian initial latent by the VAE scaling constant 0.18215; §5 additionally mandates a linear beta, integer floor endpoint timesteps, offset 0, DDIM final alpha=1, and disables Euler's initial sigma scaling. All of these disagree with the reference diffusers configuration of the pinned SD1.5 assets. The reference DDIM with the same init also degenerates by itself, so scheduler alignment cannot be declared to have solved image quality.

**Stop trigger: the specification layer is coupled to the chain.** `core-go/t2i.go:T2IInitLatent` multiplies each Gaussian by the bit pattern 0x3e3a8588 (0.18215); `services/chain/image.go:submitImage` rebuilds the initial tensor hash from that function, forces leaf 0's input/output to both match, and forces adjacency continuity afterwards. `core-go/image_profile.go:ValidateImageProfile` allows only `diffusers-sd1.5-fp32-v1`. Changing the initial tensor directly to standard normal, or multiplying by the Euler sigma, would break the current chain-side initial anchor; silently reusing that profile identifier would change the frozen numeric semantics. Per the task book, the fix and subsequent acceptance are stopped, the diagnostic materials are retained, and the matter awaits a supervisor ruling.

It is not claimed here that changing the chain is the only technical path: keeping a raw sentinel and pre-scaling in the first transition might also work, but that requires explicitly defining the migration semantics for generation, single-step audit, the evidence-opening boundary, profile versioning, the specification, and the old calibration materials; it must not be chosen unilaterally and disguised as an ordinary adapter bugfix.

## Measurements, strictly in order

1. `/root/workspace/ember-supervisor/t2i-divergence-test-2026-11-22/runs/` does not exist on this machine; per the task book, review of old artifacts was skipped. No old self-comparison was used as a substitute for reference ground truth.
2. First run a genuine `StableDiffusionPipeline.from_pretrained`, handing the assets' scheduler configuration to `DDIMScheduler.from_config`; fp32, CPU, prompt 1, empty negative, 20 steps, guidance 7.5, 512×512. The adapter's seed-42 Philox init was injected via `latents=`; the standard reference scheduler and the pipeline's encoding/CFG/UNet/decoding path were used — no Formula implementation replaced the reference scheduler. reference/000.npy is the input; callback_on_step_end saved 001..020; the final image is final.png.
3. Only after the reference completed was the unmodified adapter loaded, saving adapter/000..020.npy and final.png under the same configuration. Each Python process used torch 4 threads, OMP_NUM_THREADS=4, nice 15, and an empty CUDA_VISIBLE_DEVICES. No GPU was run; the 27B was not touched.
4. The comparison and intermediate quantities are in `comparison.json`, `first-step.json`, and the corresponding .npy files; the diffusers scheduler source was dumped method by method, and the configurations are in `reference-schedulers.json`. The diagnostic scripts `diagnose.py` and `first_step.py` are both in `/root/autodl-tmp/t2i/t2g-logs/`.

## First inconsistency and formula check

- The DDIM init is element-for-element identical on both sides; the first completed transition deviates at step 1, with relative L2=0.054130137337861234 (denominator: the reference norm, float64 reduction) — no longer rounding noise.
- The two sides already differ before the first UNet call: the reference leading timesteps are `[951,901,...,51,1]` (1000//20=50, steps_offset=1); the adapter's are `[999,946,893,841,...,52,0]` (floor endpoints). The first step's differing eps prediction therefore cannot be attributed to cross-device error on the same input.
- The first element of beta is 0.00085 on both sides; they differ from the second element on. The reference uses `linspace(sqrt(beta_start),sqrt(beta_end),1000,float32)**2` (scaled_linear); the adapter linearly interpolates beta directly; the cumulative alphas therefore deviate systematically, affecting every step's x0 and update.
- The reference DDIM alpha_prev is indexed by `t - 1000//steps`; at the end, set_alpha_to_one=false, so it uses alpha[0]; the adapter uses 1 at the end. eta=0, so no per-step noise is injected.
- The reference Euler timesteps are a floating-point linspace, e.g. 946.4210815, 893.8421021; sigma is linearly interpolated over the training sigma array, with first sigma=14.61464691 and a final appended 0. pipeline.prepare_latents also multiplies latents passed in by init_noise_sigma; the adapter does not multiply, and its sigmas also come from the wrong linear beta and integer timesteps. The two cannot be treated directly as the same ODE grid.
- The CFG semantics on both sides are `eps_u + 7.5*(eps_c-eps_u)`, and prediction_type is epsilon on both; there is no evidence that the CFG composition direction or a v_prediction branch is the primary cause. The reference batches CFG; the adapter calls the UNet separately; possible batch-shape rounding differences cannot explain the large deviation caused by different timesteps/alphas.
- The init's standard deviation is 0.18363014 and its mean is 0.0005467611; "injecting the same init to isolate the scheduler" must be distinguished from "verifying correct generation initialization". 0.18215 is the divisor for VAE latent decoding and cannot be used directly as a Gaussian standard deviation. The reference's final image with the low-amplitude init injected is also a brownish, low-detail image, with RGB mean [120.86707306,80.48270798,41.04592133] and RGB standard deviation [3.17717554,2.65671929,1.88625285], containing no recognizable astronaut/horse.

## First-step measured intermediates and final state

| Item | Reference DDIM | Original adapter |
|---|---:|---:|
| t | 951 | 999 |
| alpha_bar(t) | 0.008154998533427715 | 0.0015789621975272894 |
| alpha_bar(prev) | 0.014004888944327831 | 0.0029477758798748255 |
| sqrt((1-alpha)/alpha) | 11.0283352783 | 25.1461206803 |
| eps_uncond norm | 31.45288849 | 31.50308609 |
| eps_cond norm | 31.73301125 | 31.71580505 |
| CFG eps norm | 33.91660690 | 33.61856461 |
| x0 norm | 128.46350098 | 301.28839111 |

Per-element relative L2: eps_uncond=0.01503079, eps_cond=0.01922357, CFG eps=0.07364782, x0=1.38257332. The reference's independently rebuilt step 1 is at distance 0 from the pipeline's saved step 1; the full trajectory: step0=0, step1=0.054130137337861234, step20=2.1988239733011787. All 21 points are in comparison.json; no favorable steps were cherry-picked.

## Scope not executed and matters for ruling

The frozen specification, adapter, chain code, profile, T0 vectors, and calibration band were not modified; no fix is claimed complete. Because of the NO-GO, no regression tests based on unapproved semantics were added, the full Python/Go acceptance was not run, no post-fix 6-image/CLIP overview was generated, no bjb1 GPU/CPU calibration band or fp16 spot-check was run, and Base=7.7563e-05 was not changed. The two current diagnostic images (reference/adapter) must not be passed off as a six-image quality acceptance.

Matters requiring a supervisor ruling: the migration strategy between a new execution profile and the old frozen semantics; whether to change the chain-side initial anchor or keep a raw sentinel and define first-step pre-scaling; permission to correct the specification's init/beta/DDIM/Euler configuration, and confirmation of the scope of subsequent regression and calibration re-acceptance. None of the above will be pushed forward without a ruling.

## Initialization causality control (diagnostic, not post-fix acceptance)

After completing "reference → original adapter → per-step comparison", the same reference DDIM, the same prompt and parameters were used, with the single change `latents = frozen_init / float32(0.18215)`. This keeps the Philox samples and seed while undoing the specification-imposed VAE initial scaling; repository source was not modified. `reference/unit-noise-control.png` clearly shows an astronaut riding a horse on a Martian surface; direct observation against the brownish flat image from the low-amplitude init supports wrong initialization amplitude as the key image-quality root cause. Because this experiment also retains the standard reference scheduler, it does not claim that changing only the adapter init completes full reference alignment, nor that this single control image satisfies the six-image acceptance.

The three original diagnostic images, `diagnostic-overview.png`, and the per-image pixel statistics `pixel-statistics.json` are all in t2g-logs; the overview explicitly labels the original adapter, reference + frozen init, and reference + unit-noise control, and does not pass the control image off as fixed-adapter output.

| Diagnostic image | RGB mean | RGB std | Pixel range |
|---|---|---|---|
| adapter/final.png | [128.684258, 40.177773, 0.0] | [5.476801, 1.97759, 0.0] | 0..135 |
| reference/final.png | [120.867073, 80.482708, 41.045921] | [3.177176, 2.656719, 1.886253] | 16..127 |
| reference/unit-noise-control.png | [152.611343, 102.979519, 68.594219] | [48.725408, 45.926308, 41.619197] | 0..255 |

## Evidence retention and delivery

Both diagnostic processes exited 0, with no residual GPU work; `unchanged-source-sha256.json` records 7 unmodified source files, verified file by file against 5e37ebf. The run evidence is in `/root/autodl-tmp/t2i/t2g-logs/`; source/PNG/latents/configurations/comparison results are all retained; it contains no private keys or access credentials. The repository gains only this diagnostic report, `docs/T2G-NO-GO.md`; after one commit and one push, the commit hash and ls-remote verification result are to be appended in the external report.
