#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

ref=${1:-HEAD}
out=${2:-dist/reproducible}
out=$(realpath -m "$out")
commit=$(git rev-parse "$ref^{commit}")
epoch=$(git show -s --format=%ct "$commit")
rm -rf "$out"
mkdir -p "$out"
tmp=$(mktemp -d "$PWD/deploy/.release-repro-XXXXXX")
trap 'rm -rf "$tmp"' EXIT
for run in one two; do
  mkdir "$tmp/$run"
  git archive "$commit" | tar -x -C "$tmp/$run"
  EMBER_RELEASE_UNSIGNED=1 SOURCE_DATE_EPOCH="$epoch" EMBER_RELEASE_COMMIT="$commit" \
    EMBER_RELEASE_ARCHES="amd64 arm64" "$tmp/$run/release/build.sh" "$tmp/$run/dist"
  (cd "$tmp/$run/dist" && sha256sum ./*.tar.gz | sort -k2) >"$out/$run.sha256"
done
cmp "$out/one.sha256" "$out/two.sha256"
cp "$tmp/one/dist/"*.tar.gz "$tmp/one/dist/SHA256SUMS" "$out/"
printf 'ref=%s\ncommit=%s\nepoch=%s\nresult=PASS\n' "$ref" "$commit" "$epoch" >"$out/summary.txt"
