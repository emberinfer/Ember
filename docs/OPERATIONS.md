# Operations

Gateway: `/healthz`, `/readyz`, `/metrics`. Node: loopback `/healthz`, `/readyz`,
`/metrics` on the configured health address. Validator: CometBFT RPC `/status`
and optional Prometheus `/metrics` from `emberchain start --metrics`.

Logs are UTC, one event per line, with stable uppercase event names and request,
node, height and transaction hash fields where applicable. Do not log private keys,
bearer tokens or unredacted prompts outside the bounded audit spool.

Use `ops/alerts.yml` as the baseline Prometheus rules. Backups include validator
home, application state, node identity, gateway account keys and auth mappings.
Files containing keys stay mode 0600. Restore onto an isolated host, verify hashes
and state proof, then remove the isolated copy. Retain backup and restore
verification logs in operator-controlled storage with access appropriate to their
contents.

## Derived audit routing

New networks set `registered_auditors: true` in chain genesis parameters.
Auditors join with `ember-node join --audit-role` and the same pinned profile as
executors. The chain selects independent, active, stake-backed identities with
full forced-prefix replay capabilities and a matching runtime. The gateway
resolves the assigned identity over its authenticated outbound tunnel, verifies
its signed audit transaction, and broadcasts it. New executor registrations
require no audit-map edits. Offline assigned auditors remain a liveness risk;
hold expiry refunds the consumer. Legacy genesis can keep its historical
authority set with `--chain-audit-routing=false` during coordinated migration.
Changing consensus eligibility requires a fresh genesis or a separately
coordinated network upgrade; coordinate binary replacement with the network operators.

## Public testnet faucet listener

Keep the consumer gateway on loopback. Set `EMBER_FAUCET_HTTP` (or
`--faucet-http`) to a separate bind at public testnet launch, alongside the
existing dedicated faucet key, amount, persistent state and window. That
listener serves only `POST /testnet/faucet`; consumer API, metrics and seed
routes return 404. Its quota is shared with the private listener. A public
edge must preserve source identity. By default forwarded-IP headers are
ignored, so callers behind one proxy share its source quota. To use a relay,
set `EMBER_FAUCET_TRUSTED_PROXY` (`--faucet-trusted-proxy`) to its immediate
source IP. Only that peer may supply one IP in `X-Forwarded-For`; the relay
must overwrite incoming headers, and the backend must remain private to it.
Account and source quotas still share the same persistent state. Direct HTTPS uses the
configured `--http-tls-cert` and `--http-tls-key`. Publish the endpoint in
your network access documentation after verifying public reachability. Choose a claim amount
at least as large as the advertised minimum stake. Spark remains valueless
test credit and comes from the dedicated account's existing balance.

## Gateway concurrency and replay deadlines

The shipped gateway uses `EMBER_MAX_CONCURRENT=8` (`--max-concurrent`),
`EMBER_AUDIT_WORKERS=2` (`--audit-workers`) and
`EMBER_AUDIT_TIMEOUT=3m` (`--audit-timeout`). Inference attempt deadlines are
configured separately with `EMBER_REQUEST_TIMEOUT` (`--request-timeout`).
Each HTTP request has its own admission slot and tunnel attempt. Durable audit
jobs run after response submission in bounded workers, with one active replay
per assigned auditor and per hold; a busy reference leaves other jobs pending.
Legacy subprocess replay deadlines terminate the process group. Restart retries
the durable spool. An HTTP completion means execution and receipt submission;
verify the hold's `settled` state before treating earnings as final. Expired
holds follow the chain refund path rather than implying successful settlement.

## Node exit and unbonding

Use the QUICKSTART exit commands with the operator-owned node identity. Stop the
service, disable its supervisor and verify its health listener is down. Let the
registration expire without renewal; a default 200000-block registration can
make this a long wait. For a planned short trial, choose a shorter
`EMBER_REGISTRATION_TTL` before first registration. Never shorten or rewrite a
live registration in a state file. Poll status, drain/expire outstanding holds,
then use `ember-node exit --action unbond` and wait for the returned unbonding
release height before `--action claim`. Keep the identity backup throughout.
A failed early claim does not consume funds. A slashed operator recovers only
the stake still present after finalized penalties. Validators have a separate
operator-coordinated consensus exit; this node procedure does not remove a
validator from the consensus set.

## Scarcity pricing and role coverage

A new network enables registered auditor routing in genesis and supplies a
`pricing` policy on each model. A reference CPU policy uses targets of two
executors and three eligible auditors, with base weights 80 executor, 15 auditor
and 5 consumer rebate. These are per-profile governance constants; legacy
models without a pricing policy retain their historical accounting.

At settlement, coverage comes from the preceding finalized block's registrations
and stake. Executor coverage counts distinct active model identities; eligible
auditor coverage excludes the producer and requires a matching replay runtime.
Expired or under-staked registrations do not count. For each role:

`multiplier = 1 + max(0, target - coverage) / target`, capped at 2.

The chain computes fixed-point multipliers in millionths, multiplies the base
weights, and renormalizes all three shares inside the actual fee. Integer
remainders go to the largest fractional shares, with executor/auditor/consumer
order breaking ties. A one-subunit fee can therefore still pay its executor.
Unused hold funds plus the consumer rebate return to the payer. No subsidy is
claimed and no credit is created. The settled hold's `shares` records amounts,
coverage, multipliers and the exact finalized signal height.

```sh
emberchain query --rpc "$RPC" --path "/model/$MODEL"
emberchain query --rpc "$RPC" --path "/coverage/$ACCOUNT"
emberchain query --rpc "$RPC" --path /coverage
emberchain query --rpc "$RPC" --path "/hold/$HOLD"
```

Coverage is registration coverage, not an uptime measurement. Going offline
or withholding heartbeats cannot raise a multiplier. Active registrations
cannot remove roles, change replay runtime, shorten their expiry or unbond
stake before TTL expiry. Lowering one's own coverage requires actual visible
withdrawals and loss of that registered earning capacity. The 2x cap and
conserved fee prevent free on/off claims or issuance from fabricated scarcity.
Operators can nevertheless choose short initial TTLs, coordinate withdrawals,
withhold service, or fund multiple identities. Distinct stake-backed identities
are not proof of independent people or hardware. Strategic withholding can be
profitable under some demand/cost conditions; this mechanism does not prove a
general incentive theorem, particularly with valueless testnet stake. Watch
expiry schedules, concentration and service failure rates before changing
production targets or admission policy.

A public network is forthcoming. The queries above report coverage for a network
you operate. Network access records will be published separately.

## Relay-only public ingress

`deploy/public-relay.py` forwards a TLS seed stream and three HTTP functions
to loopback SSH reverse forwards. It runs no Ember compute or validator.
Supply separate `--seed-port`, `--rpc-port`, `--gateway-port`, `--faucet-port`
and corresponding `--*-upstream` loopback ports, plus an explicit public
`--bind`. Reserve unused high ports; do not reuse existing service ports.
RPC permits chain query, signed transaction and verification methods only;
gateway permits completions and health checks; faucet permits its claim
path only. Metrics and administrative routes stay private. The relay
overwrites forwarded client headers. For SSH loopback forwards, configure
the backend's trusted faucet proxy as `127.0.0.1`. Keep the SSH transport
supervised; a broken forward must fail closed. No validator or worker
restart is needed when restarting a relay.

For seed TLS passthrough, the public address must appear in the backend
certificate's subject alternative names. A matching fingerprint alone does
not satisfy the gRPC client's hostname verification. When backend names are
private, use `--seed-cert` and `--seed-key` for a certificate covering the
public address, plus `--seed-upstream-ca` for the trusted backend certificate.
The relay advertises HTTP/2 ALPN and verifies backend TLS to its loopback
forward. Publish the relay certificate fingerprint in the signed network
artifact. This preserves standard client verification and existing worker
certificate pins. All three TLS bridge options are required together.
