# Signed GPU budget declarations

ResourceProfile.gpu_budget uses previously unused field 40. Its GPUBudget fields
are memory_permille (1), compute_permille (2), accelerator_memory_bytes (3).
Both fixed-point values must be in 1..1000; total accelerator memory must be
positive. Absent gpu_budget preserves old registration behavior.

The chain rejects claims whose memory budget cannot hold the declared weight
files. Span-specific files are counted where the atom/shard table identifies
them. Monolithic files or missing tensor byte proofs require the full file size
as a conservative bound; the chain never estimates bytes by dividing layer count.
This checks only declaration consistency, not measured physical enforcement.
Behavioral limits and audit enforcement remain node/audit responsibilities.

The join path maps EMBER_GPU_MEMORY_FRACTION and EMBER_GPU_COMPUTE_CAP exactly
to thousandths and measures total accelerator memory before signing. Without
these settings it omits the field. Unmeasurable totals are rejected, not guessed.
Consumers must label these values "node declared"; compute is a declared soft cap.
The pre-GPU descriptor fixture verifies that old readers ignore/preserve the new
field and that new readers accept the old field-absent profile.
