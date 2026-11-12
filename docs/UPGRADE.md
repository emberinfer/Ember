# Upgrade And Hard Fork Procedure

1. Stage the new semver archive, checksums, Ed25519 signature,
   changelog and model-profile hashes. Verify on two clean checkouts. Release maintainers approve publication after verification.
2. Record the coordinated halt height, expected old state hash, new chain ID and
   rollback deadline through the network's authenticated operator coordination process. Stop gateways before
   validators at the halt height.
3. Back up validator homes and account keys. Export the finalized whole state and
   verify it against the old genesis commit.
4. Query the finalized state and require zero `frozen` or `dispute` holds. Current
   genesis cannot preserve active holds, pending audits, registrations, evidence,
   entropy, or subsidy counters safely. A migration attempt with any active hold
   must fail closed. Stop admission, wait for settlement, cancel payer-owned
   frozen holds, or wait for height expiry/refund; then re-query and retain proof.
5. For an application-only compatible upgrade, replace binaries and restart the
   same chain. For a model/Profile addition or incompatible state rule, create a
   new genesis and chain ID, preserving reviewed balances/stake and adding the new
   immutable model hashes. Set issuance parameters so non-migrated counters cannot
   create duplicate issuance. Node registrations are re-created after launch. A
   model version is never silently mutated.
6. Run a new public-only genesis ceremony. Start at least two validators, verify
   the new genesis hash and first committed
   app hash, then reopen gateways and nodes.

Rollback if the new chain cannot commit, state/account totals differ, the new
profile hash is wrong, or more than one third of voting power reports divergent
state. Stop the new chain, restore the old stopped snapshot and old binaries, and
verify the old state hash before reopening traffic. Never run old and new signing
states concurrently.

Application version 4 adds frozen request identities and policy, future block-hash
history, pair streaks and assignment record v1. Loading a version-3 state directly
fails closed. Use the incompatible-state upgrade procedure above; active holds
and assignments cannot be silently translated to the new entropy rules. Existing
client hold lookup IDs remain usable, while each accepted freeze also records its
canonical consensus request identity. No automatic live state migration is shipped.

The same application-version change also introduces offense-record version 2
and optional signed `AuditVerdict.offense` grading. Confirmed rejections without
grading are rejected; registered replay workers supply it. Genesis defaults to
`misjudge.slash_mode=gray_gate`, repeat limit 3, window 1000 blocks and block cap
500 basis points. Numerical bands require explicit per-model calibration entries;
no global numerical threshold is silently installed. Request-frozen tables and
pending-review states are consensus data. Appeals resolution is not implemented.

The same application version also includes optional per-model audit compensation
policy, signed verdict prefix citations, version-2 settlement statements and
frozen audit-work limits. Deployments must publish measured per-model policy
before enabling compensation. Existing policy-free models retain their previous
settlement behavior; this is not an automatic benchmark migration. See
[AUDIT_COMPENSATION](AUDIT_COMPENSATION.md) for parameters, funding limits and
operator artifacts.

Application version 4 adds signed evidence manifests and requires audit verdicts
to cite their roots. This changes transaction validation and committed state.
Use the new-genesis procedure above, after draining active holds; do not restart
an application-version-3 chain with mixed validator binaries. Review the genesis
`evidence.max_frame_tokens` setting (default 512) and preserve the gateway
evidence directory until all required evidence has been retained. See
[Evidence transport](EVIDENCE_TRANSPORT.md).
