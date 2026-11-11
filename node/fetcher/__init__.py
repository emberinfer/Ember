"""Verify every manifest artifact before exposing its downloaded path."""
import hashlib
from pathlib import Path
from urllib.request import urlopen


def fetch(artifact, destination, local_root=None):
    name = Path(artifact.name)
    if name.is_absolute() or '..' in name.parts or len(name.parts) != 1:
        raise ValueError('unsafe artifact name')
    destination = Path(destination)
    destination.mkdir(parents=True, exist_ok=True)
    target = destination / name
    source = Path(local_root) / name if local_root else artifact.source
    digest = hashlib.sha256()
    size = 0
    with (open(source, 'rb') if local_root else urlopen(source, timeout=30)) as reader, open(target.with_suffix(target.suffix + '.part'), 'wb') as writer:
        while data := reader.read(1024 * 1024):
            digest.update(data)
            size += len(data)
            writer.write(data)
    partial = target.with_suffix(target.suffix + '.part')
    if size != artifact.size or digest.digest() != artifact.sha256:
        partial.unlink()
        raise ValueError('artifact size/hash mismatch')
    partial.replace(target)
    return target


def verify_local(artifact, root):
    name = Path(artifact.name)
    if name.is_absolute() or '..' in name.parts or len(name.parts) != 1:
        raise ValueError('unsafe artifact name')
    path = Path(root) / name
    digest = hashlib.sha256()
    size = 0
    with path.open('rb') as reader:
        while chunk := reader.read(1024 * 1024):
            digest.update(chunk)
            size += len(chunk)
    if size != artifact.size or digest.digest() != artifact.sha256:
        raise ValueError(f'artifact size/hash mismatch: {name}')
    return path
