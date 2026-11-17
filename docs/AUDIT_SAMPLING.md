# Reputation audit sampling (application version 8)

The live chain uses the 30%/5%/2% sampling policy. The source parameter default is disabled until explicitly enabled. A governor can submit `set_audit_sampling` with
an entire valid `audit_sampling` parameter block, or a coordinated 7→8 upgrade
manifest can carry that block. The manifest can independently opt into
`moe_enabled`; neither feature is enabled by an omitted field.

After sealing the result, clients submit `resolve_audit` (`assign_audit` also
uses the same decision). The request's frozen policy selects a 30%, 5%, or 2%
base rate at audited-accept counts 0, 20, and 200. Only audited accepted work
with Actual ≥16 subunits advances progress. Amount multiplies the base rate by
max(1, frozen amount / 1000), capped at 100%. Penalty-box requests always audit.
A confirmed slash resets tier progress and imposes 100 mandatory audits, or
500 after a repeated offense within the existing slashing window. A segmented
request uses the greatest executor rate, with each executor's exposure divisor
frozen separately. Old requests without a snapshot retain divisor 5.

The existing assignment entropy is unchanged: SHA256(request identity followed
by the next M finalized block hashes). The first eight seed bytes, big endian,
modulo one million give the roll. `sampling` records the frozen snapshot,
amount, effective rate, seed blocks, seed, roll and sampled result.
`RecomputeSampling` verifies and reproduces it. Unsampled requests have
`unaudited=true`, accept-only settlement, and the same settlement-height delay.
They receive no second-party verification of their reported Actual.

Unsampled audit allocations enter `audit_pool[model][epoch]`, including
AuditComp allocations. Policy-free requests generate no allocation. After the
1000-block audit-work epoch, anyone can submit `distribute_audit_pool` with
`model_id` and `epoch`. Completed audit Actual weights payouts; largest
remainders preserve all subunits, with lexicographic identity tie breaking.
An epoch with no completed audit rolls its balance into the next epoch.
Probation redirects executor income separately and cannot spend pool funds.

Cooldown scaling, fallback, and completion-time streak counting are independent
switches. A single hard-eligible auditor is never masked. Larger pools use
min(configured cooldown, pool size). If every hard-eligible candidate is masked,
fallback selects the smallest maximum executor-pair streak, then uses the usual
seed ordering for ties. Self-audits, prior assignees, liveness, profile evidence
eligibility, privacy revocations and contribution limits remain hard rules.
`fallback_cooldown` records use of this fallback. Service failures burn no streak.

Version 8 keeps per-node `node_audit_stats` and per-hold counter events. Migration
folds historical holds once with the same counter event reducer. Legacy holds
lack actual audit/settlement timestamps: their persisted SettleHeight is the
canonical ordering bound for historical accepted work; slash and freeze heights
remain exact. New events retain exact heights. Existing signed bytes, payments,
and request identities are not replayed or rewritten. Zero-valued new parameter
blocks are omitted when reading legacy state. Validate upgrades on isolated copies and retain the activation manifests needed for historical replay. Follow [Upgrade](UPGRADE.md).
