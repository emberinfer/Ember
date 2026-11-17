#!/usr/bin/env bash
# VERSION is the in-repo software-release source of truth.
# Scope: README testnet-live sentence and release literals; site/network.json
# release.version/base_url and assets; site/index.html EMBER_VERSION and release
# literals; docs/QUICKSTART.md EMBER_VERSION and release literals.
# Historical files (including CHANGELOG.md) are excluded. Chain AppVersion,
# profile IDs, dependencies and protocol/schema versions are separate concepts.
# Missing anchors or invalid metadata fail closed with file:line diagnostics.
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - <<'PY'
import json
import re
from pathlib import Path
errors = []
def fail(path, line, message):
    errors.append(f'{path}:{line}: {message}')
def read(path):
    try:
        return Path(path).read_text()
    except OSError as exc:
        fail(path, 1, str(exc))
        return ''
version = read('VERSION').strip()
if not re.fullmatch(r'\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?', version):
    fail('VERSION', 1, 'expected a software release version')
for path, anchor in [
    ('README.md', r'The public testnet is live with v(\d+\.\d+\.\d+(?:-[\w.-]+)?)\.'),
    ('site/index.html', r'EMBER_VERSION=([0-9A-Za-z.+-]+)'),
    ('docs/QUICKSTART.md', r'(?m)^EMBER_VERSION=([0-9A-Za-z.+-]+)$'),
]:
    content = read(path)
    matches = list(re.finditer(anchor, content))
    if not matches:
        fail(path, 1, 'missing required current-release anchor')
    for match in matches:
        if match.group(1) != version:
            fail(path, content.count('\n', 0, match.start()) + 1,
                 f'current release {match.group(1)!r}; expected {version}')
    for match in re.finditer(r'(?<![\w.])v?(0\.\d+\.\d+(?:-rc\.[0-9]+)?)(?![\d.])', content):
        if match.group(1) != version:
            fail(path, content.count('\n', 0, match.start()) + 1,
                 f'release literal {match.group()!r}; expected {version}')
path = 'site/network.json'
content = read(path)
def line_for(key):
    match = re.search(r'"' + re.escape(key) + r'"\s*:', content)
    return content.count('\n', 0, match.start()) + 1 if match else 1
try:
    release = json.loads(content)['release']
    for key, value in {
        'version': version,
        'base_url': f'https://github.com/emberinfer/Ember/releases/download/v{version}',
    }.items():
        if release.get(key) != value:
            fail(path, line_for(key), f'release.{key}={release.get(key)!r}; expected {value!r}')
    for match in re.finditer(r'(?<![\w.])v?(0\.\d+\.\d+(?:-rc\.[0-9]+)?)(?![\d.])', content):
        if match.group(1) != version:
            fail(path, content.count('\n', 0, match.start()) + 1,
                 f'release literal {match.group()!r}; expected {version}')
except (ValueError, KeyError, TypeError, AttributeError) as exc:
    fail(path, 1, f'invalid release metadata: {exc}')
for error in errors:
    print(error)
print(f'version-check: {len(errors)} errors; VERSION={version}')
raise SystemExit(1 if errors else 0)
PY
