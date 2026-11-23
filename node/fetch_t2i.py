"""Fetch the pinned image components from hf-mirror over a direct connection.

Run separately from the node; inference itself never downloads assets.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path


def fetch(destination, force=False):
    import requests
    from engine.adapters.diffusers_t2i import ASSETS
    manifest = json.loads(ASSETS.read_text())
    session = requests.Session()
    session.trust_env = False  # explicitly ignore all proxy environment settings
    root = Path(destination)
    for name, expected in manifest['files'].items():
        path = root/name
        if path.is_file() and not force:
            with path.open('rb') as source:
                if hashlib.file_digest(source, 'sha256').hexdigest() == expected:
                    print(expected, name, 'verified local', flush=True)
                    continue
        path.parent.mkdir(parents=True, exist_ok=True)
        temporary = path.with_name(path.name + '.download')
        url = f"https://hf-mirror.com/{manifest['repo']}/resolve/{manifest['revision']}/{name}"
        digest = hashlib.sha256()
        try:
            with session.get(url, stream=True, timeout=(30, 120)) as response:
                response.raise_for_status()
                with temporary.open('wb') as target:
                    for chunk in response.iter_content(1024*1024):
                        target.write(chunk)
                        digest.update(chunk)
                    target.flush()
                    os.fsync(target.fileno())
            if digest.hexdigest() != expected:
                raise ValueError('download SHA-256 mismatch: ' + name)
            os.replace(temporary, path)
        finally:
            temporary.unlink(missing_ok=True)
        print(expected, name, 'direct hf-mirror', flush=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--destination', required=True)
    parser.add_argument('--force', action='store_true')
    options = parser.parse_args()
    fetch(options.destination, options.force)
