#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
python=${PROFILE_PYTHON:-python3}
cli=${PROFILE_CHAIN_CLI:-emberchain}
while IFS= read -r record; do
  rpc=$("$python" -c 'import json,sys;print(json.load(open(sys.argv[1]))["rpc"])' "$record")
  anchor=$(mktemp)
  trap 'rm -f "$anchor"' EXIT
  cp "$(dirname "$record")/ledger-genesis.json" "$anchor"
  chmod 600 "$anchor"
  "$python" release/profile_pages.py --check --rpc "$rpc" --chain-cli "$cli" \
    --ledger-genesis "$anchor" --artifacts chain-genesis --output site/profiles
  rm -f "$anchor"
  trap - EXIT
done < <(find site/profiles -name release.json -type f | sort)
