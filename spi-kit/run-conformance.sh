#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")" && pwd)
ember_node=${EMBER_NODE:?set EMBER_NODE to the released ember-node executable}
tmp=$(mktemp -d)
trap '[[ -f "$tmp/pid" ]] && kill "$(cat "$tmp/pid")" 2>/dev/null || true; rm -rf "$tmp"' EXIT
python3 "$root/toy_http_engine.py" --port-file "$tmp/port" >"$tmp/server.log" 2>&1 &
echo $! >"$tmp/pid"
for _ in $(seq 1 100); do [[ -s "$tmp/port" ]] && break; sleep 0.05; done
port=$(cat "$tmp/port")
PYTHONPATH="$root" "$ember_node" conformance \
  --adapter "$root/examples/toy_adapter.py" --endpoint "http://127.0.0.1:$port"
