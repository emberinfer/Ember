# SD1.5 fp32 v2 paired calibration

Model profile ID: ember-stable-diffusion-1.5-v1. Execution profile: diffusers-sd1.5-fp32-v2.

T2H fixes unit Gaussian init, scaled_linear beta, DDIM leading/previous alpha and Euler float timesteps/interpolated sigmas/preparation. Leaf zero is unit Gaussian; the first Euler transition includes init sigma multiplication in both generation and audit.

Complete new CPU/GPU calibration: 12 trajectories, 732 paired points; soft(step)=4.5211e-06+1.23262e-05*step, hard=100*soft. Maximum first-step intercept and minimum envelope slope each receive 10% margin and upward rounding to 1e-10 (T4T convention). VAE: within_one >=0.999; all 12 measurements are 1.0.

New conditional precision region [0.043,0.06] derives from F32 audit of actual F16 Euler N=1. DDIM has no observed feature region; all DDIM measurements are below the region. Euler20 measurements are also below it. Both audit directions are retained separately in the evidence. Declaring F16 alone never establishes configuration-error eligibility.

The manifest and independent device signatures bind all raw traces, VAE samples and precision evidence in external t2h-logs. Weight files are unchanged, not vendored. scheduler_cfg.json derives from the pinned SD1.5 scheduler asset; arch_manifest.json is unchanged publication material. v1 material and bands are revoked.
