#!/usr/bin/env bash
# Deterministic local ABCI chain: no RPC addresses or operator keys accepted.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
mkdir -p .runtime/pricing docs/evidence/PRICING
export PRICING_EVIDENCE_DIR="$root/docs/evidence/PRICING"
go test ./services/chain -run '^TestSupplyDemandRehearsal$' -count=1 -v \
  > .runtime/pricing/rehearsal.log 2>&1
cat docs/evidence/PRICING/summary.json
