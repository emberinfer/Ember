Resident auditor liveness
==========================
emberd with --ledger-backend chain hosts the identity loaded by
--chain-auditor-key (deploy/ember-role.sh supplies EMBER_AUDITOR_KEY). Its resident
Go guardian now polls /state even with no audit traffic and submits prove_liveness
when liveness_window - (height - last_proof_height) < threshold, respecting the
chain's proof_min_interval. Submission shares the ledger nonce mutex with audits;
RPC/broadcast failures retry on the next poll. Flags default to enabled:
--liveness-enabled=true --liveness-threshold=300 --liveness-poll=10s.

The node agent's flags are --liveness-enabled (default on),
--no-liveness-enabled, --liveness-threshold 300 and --liveness-poll 10 (seconds).
It retains the normal proof_interval cadence and additionally guards the remaining
window. It reads the identity from --state-dir/identity.key.
