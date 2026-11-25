# Hardware Sizing

Sizing is per immutable model/Profile and evidence grade. Measure the complete
process, tokenizer, KV cache, hidden-state buffers, and audit overhead; model file
size alone is not sufficient.

Ledger nodes need durable consensus/application storage and no model GPU for
consensus. Gateway sizing must include queues, evidence storage and any hosted
replay worker. Serving/audit sizing depends on the loaded weights, context and
concurrency. No universal core/RAM/disk minimum is established by the source.
Measure representative workloads and retain capacity headroom before admission.

## Small-device shard guidance

Choose a shard only after measuring its peak resident memory. A practical initial
budget is at most 60% of currently available memory for weights plus runtime
buffers, leaving at least 25% host headroom and the remainder for KV/evidence
growth. Reduce layer count when long-context KV growth or hidden-state transfer
breaks that bound. WAN shard performance has not been established by the LAN pipeline. One
hidden-state round trip per generated token remains a material constraint.

For a candidate shard, run representative short and long prompts, record peak RSS,
first-token latency, steady token latency, transfer bytes, and failure recovery.
Admission must use the measured Profile, not a device marketing capacity figure.

## Explicit x86_64 CUDA installation

Use `./install-release.sh --role node --prefix "$HOME/ember-cuda" --python python3.12 --profile-family cuda`
from the verified amd64 release. Install an NVIDIA Linux driver >=550.54.14 first;
`nvidia-smi -L` must work. A separately installed CUDA toolkit is unnecessary for
the torch wheel. arm64 CUDA is unsupported by this family and is rejected.

The CUDA requirements pin torch **2.5.1+cu124** (CUDA **12.4**), with the shared
SPI environment's Transformers **4.45.2** and tokenizers **0.20.3**. At public RPC
height 414581, signed active A16 and RTX 2080 Ti ResourceProfiles reported this
combination. The active 27B remote-vLLM profile omitted torch/CUDA and reported
an unknown engine version; it is not evidence for a vLLM installation pin.
The observed registrations were unexpired at that query height.

CPU remains the default, including on GPU machines; the installer prints a
notice and never silently changes families. CUDA installation rejects missing
GPUs, unsupported architectures and old drivers before copying files. After
installation it checks the actual torch build, CUDA runtime, availability and
a real device allocation. Explicit CUDA never silently falls back to CPU.

Verify the loaded engine's actual capabilities and runtime, using your verified
model and a measured CUDA-compatible policy:

```sh
PREFIX="$HOME/ember-cuda"
MODEL="$HOME/models/Qwen2.5-1.5B-Instruct"
"$PREFIX/venv/bin/python" - <<'PY'
import torch
print(torch.__version__, torch.version.cuda, torch.cuda.is_available())
assert torch.__version__ == '2.5.1+cu124' and torch.version.cuda == '12.4'
assert torch.cuda.is_available()
print(torch.cuda.get_device_name(0))
PY
"$PREFIX/bin/ember-node" profile verify --genesis ./publication/genesis.pb \
  --manifest ./publication/manifest.pb --model-path "$MODEL" --conformance --device cuda
```

The adapter obtains runtime declarations directly from loaded torch and
Transformers libraries; `requested_device`, `actual_device`, torch/CUDA versions
and accelerator name are signed into ResourceProfile. The declared policy must
cover that actual runtime/device pair. A version string alone does not prove
conformance or calibration. Driver/runtime compatibility does not imply enough
memory for a model: admission retains measured memory checks and serving limits.
