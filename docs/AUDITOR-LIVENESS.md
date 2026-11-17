Resident auditor liveness
==========================
emberd with --ledger-backend chain hosts the identity loaded by
--chain-auditor-key (deploy/ember-role.sh supplies EMBER_AUDITOR_KEY). Its resident
Go guardian now polls /state even with no audit traffic and submits prove_liveness
when liveness_window - (height - last_proof_height) < threshold, respecting the
chain's proof_min_interval. Submission shares the ledger nonce mutex with audits;
RPC/broadcast failures retry on the next poll. Flags default to enabled:
--liveness-enabled=true --liveness-threshold=300 --liveness-poll=10s.

For 684 / auditor 47dfaff1: no checked-in deployment config maps that prefix to a
PID or key path, so the exact running process cannot be established from this
repository. Match the full account ID derived from the configured auditor key
against /state before enabling. If hosted by emberd, use its existing
--chain-auditor-key and the defaults above when deploying the new binary. If
hosted by node's enrolled audit-role process, its existing resident sender is
started by --chain-rpc; use the node guardian options documented below. A
one-shot python -m auditor job is not a resident identity host. No production
processes were inspected or changed for this implementation.
The node agent's flags are --liveness-enabled (default on),
--no-liveness-enabled, --liveness-threshold 300 and --liveness-poll 10 (seconds).
It retains the normal proof_interval cadence and additionally guards the remaining
window. It reads the identity from --state-dir/identity.key.
