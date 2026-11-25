# S1: chain artifact distribution and asserted JOIN

Implementation stays in the taskbook's file scope. No consensus rules,
State.Bytes encoding, VERSION, production services, funding, or production
registrations were changed by S1. Pull/rebase also incorporated concurrent D2
commits already present on remote master.

## Interfaces and trust

- `emberd ... --profile-id ember-... --chain-rpc URL --chain-id LEDGER`
  loads one isolated profile context. It excludes `--genesis/--manifest`.
  `EMBER_PROFILE_ID` selects this mode in the gateway role launcher; the file
  environment variables keep their original path otherwise.
- `ember-node configure --profile-id ember-... --chain-rpc URL
  --chain-cli emberchain --trust-policy PATH ...` fetches and verifies the
  publication or legacy signed artifacts and writes exact bytes under
  `profiles/ID/`, with SHA256 pins in the generated configuration.
  Unbundled fetch requires the explicit RPC argument as an operator trust pin;
  ordinary ABCI responses are not light-client/Merkle proofs. Existing local
  publisher/endorsement policy is applied; an RPC-provided publisher is never
  automatically installed as a trusted publisher for an unbundled profile.
- Gateway directory entries may opt into `dynamic_profiles: true` instead of
  requiring exact static profile membership. This is an operator capability
  declaration, not a promise that every profile is running on that endpoint.
  `--gateway ID_OR_DIRECTORY_URL` explicitly selects a directory entry even
  outside its static membership; private entries still require same-host opt-in.
  No existing production directory entry was newly advertised as dynamic.
- New JOIN clients assert `profile_id` and optional `genesis_sha256` in the
  first frame. A gateway serving another ID/hash fails before exposing its
  challenge. Empty legacy JOIN payloads retain the previous default path.
  Reconnect and release pins now hash the original challenge bytes.
- The ledger cache holds one entry (16 MiB combined raw artifact limit),
  serializes fetches, returns byte copies, and keys on ledger ID, profile ID,
  response height, genesis SHA256 and manifest root. Every fetch checks current
  RPC height/chain identity; a new height refreshes validation and invalidation.
  Offline queries fail explicitly rather than presenting cached state as fresh.
  Publication profile_hash means genesis protocol digest; legacy profile_hash
  means raw manifest SHA256. These paths are tested separately.

## Reproduce and retained outputs

All output files are in `s1-output/`, including initial environment failures.
The final Go invocation covers entire ledger/tunnel/daemon packages; the full
Python invocation covers the complete node suite, plus release/deploy checks.

```sh
PATH=/tmp/gateway-venv/bin:/usr/local/go/bin:$PATH go test ./services/ledger ./services/tunnel ./cmd/emberd
PATH=/tmp/gateway-venv/bin:/usr/local/go/bin:$PATH go test -race ./services/ledger ./services/tunnel
PYTHONPATH=core-py:proto:node /tmp/gateway-venv/bin/python -m pytest node -q
PYTHONPATH=core-py:proto:node /tmp/gateway-venv/bin/python -m pytest release deploy/test_public_relay.py -q
bash -n deploy/ember-role.sh
PATH=/usr/local/go/bin:$PATH go build -o .runtime/s1/emberchain ./cmd/emberchain
PATH=/usr/local/go/bin:$PATH go build -o .runtime/s1/emberd ./cmd/emberd
PYTHONPATH="$PWD/core-py:$PWD/proto:$PWD/node" /tmp/gateway-venv/bin/python services/ledger/testdata/s1_integration.py
```

The integration script starts an isolated, single-validator CometBFT ledger on
random loopback ports. Its genesis contains a signed tiny mock publication
absent from the bundle. This exercises recovery/distribution of an existing
on-chain publication, not the publisher build/submit workflow. It starts the
real emberd with `--profile-id`, runs the actual configure CLI with a pinned
RPC and publisher policy, compares recovered bytes, then runs the actual Python
JOIN client with chain enrollment and waits for JOIN READY. Finally it requests
a different profile from the same gateway and verifies rejection before any
challenge. All keys/accounts/stake are throwaway local fixtures; no external
chain is contacted. The fixture has no model weights and proves D1 material
and enrollment flow, not inference execution/readiness or D1-c routing.

`integration.log` records both PASS assertions; `integration-{chain,gateway,client}.log`
retain chain startup, registration transaction, and JOIN READY/rejection evidence.

## Concurrent D2 configuration observation

Rebuilding after the required rebase exposed an out-of-scope D2 launcher issue:
`emberchain init-validator` emits CometBFT's default config containing a quoted
`statesync.chunk_fetchers`, and the new launcher rejects it with
`invalid statesync.chunk_fetchers: strconv.ParseInt: parsing "\"4\"": invalid syntax`.
The S1 integration fixture writes a minimal `[statesync] enable = false` local
config before starting its isolated ledger. S1 does not patch cmd/emberchain.
This is separate from the D1 acceptance, which passes with that explicit config.

## Final results

- `go-rebased.log`: complete ledger, tunnel and emberd suites pass on rebased head.
- `go-race.log`: complete ledger and tunnel suites pass under the race detector.
- `python-pytest-final.log`: **499 passed, 3 skipped, 63 subtests passed**.
- `release-deploy.log`: **12 passed**; launcher `bash -n` also succeeds.
- `python-join-detail.log`: **41 passed, 3 subtests passed**, including configure
  CLI/explicit gateway, trust rejection, artifact validation and first-frame pins.
- `integration.log`: both PASS assertions from the real loopback chain/gateway/
  configure/JOIN sequence; chain enrollment transaction and READY are retained.
- `test-versions.log` records actual tool/runtime versions. These are local test
  environment results, not a runtime conformance claim for a production profile.

Initial logs intentionally remain: the first Go run used an unsuitable Python
PATH; initial Python discovery lacked pytest, and the first pytest run lacked
Diffusers. Dependencies were supplied for the final successful run. An attempted
alternative Transformers 4.45.2 download was stopped; the final tests use 4.46.3
and Diffusers 0.32.2 as specified by requirements-t2i.txt.
