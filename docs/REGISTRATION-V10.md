# AppVersion 10 registration availability

New registrations offer execution and chain-drawn review by default. The signed
`register_node` action carries `profile`, expiry and capacity, plus optional
`no_audit` / `no_exec` booleans. The opt-outs are stored on the registration and
expire with it. Active coverage remains TTL-locked: renewal cannot withdraw an
enabled role before the current record expires.

`ember-node join` accepts `--no-audit` and `--no-exec`; omitting both offers both.
Engine capability roles continue to describe adapters (including shard/MoE
adapters), but no longer choose review versus execution for new registrations.
The existing contribution selector selects profiles and measured capacity and
remains independent of availability. Range admission still selects the held
range. This baseline contains no separate NM-5 auto-mode implementation or role
selector; future auto-mode callers must use the same registration opt-out fields.

The legacy `--audit-role` flag still parses. Before v10 it appended `auditor` to
existing engine roles: a model/shard engine therefore offered both roles. That
behavior is preserved. If an engine had no execution role, the legacy flag maps
to `no_exec=true`. `--audit-role --no-audit` is rejected as contradictory. No
sibling legacy execution-role CLI flag exists in this baseline.

Activation follows the existing AppVersion 9 manifest mechanism: all validators
load the same `--membership-upgrade` JSON with matching `chain_id` and future
`activation_height`. A version-9 state remains version 9 until exactly that
height; missed activation fails closed. The activation writes version 10 and a
consensus app-version update. Each current record maps its existing `auditor`
role to `no_audit = !has_auditor`, and any execution adapter role (`model`,
`executor`, `shard-stage`, `moe-attention`, `moe-expert`) to `no_exec = !has_exec`.
Neither signed profile bytes nor past assignments/statistics are rewritten.

Gateway candidates and chain execution admission exclude execution opt-outs.
Review candidate construction excludes review opt-outs; the lottery adds a
second exclusion check. Assignment still requires a committed result and future
finalized block entropy. The gateway receives no reviewer assignment capability.
Manifest identity, held-range containment, calibrated device paths, evidence,
liveness and contribution-capacity checks remain in place.

Coverage/supply statistics count the enabled registration roles at v10;
pre-v10 counting remains unchanged. Shard/MoE execution adapters count as enabled
executors at v10. Historical statistics, frozen assignment records, fee splits
and audit compensation formulas remain unchanged.
