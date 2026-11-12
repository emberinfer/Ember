#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
dist=$(realpath "${1:?usage: verify.sh RELEASE_DIRECTORY}")
openssl pkeyutl -verify -pubin -inkey "$root/release/ember-root.pub" -rawin \
  -in "$root/release/trust-statement.txt" -sigfile "$root/release/trust-statement.sig"
root_hash=$(openssl pkey -pubin -in "$root/release/ember-root.pub" -outform DER | sha256sum | cut -d' ' -f1)
release_hash=$(openssl pkey -pubin -in "$dist/release-signing.pub" -outform DER | sha256sum | cut -d' ' -f1)
test "$(sed -n 's/^Root-SHA256: //p' "$root/release/trust-statement.txt")" = "$root_hash"
test "$(sed -n 's/^Release-SHA256: //p' "$root/release/trust-statement.txt")" = "$release_hash"
openssl pkeyutl -verify -pubin -inkey "$dist/release-signing.pub" -rawin \
  -in "$dist/SHA256SUMS" -sigfile "$dist/SHA256SUMS.sig"
cd "$dist"
# Only simple archive filenames are allowed; no paths may escape the release set.
python3 - <<'PY'
from pathlib import Path
import re
lines = Path('SHA256SUMS').read_text().splitlines()
if not lines or any(not re.fullmatch(r'[a-f0-9]{64}  \./ember-[A-Za-z0-9.-]+\.tar\.gz', line) for line in lines):
    raise SystemExit('invalid archive checksum list')
PY
if [[ ${EMBER_VERIFY_PARTIAL:-0} == 1 ]]; then
  sha256sum --strict --ignore-missing -c SHA256SUMS
else
  sha256sum --strict -c SHA256SUMS
fi
