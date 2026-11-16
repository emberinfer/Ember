"""NM-7 signed declarations; no invented total-memory measurements."""
import os
from decimal import Decimal
from ember_pb2 import GPUBudget

def declared_budget(runtime, engine, environ=None):
    env = os.environ if environ is None else environ
    memory, compute = env.get('EMBER_GPU_MEMORY_FRACTION'), env.get('EMBER_GPU_COMPUTE_CAP')
    if memory is None and compute is None:
        return None
    def fixed(value):
        number = Decimal(value or '1') * 1000
        if not number.is_finite() or number != number.to_integral_value() or not 1 <= number <= 1000:
            raise ValueError('GPU budget must be exact thousandths in (0,1]')
        return int(number)
    total = runtime.get('accelerator_memory_bytes', 0)
    if not total and getattr(engine, 'device', '') == 'cuda' and hasattr(engine, 'torch'):
        total = engine.torch.cuda.get_device_properties(0).total_memory
    if not total:
        raise ValueError('GPU budget requires measured accelerator total memory bytes')
    return GPUBudget(memory_permille=fixed(memory), compute_permille=fixed(compute),
                     accelerator_memory_bytes=total)
