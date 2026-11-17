#!/usr/bin/env bash
# Sanitization gate for the public tree.
#
# The public repository must never reference the external specification
# drafts this design was once aligned with, their tracker pages, the
# people or institutions behind them, or internal deployment material.
# A single hit fails the public-repository checks. This script is the
# only file allowed to carry the patterns, and it excludes itself.
set -euo pipefail
cd "$(dirname "$0")/.."

pattern='draft-wang|datatracker|odsi|hanling|pengcheng|internal-preserve|scalable inference'
hits=$(git grep -I -n -i -E -e "$pattern" -- . ':(exclude)security/sanitization-check.sh' || true)
if [ -n "$hits" ]; then
  echo "sanitization-check: forbidden reference(s) in the public tree:" >&2
  echo "$hits" >&2
  exit 1
fi
echo "sanitization-check: clean"
