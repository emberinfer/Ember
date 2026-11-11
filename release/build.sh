#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

version=$(tr -d '\n' <VERSION)
out=${1:-dist}
arches=${EMBER_RELEASE_ARCHES:-amd64 arm64}
epoch=${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct HEAD)}
commit=${EMBER_RELEASE_COMMIT:-$(git rev-parse HEAD)}
if [[ ${EMBER_RELEASE_UNSIGNED:-0} != 1 ]]; then
  : "${EMBER_RELEASE_SIGNING_KEY:?set EMBER_RELEASE_SIGNING_KEY for a distributable release (or EMBER_RELEASE_UNSIGNED=1 for build regression only)}"
  test -r "$EMBER_RELEASE_SIGNING_KEY"
fi
rm -rf "$out"
mkdir -p "$out"

for arch in $arches; do
  name="ember-$version-linux-$arch"
  root="$out/$name"
  mkdir -p "$root/bin" "$root/lib/ember/node" "$root/profiles" "$root/config" "$root/docs"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X ember/version.Version=$version" -o "$root/bin/emberd" ./cmd/emberd
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X ember/version.Version=$version" -o "$root/bin/emberchain" ./cmd/emberchain
  install -m 0755 release/ember-node "$root/bin/ember-node"
  cp -a core-py proto "$root/lib/ember/"
  cp -a node/agent node/auditor node/engine node/evidence node/fetcher \
    node/conformance.py node/pinned_tls.py node/pipeline_client.py node/shard_rpc.py \
    node/sharding.py node/tokenize_prompt.py \
    "$root/lib/ember/node/"
  cp node/requirements-spi.txt VERSION "$root/lib/ember/"
  cp "node/requirements-cpu-$arch.txt" "$root/lib/ember/requirements-cpu.txt"
  cp -a chain-genesis/ember-* "$root/profiles/"
  cp config/*.example "$root/config/"
  cp docs/QUICKSTART.md docs/VALIDATOR_GUIDE.md docs/UPGRADE.md docs/OPERATIONS.md \
    docs/THREAT_MODEL_SUMMARY.md docs/THREAT_MODEL.md docs/ENGINE_SPI.md \
    docs/GENESIS_CEREMONY.md docs/SIGNING.md docs/HARDWARE.md "$root/docs/"
  cp LICENSE NOTICE README.md CHANGELOG.md SECURITY.md "$root/"
  install -m 0755 deploy/ember-role.sh "$root/ember-role"
  install -m 0755 deploy/install-release.sh "$root/install-release.sh"
  find "$root" -type f \( -name '*.pyc' -o -name '*.pyo' \) -delete
  find "$root" -depth -type d -name __pycache__ -empty -delete
  {
    printf 'version=%s\ncommit=%s\nepoch=%s\narchitecture=%s\n' "$version" "$commit" "$epoch" "$arch"
    printf '\n[emberd go modules]\n'
    go version -m "$root/bin/emberd" | sed '1c\\bin/emberd'
    printf '\n[emberchain go modules]\n'
    go version -m "$root/bin/emberchain" | sed '1c\\bin/emberchain'
    printf '\n[python requirements]\n'
    cat "node/requirements-cpu-$arch.txt" node/requirements-spi.txt
  } >"$root/DEPENDENCIES.txt"
  find "$root" -exec touch -h -d "@$epoch" {} +
  tar --sort=name --mtime="@$epoch" --owner=0 --group=0 --numeric-owner \
    -C "$out" -czf "$out/$name.tar.gz" "$name"
  rm -rf "$root"
done


kit="ember-engine-spi-kit-$version"
mkdir -p "$out/$kit"
cp -a spi-kit/. "$out/$kit/"
cp LICENSE NOTICE "$out/$kit/"
find "$out/$kit" -type f \( -name '*.pyc' -o -name '*.pyo' \) -delete
find "$out/$kit" -depth -type d -name __pycache__ -empty -delete
find "$out/$kit" -exec touch -h -d "@$epoch" {} +
tar --sort=name --mtime="@$epoch" --owner=0 --group=0 --numeric-owner \
  -C "$out" -czf "$out/$kit.tar.gz" "$kit"
rm -rf "$out/$kit"
(cd "$out" && sha256sum ./*.tar.gz | sort -k2 >SHA256SUMS)

if [[ ${EMBER_RELEASE_UNSIGNED:-0} == 1 ]]; then
  printf 'UNSIGNED build regression: not a distributable release set\n' >&2
else
  release/sign.sh "$out" "$EMBER_RELEASE_SIGNING_KEY"
fi
