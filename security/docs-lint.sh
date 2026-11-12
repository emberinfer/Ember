#!/usr/bin/env bash
# Public documentation inclusion rules:
# - Audience test: explain what an operator, integrator or reviewer can do.
#   Team task sheets, acceptance logs and custody maps belong outside the repo.
# - Public docs cite only existing public in-repo paths for local references;
#   external primary-source URLs are allowed. English only; decisions use ADRs.
# - No project-stage labels, private deployment topology or unpublished evidence.
# Scope: docs/*.md, README/CHANGELOG/SECURITY, spi-kit/*.md, site sources and
# config examples. Generated profile pages are checked separately; runtime
# class labels and ordinary security terminology
# (private keys/networks, internal engine details) are legitimate public concepts.
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - <<'PY'
import re
import base64
import subprocess
from pathlib import Path
from urllib.parse import unquote, urlsplit

root = Path.cwd()
# Tracked files are always public. Root review notes and runner logs stay local;
# include other untracked public content so local review catches new files too.
def git_paths(*args):
    return set(filter(None, subprocess.check_output(
        ['git', 'ls-files', *args, '-z']).decode().split('\0')))

tracked = git_paths('--cached')
local_notes = re.compile(base64.b64decode('Xig/Oig/OlRBU0tcZCp8UkVQT1JUXGQqfERFQ0lTSU9OU3xQUk9HUkFNfFRFU1RCRUR8QUdFTlRTKVwubWR8W14vXStcLmxvZykk').decode(), re.I)
paths = tracked | {p for p in git_paths('--others', '--exclude-standard')
                   if not local_notes.fullmatch(p)}
public = {p for p in paths if (root / p).is_file()}

def included(p):
    return (p in {'README.md', 'CHANGELOG.md', 'SECURITY.md'}
            or (p.startswith(('docs/', 'spi-kit/')) and p.endswith('.md'))
            or (p.startswith('site/') and not p.startswith('site/profiles/')
                and p.endswith(('.html', '.json')))
            or (p.startswith('config/') and p.endswith('.example')))

# Encoded expressions keep this guard free of the identifiers it rejects.
privacy_patterns = {'origin': 'T0RTSXxJRVRGfENBVFN8ZGF0YXRyYWNrZXJ8ZHJhZnR8YXJ4aXZ8dGVhbVstIF1wYXBlcg==', 'people': 'SGFubGluZyBXYW5nfOeOi+axieWHjHxZYW55ZSBLb25nfDB4TG9naWNMYWJ8a29uZ3lhbnllfGd1bGxhbGRlcnxIYW5saW5n', 'contacts': 'W0EtWjAtOS5fJSstXStAW0EtWjAtOS4tXStcLltBLVpdezIsfXxyZXNlYXJjaC1vc1wub25saW5lfDB4bG9naWNsYWJcLmdpdGh1YlwuaW8=', 'infrastructure': 'c3BhcmstZmM2OXwxMDBcLjc1XC4xODJcLjY3fDEzXC4xNDBcLjI0MlwuMTF8XGIxMDBcLig/OjZbNC05XXxbNy05XVswLTldfDFbMDFdWzAtOV18MTJbMC03XSlcLlswLTldK1wuWzAtOV0rfENvbnRhYm98QXV0b0RMfHNlZXRhY2xvdWR8XGJvcmluWzAtOV0rXGI=', 'process': 'XGJbU3NdXGR7MSwyfVxifCg/OlRBU0tcZCp8REVDSVNJT05TfFBST0dSQU18VEVTVEJFRHxBR0VOVFN8UkVQT1JUKVwubWR8ZG9jcy9ldmlkZW5jZS98L2hvbWUvfH4vZmlsZXMvfFtcdTM0MDAtXHU0ZGJmXHU0ZTAwLVx1OWZmZl0='}
privacy_patterns['additional'] = 'XGJ0YWlsbmV0XGJ8XGIoPzpUQVNLfFJFUE9SVHxERVNJR058TUVSR0UpWy1fIF0/XGQrXGJ8XGIoPzpzdGFnZXxwaGFzZSlccytbQS1GXVxifCg/PCFbXHddKWY1KD89Wy0gX10pfC9Vc2Vycy98W0EtWl06XFxVc2Vyc1xc'
privacy = {name: re.compile(base64.b64decode(value).decode(), re.I)
           for name, value in privacy_patterns.items()}
audit_patterns = {
    'process marker': re.compile(base64.b64decode('XGJUQVNLXGQrXGI=').decode(), re.I),
    'unsupported accelerator class': re.compile(base64.b64decode('cnR4MjA4MHRp').decode(), re.I),
}
markers = re.compile(r'\b[sS]\d{1,2}\b|\brehearsal\b|\btestbed\b')
cjk = re.compile(r'[\u3400-\u4dbf\u4e00-\u9fff]')
inline = re.compile(r'!?\[[^\]\n]*\]\(\s*(<[^>\n]+>|[^\s)]+)(?:\s+"[^"\n]*")?\s*\)')
reference = re.compile(r'^\s*\[[^\]\n]+\]:\s*(<[^>\n]+>|\S+)', re.M)
errors = []
checked = 0

def check_link(path, target, line):
    target = target.strip('<>')
    url = urlsplit(target)
    if url.scheme or url.netloc or not url.path:
        return
    resolved = (root / path).parent.joinpath(unquote(url.path)).resolve()
    try:
        local = resolved.relative_to(root).as_posix()
    except ValueError:
        errors.append(f'{path}:{line}: local link escapes repository: {target}')
        return
    # An ignored operator artifact is not a public reference even if present locally.
    if local not in public and not any(p.startswith(local.rstrip('/') + '/') for p in public):
        errors.append(f'{path}:{line}: missing public link target: {target}')

# Privacy checks cover every public file, including fixtures and binary strings.
for path in sorted(public):
    data = (root / path).read_bytes().decode('utf-8', 'replace')
    for label, pattern in (privacy | audit_patterns).items():
        for match in pattern.finditer(data):
            errors.append(f'{path}:{data.count(chr(10), 0, match.start()) + 1}: {label} identifier')

for path in sorted(public):
    if not included(path):
        continue
    checked += 1
    content = (root / path).read_text()
    if re.fullmatch(r'docs/[^/]+\.md', path):
        for match in re.finditer(r'\bstaging\b', content, re.I):
            errors.append(f'{path}:{content.count(chr(10), 0, match.start()) + 1}: deployment-process wording')
    for line, value in enumerate(content.splitlines(), 1):
        for pattern, label in ((markers, 'internal marker'), (cjk, 'non-English text')):
            candidate = value
            match = pattern.search(candidate)
            if match:
                errors.append(f'{path}:{line}: {label}: {match.group()}')
    if path.endswith('.md'):
        for pattern in (inline, reference):
            for match in pattern.finditer(content):
                check_link(path, match.group(1), content.count('\n', 0, match.start()) + 1)

version = (root / 'VERSION').read_text().strip()
version_match = re.fullmatch(r'(\d+\.\d+)\.\d+(?:[-+][0-9A-Za-z.-]+)?', version)
supported = re.findall(r'^Supported release(?:s)?:[^\n]*',
                       (root / 'SECURITY.md').read_text(), re.M | re.I)
release_lines = [v for line in supported for v in re.findall(r'`([^`]+)`', line)]
expected = version_match[1] + '.x' if version_match else None
if not release_lines or any(v != expected for v in release_lines):
    errors.append('SECURITY.md: supported releases must match the VERSION major.minor line')

for error in errors:
    print(error)
print(f'docs-lint: {checked} public source files checked; {len(errors)} errors')
raise SystemExit(bool(errors))
PY
