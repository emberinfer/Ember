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
