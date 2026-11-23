# SD1.5 fp32 paired calibration

The image profile and pixel policy bind the T4 GPU/CPU measurements. All raw
ETV1 trajectories remain in the T4 evidence archive; the manifest and two device
signatures bind their exact bytes. The pinned model weights are not vendored.
`scheduler_cfg.json` and `arch_manifest.json` are locally derived publication
materials, not artifacts claimed to exist in the upstream HF revision.

The only image comparison criterion is `mode=within_one`: at least 99.9% of
RGB pixels must have all three channel differences at most one. Relative L2 is
also recorded for evidence but does not decide image acceptance.

The precision feature interval is conditional: measured F16 steps outside the
interval do not qualify for configuration-error treatment merely by declaring
F16. DDIM F16 single-step distances in this matrix fall below the interval.
