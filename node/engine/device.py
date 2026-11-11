"""Shared local-engine memory and device policy."""
def available_bytes():
    with open('/proc/meminfo', encoding='ascii') as f:
        for line in f:
            if line.startswith('MemAvailable:'):
                return int(line.split()[1]) * 1024
    raise RuntimeError('MemAvailable unavailable')


def rss_bytes():
    with open('/proc/self/status', encoding='ascii') as f:
        for line in f:
            if line.startswith('VmRSS:'):
                return int(line.split()[1]) * 1024
    raise RuntimeError('VmRSS unavailable')


def memory_budget(torch, device='cuda'):
    """Qwen2.5-1.5B limits: available memory, CUDA allocator, RSS, reserve.

    Orin-class policy sets a system-memory reserve and lower allocator/RSS limits.
    Other GPUs use the conservative default limits. A device name never bypasses admission.
    """
    gib = 1024 ** 3
    name = (getattr(torch.cuda, 'get_device_name', lambda _index: '')(0)
            if device == 'cuda' else '')
    if name in ('Orin', 'NVIDIA Orin'):
        return (7 * gib, 4 * gib, 6 * gib, 3 * gib)
    return (16 * gib, 8 * gib, 8 * gib, 0)


def check_memory(budget, current_rss=None, memory_available=None):
    """Fail this worker before its measured reserve is exhausted."""
    current_rss = rss_bytes() if current_rss is None else current_rss
    if current_rss > budget[2]:
        raise MemoryError('model process RSS exceeds memory budget')
    if budget[3]:
        free = available_bytes() if memory_available is None else memory_available
        if free < budget[3]:
            raise MemoryError('model process exhausted system memory reserve')


def select_device(torch, requested, memory_available=None):
    """Resolve the requested device; explicit CUDA never silently falls back."""
    if requested not in ('auto', 'cpu', 'cuda'):
        raise ValueError('invalid device')
    if requested == 'cpu':
        return 'cpu', 'cpu explicitly requested'
    if not torch.cuda.is_available():
        if requested == 'cuda':
            raise RuntimeError('CUDA requested but torch.cuda.is_available() is false')
        return 'cpu', 'auto fallback: CUDA unavailable'
    free = available_bytes() if memory_available is None else memory_available
    minimum = memory_budget(torch)[0]
    gib = minimum // 1024**3
    if free < minimum:
        if requested == 'cuda':
            raise MemoryError(f'CUDA requested but less than {gib} GiB system memory is available')
        return 'cpu', f'auto fallback: less than {gib} GiB system memory available'
    return 'cuda', 'CUDA selected'
