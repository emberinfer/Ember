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

New staging networks set `registered_auditors: true` in chain genesis parameters.
Auditors join with `ember-node join --audit-role` and the same pinned profile as
executors. The chain selects independent, active, stake-backed identities with
full forced-prefix replay capabilities and a matching runtime. The gateway
resolves the assigned identity over its authenticated outbound tunnel, verifies
its signed audit transaction, and broadcasts it. New executor registrations
require no audit-map edits. Offline assigned auditors remain a liveness risk;
hold expiry refunds the consumer. Legacy genesis can keep its historical
authority set with `--chain-audit-routing=false` during coordinated migration.
Changing consensus eligibility requires a fresh staging genesis or a separately
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
`network.json` after verifying public reachability. Choose a claim amount
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
make this a long wait. For a planned short staging trial, choose a shorter
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
`pricing` policy on each model. The staging CPU profile uses targets of two
executors and three eligible auditors, with base weights 80 executor, 15 auditor
and 5 consumer rebate. These are per-profile governance constants; legacy
models without a pricing policy retain their historical accounting.

Coverage counts distinct active, stake-backed registrations matching the profile.
Auditor coverage additionally excludes the producer and requires a matching replay
runtime. Expired or under-staked registrations do not count. Coverage counts are
observational: they no longer multiply settlement role weights.

The per-profile consumer price changes at block epoch boundaries using accepted
settlements and effective executor coverage. The default multiplier corridor is
0.8 to 2.0, with at most a 10% relative change per epoch. Gross whole-model shares
remain 80/15/5, with largest-remainder rounding and executor/auditor/consumer tie
order. The consumer funds the entire gross fee; no subsidy is claimed. Holds
record the frozen multiplier and its effective height under `token_fee`, and
`shares` records the gross amounts and coverage snapshot. See
[Pricing](PRICING.md#supply-and-demand-consumer-price) for controller arithmetic.

```sh
emberchain query --rpc "$RPC" --path "/model/$MODEL"
emberchain query --rpc "$RPC" --path "/price/$MODEL"
emberchain query --rpc "$RPC" --path "/coverage/$ACCOUNT"
emberchain query --rpc "$RPC" --path /coverage
emberchain query --rpc "$RPC" --path "/hold/$HOLD"
```

Coverage is registration coverage, not an uptime measurement. Going offline
or withholding heartbeats cannot change the effective supply count. Active registrations
cannot remove roles, change replay runtime, shorten their expiry or unbond
stake before TTL expiry. Lowering one's own coverage requires actual visible
withdrawals and loss of that registered earning capacity. The price corridor bounds consumer fees; all payouts remain funded by locked
consumer funds.
Operators can nevertheless choose short initial TTLs, coordinate withdrawals,
withhold service, or fund multiple identities. Distinct stake-backed identities
are not proof of independent people or hardware. Strategic withholding can be
profitable under some demand/cost conditions; this mechanism does not prove a
general incentive theorem, particularly with valueless testnet stake. Watch
expiry schedules, concentration and service failure rates before changing
production targets or admission policy.

The live public testnet publishes a verified coverage snapshot/current multipliers
in `site/network.json` alongside its complete public access parameters and the
identical signed release artifact. The chain queries above provide current
coverage; the website is a release snapshot. Publishing v0.22.0 changes labels
and documentation without replacing the running network's binaries or genesis.


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

## Exit window relationships

All consensus windows below use finalized block heights. Values are genesis
inputs, not wall-clock promises; query `/state` for the network's parameters.
The node's requested registration TTL defaults to 200000 blocks and must fit the
chain's configured maximum. Library defaults are smaller than operator examples.

| Window | Value or relationship | Source |
| --- | --- | --- |
| Registration maximum | default 100; minimum 2 blocks | `services/chain/state.go:259`, `services/chain/state.go:265` |
| Node requested registration | default 200000 blocks | `node/agent/__main__.py:61` |
| Published profile ledger snapshot | U=3, D=1, H=500, registration maximum=2000, publication challenge=300 blocks | `site/profiles/ember-qwen2.5-1.5b-publisher-cpu-v1/ledger-genesis.json:1` |
| Consensus validator evidence | maximum age 100000 blocks and 172800 seconds; distinct from application request audit | `site/profiles/ember-qwen2.5-1.5b-publisher-cpu-v1/ledger-genesis.json:1` |
| Unbonding delay U | explicit genesis value, minimum 1; GPU example 3 | `services/chain/state.go:261`, `ops/f5-gpu-stage.py:115` |
| Receipt challenge D | explicit genesis value, minimum 0; GPU example 1 | `services/chain/state.go:262`, `ops/f5-gpu-stage.py:116` |
| Hold timeout H | default 20; minimum 2; GPU example 30000 | `services/chain/state.go:256`, `services/chain/state.go:265`, `ops/f5-gpu-stage.py:117` |
| Registration GPU example | maximum 100000 blocks | `ops/f5-gpu-stage.py:117` |
| Request expiry E | freeze height + H | `services/chain/state.go:571` |
| Receipt challenge close C | submit height + D | `services/chain/state.go:625`; `services/chain/privacy.go:230` |
| Exit liability bound L | maximum of E and C across retained requests and pipeline authorizations | `services/chain/exit_guard.go:7` |
| Stake release | unbond height + U, with unbond height >= L and no active exposure; claim rechecks L | `services/chain/state.go:520`, `services/chain/exit_guard.go:7` |
| Assignment entropy | default 3 future blocks; allowed 1–64 | `services/chain/state.go:230`, `services/chain/state.go:235` |
| Assignment pair cooldown | default 8 requests, not blocks | `services/chain/state.go:233` |
| Gray repeat window | default 1000 blocks; repeat limit 3; observation age < window (strict) | `services/chain/misjudge.go:42`, `services/chain/misjudge.go:45`, `services/chain/misjudge.go:169` |
| Consumer gray window | `misjudge.consumer_gray.window_blocks=1000`; settled sample age < window (strict) | `services/chain/consumer_gray.go` |
| Consumer global trigger | `global_min_samples=20`, `global_gray_rate_bps=5000`; samples >= 20 and gray rate >= 50% | `services/chain/consumer_gray.go` |
| Consumer pair trigger | `pair_min_samples=10`, `pair_min_gray=5`, `pair_gray_rate_bps=5000`; all three inclusive conditions required | `services/chain/consumer_gray.go` |
| Slash repeat history | default 1000 blocks | `services/chain/state.go:243` |
| Audit work epoch | default 1000 blocks | `services/chain/state.go:221` |
| Publication challenge | default 100; minimum 2 blocks; separate publisher bond | `services/chain/publishing.go:30`, `services/chain/publishing.go:40` |
| Legacy profile dispute hint | 60 seconds; does not override consensus D | `chain-genesis/build.py:82` |
| Gateway replay deadline | default 3 minutes; transport timeout, not a stake window | `cmd/emberd/main.go:109` |
| Signed resource advertisement | 120 seconds; distinct from chain registration | `node/agent/join.py:129` |
| Requested registration clamp | min(node requested TTL, chain maximum) | `node/agent/join.py:206` |
| Gateway inference deadline | default 5 minutes; transport timeout | `cmd/emberd/main.go:111` |
| Evidence fetch timeout | 30 seconds; transport failure becomes service failure | `node/auditor/worker.py:84` |
| Publication submission interval | default 10 blocks; separate from node exit | `services/chain/publishing.go:40` |
| Gray pending review | no automatic adjudication deadline or extra replay timer | `services/chain/misjudge.go:203` |
| Node generation deadline | default 300 seconds; transport timeout | `node/agent/__main__.py:45` |

There is no independent post-settlement challenge transaction or extra evidence
challenge timer. An unresolved hold remains an exposure until settlement or
expiry finalizes, even after its nominal challenge height. Early refunds retain
the exit bound. In particular, U >= D alone is insufficient when the last
receipt arrives near expiry. The conservative bound enforces release >= L >= C
without changing fees, penalties or audit compensation. Stake is not allocated
by closed request, so exits wait for all windows rather than estimating a
releasable fraction. This is application version 5; version 4 state requires a
coordinated upgrade or a fresh genesis before running the new consensus binary.

## Consumer gray payment policy

Governors update the nested `misjudge.consumer_gray` values through the existing
signed `set_misjudge` transaction with the complete valid misjudge policy.
Global or pair trigger enables normal charging for that account's subsequent
gray/pending-review requests; global scope takes priority. The hold records the
charge decision, scope and all thresholds at freeze, based only on previously
settled non-pipeline audited requests. Parameter updates affect new freezes.
An occasional gray outcome, or a legacy hold without a snapshot, still receives
a full refund. See [consumer gray governance](AUDIT_DESIGN.md#consumer-gray-governance)
for sample exclusions, exact conditions and the unchanged payment path.
`/state` exposes `consumer_samples` keyed by hold ID; `/hold/<id>` exposes the
`consumer_gray` snapshot, original audit verdict, shares and any audit escrow.
Samples are retained as explicit history and scanned by window; state growth and
scan cost follow retained history, with no automatic pruning in this change.

## Content-policy operation

Nodes accept content decisions only from their authenticated profile manifest;
there is no local content-filter flag or environment setting. See
[profile content policies](PROFILE_RELEASES.md#profile-content-policies) for the
versioned evaluator and refusal evidence. A refusal is final only after the
assigned auditor accepts it and chain settlement completes. Check the signed
receipt category and audit verdict instead of inferring failure from refusal
text. A profile without a declaration has no policy exemption from ordinary
replay. Do not independently enable moderation on an external inference backend.
