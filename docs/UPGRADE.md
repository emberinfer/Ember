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
canonical consensus request identity. Version-3 input remains unsupported. The reviewed version-2 bridge described below is a separate, manifest-gated path.

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


## Manifest-gated version-2 bridge

For the reviewed F6 in-place upgrade, `emberchain start --membership-upgrade
/path/to/membership-upgrade.json` enables deterministic load-time normalization
of version-2 state through logical stages 2 -> 3 -> 4. Without a manifest,
version-2 input still fails with `unsupported state version`. Distribute the same
manifest file to all validators and observers and cross-check its SHA256 before
activation. The existing file-distribution mechanism is unchanged.

The bridge preserves height, accounts, balances, stakes, nonces, map identities,
registrations, terminal payments and signed payload bytes. It installs only the
reviewed absent-field defaults: slash floor 100 BPS, repeat window 1000 blocks,
assignment window 3/cooldown 8, audit-work epoch 1000/limit 64 and the existing
misjudgment defaults. Absent token-fee commitments, compensation policies,
thresholds, evidence manifests and historical records remain absent. Present
later-v2 pricing, profiles and settlement shares are preserved without repricing.
Explicitly present parameter values, including zero values, are not replaced.

The result stays version 4 with no membership until the manifest's future
activation height. Existing activation then installs membership and reports
version 5 atomically. A saved pre-activation version-4 state loads normally on
restart. Migration performs no file write by itself; ordinary block commits save
the normalized state. Identical input state bytes and manifest produce identical
state bytes and hash: there is no clock, randomness, transaction replay or
iteration-dependent serialization. JSON map keys are sorted by Go's encoder.

**Confirmed-offense history starts empty at the upgrade height.** Version 2 did
not record authentic offense heights; settlement bounds cannot reconstruct them.
No backfill is fabricated. Repeat-offense multipliers therefore restart within
the 1000-block window; the configured base slashing rule remains unchanged.
Historical balances and stakes are never replayed.

Existing holds without a pinned request identity keep their original hold-ID
audit-selection domain and round proof, assigned auditors and ungraded
confirmation ladder. No request
identity, freeze signature or first offense grade is synthesized. Their original
confirmed-rejection refund/penalty path is retained. New freezes pin the current
request identity and use the current assignment and graded-verdict rules.
Policy-free legacy holds retain Amount/Actual accounting; authentic existing
role pricing remains effective. This bridge preserves in-flight holds through
normal signed transitions; the drain/new-genesis requirements above continue to
apply to other incompatible upgrades. Rehearse on isolated copies before any
coordinated deployment. Direct version-5 downgrade is unsupported.
Application version 5 adds consensus supply-and-demand parameters, window
counters and per-profile consumer price snapshots, plus frozen hold multipliers.
It removes scarcity weighting from role allocation. Loading version-4 persisted
state fails closed. Use the incompatible-state procedure above after draining
all active holds; preserve calibrated model tables and reviewed balances, and
review executor targets and the new epoch/protection settings in genesis.
All validators and chain-backed gateways must upgrade together. No automatic
state migration or public network upgrade is performed by this change.
## Genesis issuance state

Application version 5 adds optional genesis voucher issuance, permanent marked
sub-balances, calendar issuance counters and governor approval records. See
[Genesis consumption vouchers](genesis-issuance.md) for configuration and APIs.
All validators on an issuance chain must run this application version. Version 4
persisted state is rejected; this feature does not supply an in-place migration.

## In-place upgrades on ember-testnet-1 (versions 6-9)

The live testnet upgrades in place: validators arm a signed membership-upgrade
manifest naming a future activation height, restart on the new binary, and the
chain switches application version atomically at that height. State, accounts,
registrations and in-flight holds carry over; the drain-and-new-genesis
procedure above is reserved for upgrades that cannot migrate state.

- Version 6: token-equality audit verdicts with first-divergence margin
  absorption (ε = 1.0) and second-auditor corroboration for rejections.
- Version 7: audit eligibility rules for the unified profiles.
- Version 8 (2026-11-15): unified profiles, attached per-profile pricing,
  reputation audit sampling parameters, free layer ranges, MoE code (shipped
  disabled), GPU budget declarations, supply-and-demand pricing, contribution
  caps, genesis issuance code. Sampling was switched on separately by
  governance after activation.
- Version 9 (2026-11-16): MoE enablement (`moe_enabled`); tokenizer artifacts
  bound by genesis hash instead of file name.

Operational rule learned in production: once an upgrade has activated, remove
the `--membership-upgrade` argument from every validator start script — a
validator restarted with a consumed manifest refuses to boot — and cold-start
at least one validator to prove it.
