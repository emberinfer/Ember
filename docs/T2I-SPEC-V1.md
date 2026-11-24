# T2I Numeric Specification (T2H Errata, Execution Profile v2): Initial Anchor, Prompt, Latent and Deterministic Samplers

Status: **T0 specification, errata approved under T2H**, 2026-11-23. Specification ID
`philox-gauss-v1`; first-release profile `ember-stable-diffusion-1.5-v1`, execution profile
`diffusers-sd1.5-fp32-v2`.
Subsequent work packages must comply with this file; any change to bytes, rounding,
constants, sequences or formulas requires a new specification version, except for the
v2 errata approved by the supervisor in this round and the init-vector regeneration
exception; see the final section. This document defines reference semantics only; it
does not activate the protocol or change on-chain state.

Design basis: construction drawing DESIGN-t2i §1.3, §2.1–2.3, §3.2–3.3; the experiment
report only proves that same-precision cross-hardware single-step recomputation is
feasible. The experiment's torch RNG hashes are not part of this version's vectors; the
RangeBand measured in the experiment cannot substitute for a new profile's release
calibration. SD1.5 / fp32 / no content moderation are the settled line-wide positions.

## 1. Numeric Foundations and Profiles

- `F(x)` = IEEE 754 binary32, roundTiesToEven; rounding is applied immediately after
  each basic operation specified below. FMA, reassociation, fast-math, FTZ/DAZ, and
  keeping intermediate results in extended precision are not allowed. Integer-to-fp32
  conversion also uses roundTiesToEven. The operands of addition, subtraction,
  multiplication and division are all fp32; the reference implementation computes a
  single operation with sufficient precision and then immediately converges it to
  fp32. Negation flips the sign bit and introduces no new rounding.
- Below, `a ⊕ b = F(a+b)`, `a ⊖ b = F(a-b)`, `a ⊗ b = F(a*b)`,
  `a ⊘ b = F(a/b)`; parentheses around operations are a mandatory evaluation order —
  operations that look redundant must not be eliminated. Loops in this file must
  execute in ascending order; all accumulations are sequential accumulations.
- SD1.5: C=4, VAE spatial factor = 8; image height×width may only be 512×512, 512×768,
  768×512. The full latent shapes are `(1,4,64,64)`, `(1,4,64,96)`, `(1,4,96,64)`
  respectively. All shapes include batch=1; the batch dimension must not be omitted
  when hashing.
- Execution dtype = `f32`; this package's dual implementations and vectors certify
  only that dtype. If a different dtype is published later, it must have its own
  execution profile, dtype encoding and vectors; audit pools must not be mixed.
  Conversion points: see §2.4.
- seed is a uint64, unsigned decimal input, range `[0,18446744073709551615]`; signed
  overflow, taking absolute values, or accepting out-of-range input via modulo are
  forbidden. In vector JSON, seed is a decimal string, to avoid JavaScript's 53-bit
  integer precision loss.

Constants are defined by the following **fp32 bit patterns**, not by decimal parsers
or platform π:

| Name | Bit pattern (numeric uint32, not byte order) | Use |
|---|---|---|
| PI | `40490fdb` | π |
| TWO_PI | `40c90fdb` | 2π |
| HALF_PI | `3fc90fdb` | π/2 |
| LN2 | `3f317218` | ln 2 |
| INIT_SCALE | `3f800000` | F(1.0) |

## 2. `philox-gauss-v1`

### 2.1 Philox4x32-10

Run ten Philox rounds separately for each element index `i`. One counter is not
shared across four elements, Gaussian pairs are not cached across elements, and
nothing depends on device, thread count, chunking, or how many elements were
generated before.

- The 128-bit counter is the zero-extension of the numeric value i:
  `(c0,c1,c2,c3)=(i low 32, i high 32, 0, 0)`; supported element indices are `< 2^64`.
  The 64-bit key is `(k0,k1)=(seed low 32, seed high 32)`.
- The unsigned word sequence is `c0,c1,c2,c3`; each word is encoded as 4 little-endian
  bytes; when taking a uint32 from the RNG byte stream it must be little-endian
  decoded — host-endian reinterpretation must not change the value.
- Multipliers `M0=0xd2511f53`, `M1=0xcd9e8d57`; Weyl increments
  `W0=0x9e3779b9`, `W1=0xbb67ae85`.
- Each round first computes two full uint64 products `p0=M0*c0`, `p1=M1*c2`, and
  updates simultaneously:
  `(c0,c1,c2,c3)=(hi32(p1) XOR c1 XOR k0, lo32(p1),
  hi32(p0) XOR c3 XOR k1, lo32(p0))`; then the key adds W0/W1 respectively, modulo
  `2^32`. Exactly 10 rounds; the first round uses the original key; the tenth round's
  counter is returned (the final key increment is not used again).
- Known answer: for seed=0, i=0, the four output words are
  `6627e8d5 e169c58d bc57ac4c 9b00dbd8` (Random123 Philox4x32-10 KAT).

### 2.2 Uniforms and the Box–Muller Polar Transform

For the first two words x0, x1 after this element's ten rounds, compute
`u_j = F(F(x_j >> 8) * 2^-24)`, so `u_j ∈ [0,1)`. Discard the last two words.
After the right shift the integer has at most 24 bits; the conversion and the
multiplication by a power of two are both exact.

Box–Muller's polar form is `r=sqrt(-2 ln(1-u0))`, `θ=2πu1`, `z=r cos θ`.
The radius/angle form is explicitly used here, not the Marsaglia variant that needs
rejection sampling; each element has exactly one counter, two uniforms, one output.
The second Gaussian `r sin θ` is not output and not cached.
Using `1-u0` (range `[2^-24,1]`) avoids `log(0)`; it must not be replaced by u0, an
epsilon clamp, `nextafter`, or `(x+0.5)/2^32`.

**Per-operation definition**: `v = 1 ⊖ u0`; `l = log32(v)`; `a = (-2) ⊗ l`;
`r = sqrt32(a)`; `θ = TWO_PI ⊗ u1`; `z = r ⊗ cos32(θ)`.
Even if `l=+0` produces `a=-0`, sqrt32 returns +0. Other zeros preserve their IEEE
sign bit.

### 2.3 Platform-Independent log32 / sqrt32 / cos32

These names refer specifically to the finite algorithms in this section; they are not
aliases for the system libm, Go math, Python math, NumPy, or GPU built-in functions.
Pinning the order of basic operations is not enough to pin library functions, so the
approximation algorithms must also be fixed. Substituting other approximations or
correctly rounded transcendental functions in the future requires a new specification
ID. Only the call domains below are required; no general mathematical-function
interface for arbitrary negatives, NaN, or infinities is provided.

`log32(x)`, input is a positive normal fp32 from §2.2:

1. `b=bits32(x)`, integer `e=(b >> 23)-127`;
   `m=frombits32((b AND 0x007fffff) OR 0x3f800000)`, so `1≤m<2`.
2. `z=(m ⊖ 1) ⊘ (m ⊕ 1)`; `z2=z ⊗ z`; `p=z`, `s=z`.
3. For integer k=1..12: first `p=p ⊗ z2`, then `s=s ⊕ (p ⊘ F(2k+1))`.
4. Return `(F(e) ⊗ LN2) ⊕ (2 ⊗ s)`.

`sqrt32(x)`, input is a non-negative finite fp32 from §2.2 (including -0):

1. If x equals zero, return bit pattern `00000000`.
2. `y=frombits32((bits32(x) >> 1)+0x1fc00000)`, where the addition is integer addition.
3. Exactly 8 times (no early-convergence exit): `y=0.5 ⊗ (y ⊕ (x ⊘ y))`; return y.

`cos32(θ)`, θ is an fp32 from §2.2, range `[0,TWO_PI)`:

1. `x=θ`. If `x>PI`, set `x=x ⊖ TWO_PI`. Initial `sign=1`.
2. If `x>HALF_PI`, simultaneously set `x=PI ⊖ x`, `sign=-1`; else if `x<-HALF_PI`,
   simultaneously set `x=(-PI) ⊖ x`, `sign=-1`. Comparisons are strict greater/less.
3. `x2=x ⊗ x`; `term=1`, `total=1`.
4. For k=1..12: `term=((-term) ⊗ x2) ⊘ F((2k-1)(2k))`;
   `total=total ⊕ term`. The denominator is computed by integer multiplication first,
   then converted to fp32.
5. Return `sign ⊗ total`.

### 2.4 Layout, Scaling, dtype Conversion Points

Row-major NCHW, the rightmost w is contiguous. `i=(((n*C+c)*h+y)*w+x)`, n is always 0.
Generate fp32 z in ascending i from zero; the initial latent element is
`init_i=z ⊗ INIT_SCALE`.
**Complete the scaling in fp32 first, and only then convert to the execution
profile's dtype**; the current f32 conversion is the identity.
Converting z to lower precision first is forbidden, as is substituting a torch/numpy
RNG. The chain's leaf 0 snapshot is the standard normal init (×1.0). DDIM uses it
directly; Euler multiplies it by `init_noise_sigma=σ_0` inside the first transition,
before the UNet, and the first-step audit must perform the same preparation. The
sigma-scaled tensor must not be passed off as the chain's leaf 0; later transitions
do not multiply by sigma again. 0.18215 is only a VAE boundary constant.

## 3. Prompt Encoding

- Tokenizer assets (vocab, merges, special_tokens_map, tokenizer_config, and every
  file read at runtime, plus their configuration) are pinned by the profile's file
  SHA-256 manifest. Automatic online updating, automatic replacement by a same-named
  tokenizer, or a library default choosing another normalization / tokenizer version
  are forbidden.
- Input must be valid UTF-8. It is passed to the content-tokenization path of the
  profile-pinned CLIP tokenizer, with `add_special_tokens`, `padding`, and
  `truncation` off; pre-truncating characters or tokens is forbidden. Returned
  content token IDs must be in 0..49405 (inclusive). This package's interface
  receives this content-tokenization callback; it does not download or fabricate
  another BPE vocabulary. Pinning and loading the real tokenizer's assets is the
  responsibility of the profile/client package.
- SD1.5: sequence length 77, BOS=49406 (`<|startoftext|>`), EOS=49407
  (`<|endoftext|>`), **pad token ID = 49407**. Not 0, and not another pad ID from a
  text model's config. Output `[BOS] + content_tokens + [EOS] + [PAD]*remaining length`.
- Count the full content token count K first. If K+2>77, reject the whole request;
  there is no truncation mode. K=75 exactly fills 77 and is acceptable; K=76 must be
  rejected; K=0 outputs BOS followed by 76 EOS/PAD.
- The positive and negative prompts must call the same function, the same tokenizer
  and configuration. An empty negative is `""`, tokenized as usual, with BOS/EOS
  added, padded, and run through the negative text encoder; the negative forward pass
  must not be omitted or replaced by a zero embedding. The token IDs produced by both
  paths must be included in the first-step anchor's upstream input.
- CLIP encodes all 77 positions, using the SD1.5 CLIP native causal attention mask;
  no additional padding attention mask, no clip-skip, and no pooled embedding in
  place of the last layer's per-token hidden states. Positive and negative each run
  one forward pass and do not share an empty-prompt special case.

## 4. Canonical Latent Serialization and Hashing

**Reuse the existing audit `ETV1` / `raw-le-v1`** (`node/auditor/spot_binding.py`);
do not invent another container. The whole hash input is the header immediately
followed by the payload, with no separator, no JSON, no length prefix.

| Offset (bytes) | Length | Content |
|---|---|---|
| 0 | 4 | ASCII `ETV1`, hex `45545631` |
| 4 | 1 | dtype tag, f32=1 (existing audit f16=4, bf16=3; this version allows only f32) |
| 5 | 1 | rank=4 |
| 6 | 32 | four uint64 **big-endian** dimensions, in the order 1,C,h,w |
| 38 | 4*C*h*w | NCHW row-major fp32 raw bit patterns, 4 bytes **little-endian** per element |

All dimensions are positive integers, batch must be 1; the length must match exactly
and trailing data is not accepted. The canonical init must be one of the three
allowed shapes; the generic serializer can handle other batch=1 positive dimensions
to support reference tests. NaN/Inf are rejected; the bit patterns of ±0 are
preserved, with no numeric textualization or normalization. Subsequent trajectories,
audit in/out evidence and the init must use the same serialization. A bare payload
may be stored separately as an evidence file, but the commitment hash is always
`sha256(header || payload)`; the audit tensor_hash rebuilds the header and then
reads the bare file.

`latent_hash = SHA-256(canonical serialization bytes)`; the `sha256` in documents and
fixtures is that value. `raw_sha256` is for diagnostics only and must not be mistaken
for a commitment. The tensor descriptor remains `f32|1,4,h,w|raw-le-v1`; the shape
does not omit batch.

## 5. SD1.5 Scheduler Configuration and Timesteps (v2)

The sole configuration source of truth is the pinned SD1.5 asset
`scheduler/scheduler_config.json`. Method-by-method reference: diffusers 0.32.2;
source evidence is in the external `t2g-logs/`: `DDIMScheduler-__init__.py`,
`DDIMScheduler-set_timesteps.py`, `DDIMScheduler-step.py`, the corresponding Euler
files, and the `scale_model_input` files.

- Training steps 1000; beta_start=0.00085, beta_end=0.012; `scaled_linear`:
  `β = torch.linspace(sqrt(0.00085),sqrt(0.012),1000,dtype=float32)**2`.
  `A=torch.cumprod(1-β, dim=0)`, fp32; the old per-element scalar approximation
  construction is not used.
- prediction_type=epsilon; clip_sample=false; rescale_betas_zero_snr=false.
- DDIM leading: `r=1000//N`, `t_i=(N-1-i)*r+1`, steps_offset=1.
  For 20 steps: `[951,901,…,51,1]`; for N=1: `[1]`.
  `a=A[t_i]`, `b=A[t_i-r]` if the index is non-negative, otherwise `b=A[0]`.
  set_alpha_to_one=false, eta=0, use_clipped_model_output=false.
- Euler: `t=np.linspace(0,999,N,dtype=float32)[::-1]`, keeping floating-point
  timesteps; for N=1: `[0.0]`. The training sigmas are `sqrt((1-A)/A)`, interpolated
  via `np.interp(t,arange(1000),training_sigma).astype(float32)`, with 0 appended at
  the end. init_noise_sigma is the first sigma (the maximum of the sequence),
  14.614646911621094 for 20 steps. interpolation_type=linear; final_sigmas_type=zero;
  churn=0; no Karras, ancestral, exponential, or extra noise. The UNet receives this
  floating-point t.
- Scheduler state, model and updates are all fp32; generation and audit share the
  Formula implementation. The torch CPU array construction of beta/alpha and Euler's
  numpy interpolation rounding follow the reference source; the Philox finite-series
  rules are used only for the initial standard normal samples and do not replace the
  scheduler's native square roots.

## 6. CFG, DDIM and Euler Per-Step Updates

In this section, `sqrt_fp32` denotes the fp32 square root of the reference scheduler
coefficients; it does not use the finite-series approximation of §2.

Each step executes the uncond and cond passes as **two independent UNet forward
passes**, not concatenated into a batched forward; both use the same current
model_input, t_i, and the corresponding prompt embedding. Denote the outputs ε_u,
ε_c. The request guidance is stored as an integer G under the existing fixed-point
convention, 1000≤G≤30000, `g=F(G) ⊘ F(1000)`; strictly per element,
`ε=ε_u ⊕ (g ⊗ (ε_c ⊖ ε_u))`. Even when g=1 the same path is executed: the negative
forward pass is not skipped, the expression is not rewritten into another algebraic
form, and no CFG rescale is applied.

### 6.1 DDIM, eta=0

The current input x_i is the model_input (no extra scaling). Let `a=A_{t_i}`,
`b=A_{t_i-1000//N}`, with b=A_0 for a negative index; all operations below are per
element, with scalars shared:

1. `sa=sqrt_fp32(a)`; `sna=sqrt_fp32(1 ⊖ a)`;
   `pred_x0=(x_i ⊖ (sna ⊗ ε)) ⊘ sa`.
2. `sb=sqrt_fp32(b)`; `snb=sqrt_fp32(1 ⊖ b)`.
3. `x_{i+1}=(sb ⊗ pred_x0) ⊕ (snb ⊗ ε)`.

Equivalent real-number formulas: `x̂_0=(x_i−√(1−a)ε)/√a`;
`x_{i+1}=√b x̂_0+√(1−b)ε`. x̂_0 is not clipped, no dynamic thresholding, no variance
term is added, and no extra randomness is consumed. The last step still executes the
same three instructions; there is no special-case return of x̂_0 via another rounding
path.

### 6.2 Euler, Deterministic ODE

Let s=σ_i>0, s_next=σ_{i+1}. First compute
`denom=sqrt_fp32((s ⊗ s) ⊕ 1)`; per element, `model_input=x_i ⊘ denom`.
Run the two UNet passes and CFG with that model_input, then update using the
**unnormalized x_i**:

1. `pred_x0=x_i ⊖ (s ⊗ ε)`.
2. `d=(x_i ⊖ pred_x0) ⊘ s`.
3. `dt=s_next ⊖ s`; `x_{i+1}=x_i ⊕ (d ⊗ dt)`.

Equivalent real-number formulas: `x̂_0=x_i−sε`, `d=(x_i−x̂_0)/s`,
`x_{i+1}=x_i+d(s_next−s)`. Steps 1 and 2 must not be skipped just because d=ε in
real-number algebra; the fp32 rounding differs. No ancestral decomposition, no
historical state, no s_hat/churn. The first transition first multiplies the standard
normal init by σ_0 per §2.4; afterwards it only receives the previous step's output.

Both samplers save `x_0=init` and the post-update `x_1..x_N` after each step, N+1
latent snapshots in total. This package does not define a leaf structure or implement
a chain state machine.
VAE decoding, the pixel anchor, and numeric calibration are outside this package's
frozen scope; the exact-agreement requirement for the init must not be extended to
require bit-exact cross-hardware UNet/VAE outputs — the latter are audited against
the release calibration band.

DDIM eta>0, Euler ancestral, DPM++ SDE, and other stochastic-injection samplers are
excluded: they consume randomness at every step, and the current experiments have
not verified their device-independent per-step random streams. They must not enter an
auditable profile before an independent RNG specification and divergence calibration
are complete. DPM-Solver++(2M) is deterministic but is also not admitted, because
recomputation of its history terms is not included.

## 7. Conformance Vectors (Frozen at First Computation)

Authoritative fixture: `testdata/vectors/t2i-v1.json`; each row contains the seed,
the full shape, `raw_sha256`, `sha256`, and the fp32 bit patterns of the first 8
elements. The following are the init latent SHA-256 values **including the ETV1
header**. The first-computation tool `testdata/vectors/generate_t2i.py` uses the
Python reference implementation; Go independently generates identical bytes in full
and verifies them, without using Python latents as Go's input.

| seed | shape (display omits the constant batch=1) | sha256 (canonical serialization) |
|---|---|---|
| 0 | 4×64×64 | `e98e2a594e16b852712167b09b3cf46894e2def67360df61a0456f2ab9d12b3c` |
| 0 | 4×64×96 | `b1202b8e92d0c7b002724daccaf64f9075422b899930b63d9e475c6b3517f756` |
| 0 | 4×96×64 | `05ccd64d2f0ed2ebf0902ce2dc92b03f509c5c524885ee81afae5215b572b0d1` |
| 1 | 4×64×64 | `d06a29c9f027a214634ac91cd3d1606df357125d3ba8aafec43c65ef794f8725` |
| 1 | 4×64×96 | `9a9d73dbf8c1ac4718b052b2d8943571bf1ab833be3d321a47bcdc2a2c1bf2b4` |
| 1 | 4×96×64 | `f5ef51cfc4b16c0c049483f3d5c63c8cd72925c1e389d36b4c59b456a580c759` |
| 42 | 4×64×64 | `c0335c8fdcd134b8b392a485e30488ab3bde90a0d539d24a6f6a1f90aa93ef37` |
| 42 | 4×64×96 | `0548d34296507f4297e397378584dcc2240ceaf56bb17855438b234d8c583b72` |
| 42 | 4×96×64 | `916014aac06af0cca809296f8127bf2bfc1acf7b707aa26143d79fc07c692973` |
| 9223372036854775808 | 4×64×64 | `5768625a26a487a632d7968665887603b22758e63688a16ac79ba6f7be88dcb3` |
| 9223372036854775808 | 4×64×96 | `5f75b6d41a2cda60d7dc396a4d138cd1aa927a5d88c0b7e2405d580ddeef74a2` |
| 9223372036854775808 | 4×96×64 | `f0438a99ff1aab279592609c10e4b7a933a14f8755b4f112b93ec860bdccb261` |
| 18446744073709551615 | 4×64×64 | `53aa7546ed613bb49087cf24394e3966c2eb342cb1907b326df44cde81654a15` |
| 18446744073709551615 | 4×64×96 | `70f48c2dc6a998ec182eabd794aa109cdfe1ac2a8fb9c4826654fee3ad019db5` |
| 18446744073709551615 | 4×96×64 | `ea505965e259306331a4f5d47da4ced226497dd25dd01140f424ce870b9bbca4c59b456a580c759` |

The two non-square shapes with the same total element count have the same raw_sha256
(the RNG depends only on the linear index), but different canonical sha256 values
(the header pins the full shape). This is also the discriminative test for the shape
anchor.

## 8. Reference Implementations and Acceptance

- Go: `core-go/t2i.go`, interfaces `T2IPhilox`, `T2IInitLatent`,
  `T2ISerializeLatent`, `T2IEncodePrompt(s)`.
- Python: `core-py/ember_core/t2i.py`, corresponding interfaces `philox`,
  `init_latent`, `serialize_latent`, `encode_prompt(s)`; no torch/numpy/tokenizer
  dependency.
- Tests: `core-go/t2i_test.go`, `core-py/test_t2i.py`. The Go tests read the pinned
  fixture and run the Python helper to directly compare all 15 full canonical
  serialized tensors; the Python tests additionally use the existing audit
  `tensor_header` / `tensor_hash` to independently verify the evidence format. Both
  suites cover empty negative, K=75 exactly full, K=76 over-length rejection, and
  invalid inputs.
- To run the Go cross-language tests, set `EMBER_AUDIT_TEST_PYTHON` to a Python
  interpreter; the RNG helper needs only the standard library. Other cross-language
  tests in the repository additionally require that interpreter to have the existing
  test dependencies installed.
- First release is f32 only. This package introduces no network requests, model
  downloads, production service calls, chain transactions, publication, or
  activation.


## Errata and Execution Profile Version Bump (2026-11-23 UTC, T2H)

T2G confirmed the following five frozen errors. The supervisor ruled to correct the
specification and bump the version; the original v1 no longer accepts new
publications.

| Original error | Correct v2 semantics |
|---|---|
| Gaussian init ×0.18215 | ×1.0; the Philox raw sampling algorithm is unchanged |
| linear beta | float32 sqrt-endpoint linspace squared, scaled_linear |
| DDIM floor endpoints, offset=0 | leading ratio, offset=1; 20 steps from 951 to 1 |
| DDIM next-list alpha, final 1 | t−1000//N indexing, final A[0] |
| Euler integer timesteps, no interpolation and no initial sigma scaling | floating-point linspace, training-sigma interpolation, first transition first multiplied by the first sigma, final 0 |

The execution profile is bumped from `diffusers-sd1.5-fp32-v1` to
`diffusers-sd1.5-fp32-v2`; the model profile ID `ember-stable-diffusion-1.5-v1` is
unchanged. The old profile directory is retained with its revocation record; the old
Base/StepCoef, VAE numerics, and precision-feature zone must not be reused as v2
calibration evidence. Both samplers' chain leaf 0 remains the independently
rebuildable standard normal sentinel; the Euler first transition includes the
preparation scaling, so the evidence boundary for generation and audit agrees. This
errata replaces the corresponding old numeric freezing requirements in the body text.
