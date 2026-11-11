#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

out=${1:-/dev/stdout}
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
pattern='-----BEGIN [A-Z ]*PRIVATE KEY-----|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{30,}|sk-[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}'
revisions=0
while read -r revision; do
  revisions=$((revisions+1))
  git grep -I -n -E -e "$pattern" "$revision" -- . >>"$tmp" 2>/dev/null || true
done < <(git rev-list --all)
git grep -I -n -E -e "$pattern" -- . >>"$tmp" 2>/dev/null || true
sed -i '\|security/secret-scan.sh|d' "$tmp"
candidates=$(wc -l <"$tmp")
{
  printf 'scanner=git-grep-high-confidence\n'
  printf 'revisions=%d\n' "$revisions"
  printf 'tracked_worktree=true\n'
  printf 'high_confidence_candidates=%d\n' "$candidates"
  if ((candidates)); then cat "$tmp"; fi
} >"$out"
test "$candidates" -eq 0

# Public-document checks share the security gate; they need no secret material.
bash security/docs-lint.sh
