#!/usr/bin/env bash
set -euo pipefail

role=${1:?usage: ember-role node|validator|gateway CONFIG}
config=${2:?usage: ember-role node|validator|gateway CONFIG}
root=$(cd "$(dirname "$0")/.." && pwd)
set -a
# shellcheck disable=SC1090
source "$config"
set +a

case "$role" in
  node)
    : "${EMBER_COORDINATOR:?}" "${EMBER_TLS_FINGERPRINT:?}" "${EMBER_MODEL_CHAIN:?}"
    : "${EMBER_MODEL_PATH:?}" "${EMBER_SCHEDULER_ID:?}" "${EMBER_CHAIN_RPC:?}"
    : "${EMBER_LEDGER_CHAIN_ID:?}" "${EMBER_GENESIS_SHA256:?}" "${EMBER_MANIFEST_SHA256:?}"
    node_args=()
    if [[ -n ${EMBER_PROFILE_TRUST_POLICY:-} ]]; then node_args+=(--trust-policy "$EMBER_PROFILE_TRUST_POLICY"); fi
    if [[ ${EMBER_AUDIT_ROLE:-false} == true ]]; then node_args+=(--audit-role); fi
    exec "$root/bin/ember-node" join --state-dir "$root/var/node" \
      --coordinator "$EMBER_COORDINATOR" --tls-fingerprint "$EMBER_TLS_FINGERPRINT" \
      --chain "$EMBER_MODEL_CHAIN" --engine transformers --model-path "$EMBER_MODEL_PATH" \
      --device "${EMBER_DEVICE:-auto}" --scheduler-id "$EMBER_SCHEDULER_ID" \
      --chain-rpc "$EMBER_CHAIN_RPC" --chain-cli "$root/bin/emberchain" \
      --ledger-chain-id "$EMBER_LEDGER_CHAIN_ID" --genesis-sha256 "$EMBER_GENESIS_SHA256" \
      --manifest-sha256 "$EMBER_MANIFEST_SHA256" \
      --registration-ttl "${EMBER_REGISTRATION_TTL:-200000}" \
      --health-listen "${EMBER_NODE_HEALTH:-127.0.0.1:19192}" "${node_args[@]}"
    ;;
  validator)
    : "${EMBER_VALIDATOR_HOME:?}" "${EMBER_VALIDATOR_P2P:?}" "${EMBER_VALIDATOR_RPC:?}"
    exec "$root/bin/emberchain" start --home "$EMBER_VALIDATOR_HOME" \
      --p2p "$EMBER_VALIDATOR_P2P" --rpc "$EMBER_VALIDATOR_RPC" \
      --metrics "${EMBER_VALIDATOR_METRICS:-127.0.0.1:26660}" \
      --peers "${EMBER_VALIDATOR_PEERS:-}"
    ;;
  gateway)
    : "${EMBER_GATEWAY_HTTP:?}" "${EMBER_GATEWAY_GRPC:?}" "${EMBER_CHAIN_RPC:?}"
    : "${EMBER_LEDGER_CHAIN_ID:?}" "${EMBER_COORDINATOR_KEY:?}" "${EMBER_AUDITOR_KEY:?}"
    : "${EMBER_AUTH_FILE:?}" "${EMBER_TLS_CERT:?}" "${EMBER_TLS_KEY:?}"
    : "${EMBER_MODEL_GENESIS:?}" "${EMBER_MODEL_MANIFEST:?}" "${EMBER_MODEL_PATH:?}"
    gateway_args=()
    if [[ -n ${EMBER_PROFILE_TRUST_POLICY:-} ]]; then gateway_args+=(--trust-policy "$EMBER_PROFILE_TRUST_POLICY"); fi
    if [[ -n ${EMBER_FAUCET_KEY:-} || -n ${EMBER_FAUCET_AMOUNT:-} ]]; then
      : "${EMBER_FAUCET_KEY:?}" "${EMBER_FAUCET_AMOUNT:?}" "${EMBER_FAUCET_STATE:?}"
      gateway_args+=(--faucet-key "$EMBER_FAUCET_KEY" --faucet-amount "$EMBER_FAUCET_AMOUNT" \
        --faucet-state "$EMBER_FAUCET_STATE" --faucet-window "${EMBER_FAUCET_WINDOW:-24h}")
      if [[ -n ${EMBER_FAUCET_HTTP:-} ]]; then gateway_args+=(--faucet-http "$EMBER_FAUCET_HTTP"); fi
      if [[ -n ${EMBER_FAUCET_TRUSTED_PROXY:-} ]]; then gateway_args+=(--faucet-trusted-proxy "$EMBER_FAUCET_TRUSTED_PROXY"); fi
    fi
    exec "$root/bin/emberd" all --ledger-backend chain --chain-rpc "$EMBER_CHAIN_RPC" \
      --chain-id "$EMBER_LEDGER_CHAIN_ID" --chain-coordinator-key "$EMBER_COORDINATOR_KEY" \
      --chain-auditor-key "$EMBER_AUDITOR_KEY" --chain-auth-file "$EMBER_AUTH_FILE" \
      --scheduler-key "$EMBER_COORDINATOR_KEY" --audit-key "$EMBER_AUDITOR_KEY" \
      --http "$EMBER_GATEWAY_HTTP" --grpc "$EMBER_GATEWAY_GRPC" \
      --tls-cert "$EMBER_TLS_CERT" --tls-key "$EMBER_TLS_KEY" \
      --genesis "$EMBER_MODEL_GENESIS" --manifest "$EMBER_MODEL_MANIFEST" \
      --engine transformers --model-path "$EMBER_MODEL_PATH" \
      --auditor-python "${EMBER_AUDITOR_PYTHON:-$root/venv/bin/python}" \
      --auditor-pythonpath "${EMBER_AUDITOR_PYTHONPATH:-$root/lib/ember/core-py:$root/lib/ember/proto:$root/lib/ember/node}" \
      --audit-device "${EMBER_AUDIT_DEVICE:-cpu}" \
      --request-timeout "${EMBER_REQUEST_TIMEOUT:-15m}" \
      --max-concurrent "${EMBER_MAX_CONCURRENT:-8}" \
      --audit-workers "${EMBER_AUDIT_WORKERS:-2}" \
      --audit-timeout "${EMBER_AUDIT_TIMEOUT:-3m}" "${gateway_args[@]}"
    ;;
  *) printf 'unknown role: %s\n' "$role" >&2; exit 2 ;;
esac
