#!/usr/bin/env bash
# Public documentation inclusion rules:
# - Audience test: explain what an operator, integrator or reviewer can do.
#   Team task sheets, acceptance logs and custody maps belong outside the repo.
# - Public docs cite only existing public in-repo paths for local references;
#   external primary-source URLs are allowed. English only; decisions use ADRs.
# - No project-stage labels, private deployment topology or unpublished evidence.
# Scope: docs/*.md, README/CHANGELOG/SECURITY, spi-kit/*.md, site sources and
# config examples. Immutable site/profiles artifacts and DISCLAIMER.md wording
# are never modified by this lint (generated profiles are excluded); runtime
# class labels and ordinary security terminology
# (private keys/networks, internal engine details) are legitimate public concepts.
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - <<'PY'
import json
import re
import subprocess
from pathlib import Path
from urllib.parse import unquote, urlsplit

root = Path.cwd()
# Include new public docs during local review, without scanning internal root notes.
paths = subprocess.check_output(
    ['git', 'ls-files', '--cached', '--others', '--exclude-standard', '-z']
).decode().split('\0')
public = {p for p in paths if p and (root / p).is_file()}

def included(p):
    return (p in {'README.md', 'CHANGELOG.md', 'SECURITY.md'}
            or (p.startswith(('docs/', 'spi-kit/')) and p.endswith('.md'))
            or (p.startswith('site/') and not p.startswith('site/profiles/')
                and p.endswith(('.html', '.json')))
            or (p.startswith('config/') and p.endswith('.example')))

markers = re.compile(
    r'\b[sS]\d{1,2}\b|\b(?:PROGRAM|DECISIONS|PLAN|TESTBED|REPORT|AGENTS|TASK\d*)\.md\b'
    r'|\bdocs/evidence/|(?<![\w/])evidence/|reviews/|~/local(?:/|\b)|/home/[^\s`"<>]+'
    r'|\b(?:rehearsal|testbed|AutoDL|Contabo|Orin1|DGX Spark)\b'
    r'|(?i:\binternal (?:draft|handoff|review|channel|rehearsal|testnet)\b)'
    r'|\bprivate operator channel\b|\bthe user(?:\x27s)? (?:(?:separate )?approval|approves|requests|explicitly)\b'
    r'|用户|拍板'
)
cjk = re.compile(r'[\u3400-\u4dbf\u4e00-\u9fff]')
inline = re.compile(r'!?\[[^\]\n]*\]\(\s*(<[^>\n]+>|[^\s)]+)(?:\s+"[^"\n]*")?\s*\)')
reference = re.compile(r'^\s*\[[^\]\n]+\]:\s*(<[^>\n]+>|\S+)', re.M)
# These live signed identifiers are interoperability values, not stage prose.
immutable_ids = ('ember-qwen2.5-1.5b-s22-cpu-v1', 'ember-s23-staging')
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

for path in sorted(public):
    if not included(path):
        continue
    checked += 1
    content = (root / path).read_text()
    # Preserve provider labels verbatim in the synchronized public access object.
    access_lines = set()
    if path == 'site/network.json':
        start = content.index('{', content.index('"access"'))
        _, length = json.JSONDecoder().raw_decode(content[start:])
        access_lines = set(range(content.count('\n', 0, start) + 1,
                                 content.count('\n', 0, start + length) + 1))
    for line, value in enumerate(content.splitlines(), 1):
        for pattern, label in ((markers, 'internal marker'), (cjk, 'non-English draft text')):
            candidate = value
            if pattern is markers:
                if line in access_lines:
                    candidate = candidate.replace('Contabo', '')
                for identifier in immutable_ids:
                    candidate = candidate.replace(identifier, '')
            match = pattern.search(candidate)
            if match:
                errors.append(f'{path}:{line}: {label}: {match.group()}')
    if path.endswith('.md'):
        for pattern in (inline, reference):
            for match in pattern.finditer(content):
                check_link(path, match.group(1), content.count('\n', 0, match.start()) + 1)

for error in errors:
    print(error)
print(f'docs-lint: {checked} public source files checked; {len(errors)} errors')
raise SystemExit(bool(errors))
PY
