"""Native block-FP8 arithmetic for MoE expert segments.

Production FP8 engines (vLLM's w8a8 block-FP8 kernels) compute expert
FFNs with e4m3 weights in their native quantized form — 128x128 block
scales on the weights, activations dynamically quantized per
(row, 128-column-group) with an fp32 scale, fp32 accumulation. An
auditor that instead dequantizes the weights to bf16 and replays in
bf16 observes the full FP8-vs-bf16 caliber gap: MOE-0 measured
2.7e-2..4.2e-2 relative L2 on real Qwen3.6-35B expert segments, which
forced a ~0.06 tolerance band and ate most of the margin that catches
cheaters.

This module is the auditor's FP8-native replay caliber (2026-11-15
decision): the same arithmetic family as production, implemented with
explicit block operations so it is deterministic and runs identically
on CPU and GPU auditors — no FP8 tensor-core kernel is required, the
quantization is applied explicitly and accumulation is fp32. The only
remaining divergence from a production kernel is accumulation order
inside the fp32 sums.

Only torch is imported here on purpose: calibration harnesses load
this file standalone, so the measured code is byte-identical to the
shipped code.
"""
import torch

BLOCK = 128
E4M3_MAX = 448.0


def quantize_groups(x):
    """Dynamically quantize activations per (row, BLOCK-column-group).

    x: fp32 tensor [N, C] with C divisible by BLOCK. Returns the
    dequantized view (fp32) of the e4m3 codes, i.e. what an FP8 kernel
    effectively multiplies with.
    """
    n, c = x.shape
    if c % BLOCK != 0:
        raise ValueError(f'contraction dim {c} not divisible by {BLOCK}')
    grouped = x.float().view(n, c // BLOCK, BLOCK)
    scale = (grouped.abs().amax(dim=2, keepdim=True) / E4M3_MAX).clamp(min=1e-12)
    codes = (grouped / scale).to(torch.float8_e4m3fn)
    return (codes.float() * scale).view(n, c)


def dequantize_weight(w, scale_inv):
    """w: e4m3 [R, C]; scale_inv: [R/BLOCK, C/BLOCK] -> fp32 weights."""
    r, c = w.shape
    if (r % BLOCK != 0 or c % BLOCK != 0 or
            tuple(scale_inv.shape) != (r // BLOCK, c // BLOCK)):
        raise ValueError(f'bad block-scale shape for weight {tuple(w.shape)}')
    return (w.float().view(r // BLOCK, BLOCK, c // BLOCK, BLOCK) *
            scale_inv.float().view(r // BLOCK, 1, c // BLOCK, 1)).view(r, c)


def fp8_block_linear(x, w, scale_inv):
    """One production-caliber FP8 matmul: y = x @ W^T in fp32.

    Activations are group-quantized as the kernels do; the accumulation
    walks input groups in order so the result is deterministic.
    """
    xq = quantize_groups(x)
    wd = dequantize_weight(w, scale_inv)
    out = torch.zeros(x.shape[0], w.shape[0], dtype=torch.float32,
                      device=x.device)
    for kb in range(w.shape[1] // BLOCK):
        sl = slice(kb * BLOCK, (kb + 1) * BLOCK)
        out += xq[:, sl] @ wd[:, sl].t()
    return out


def expert_ffn_fp8(x, gate, up, down):
    """Expert FFN y = down(silu(gate(x)) * up(x)) in FP8-native caliber.

    gate/up/down are (weight_e4m3, scale_inv) pairs. x may be any float
    dtype; the result is fp32.
    """
    g = fp8_block_linear(x, *gate)
    u = fp8_block_linear(x, *up)
    mid = torch.nn.functional.silu(g) * u
    return fp8_block_linear(mid, *down)
