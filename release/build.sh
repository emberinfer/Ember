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
python3 release/check_network_json.py \
  --network-json site/network.json --version-file VERSION
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
  mkdir -p "$root/lib/ember/chain-genesis"
  cp chain-genesis/build.py chain-genesis/build_audit_profile.py "$root/lib/ember/chain-genesis/"
  # Ship the whole node/ tree instead of a hand-maintained file list.
  # The previous explicit whitelist repeatedly dropped runtime modules
  # (tokenization.py in v0.8.0, content_policy.py in v0.9.0, plus
  # moe_gateway/moe_rpc/pipeline_spool and others that were never
  # listed).  Copy everything, then remove only the exclusions below,
  # and let check_import_closure.py fail the build if any shipped
  # module imports a local module that is not shipped.
  cp -a node/. "$root/lib/ember/node/"
  # Exclusions (test / benchmark / drill only; none is imported by, or
  # invoked from, any shipped runtime entry point):
  #   test_*.py, test_support.py  - pytest/unittest suites and helpers
  #   */testdata/                 - test fixtures
  #   bench_segengine.py          - standalone stage benchmark tool
  #   drill_teacher_forcing.py    - offline audit drill tool
  #   spot_absence_peer.py,
  #   spot_integration_peer.py    - test doubles driven by Go tests
  #                                 against the source tree, not the bundle
  #   __init__.py (top level)     - repository unittest-discovery marker;
  #                                 the bundle node/ dir is a sys.path root,
  #                                 not a package
  #   requirements-*.txt          - re-emitted below in canonical form
  find "$root/lib/ember/node" -type f -name 'test_*.py' -delete
  find "$root/lib/ember/node" -type f -name 'test_support.py' -delete
  find "$root/lib/ember/node" -type d -name testdata -prune -exec rm -rf {} +
  rm -f "$root/lib/ember/node/__init__.py" \
    "$root/lib/ember/node/bench_segengine.py" \
    "$root/lib/ember/node/drill_teacher_forcing.py" \
    "$root/lib/ember/node/spot_absence_peer.py" \
    "$root/lib/ember/node/spot_integration_peer.py" \
    "$root/lib/ember/node"/requirements-*.txt
  python3 release/check_import_closure.py \
    --source-root node --source-root core-py --source-root proto \
    --bundle-root "$root/lib/ember/node" \
    --bundle-root "$root/lib/ember/core-py" \
    --bundle-root "$root/lib/ember/proto"
  cp node/requirements-spi.txt VERSION "$root/lib/ember/"
  cp "node/requirements-cpu-$arch.txt" "$root/lib/ember/requirements-cpu.txt"
  cp -a chain-genesis/ember-* "$root/profiles/"
  cp config/*.example "$root/config/"
  cp config/trust-policy.json "$root/config/"
  cp docs/QUICKSTART.md docs/VALIDATOR_GUIDE.md docs/UPGRADE.md docs/OPERATIONS.md \
    docs/THREAT_MODEL_SUMMARY.md docs/THREAT_MODEL.md docs/ENGINE_SPI.md docs/PRIVACY.md \
    docs/GENESIS_CEREMONY.md docs/SIGNING.md docs/HARDWARE.md docs/EVIDENCE_TRANSPORT.md "$root/docs/"
  cp LICENSE NOTICE README.md CHANGELOG.md SECURITY.md "$root/"
  cp site/network.json "$root/network.json"
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

cp site/network.json "$out/network.json"

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
(cd "$out" && sha256sum ./*.tar.gz network.json | sort -k2 >SHA256SUMS)

if [[ ${EMBER_RELEASE_UNSIGNED:-0} == 1 ]]; then
  printf 'UNSIGNED build regression: not a distributable release set\n' >&2
else
  release/sign.sh "$out" "$EMBER_RELEASE_SIGNING_KEY"
fi
