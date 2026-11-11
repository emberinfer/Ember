# Public network and seed operation

## Disclaimer

Ember is experimental, open-source research software provided as-is, without
warranty, subject to the [Apache-2.0 license](../LICENSE). Testnet Spark is
valueless test credit: it is not money, is not offered for sale, and is not redeemable for currency or anything else of value.
offered for sale. The Ember project operates no exchange or fiat on/off-ramp of
any kind. Nothing in this documentation is an offer, solicitation, or investment
advice. Operators and consumers participate at their own risk and subject to
their own local rules. Read the [full disclaimer](DISCLAIMER.md).

## Network operation

The network is open to independent operators contributing inference capacity;
validator admission remains static as described below.

Discovery uses finalized,
signed `ResourceProfile` registrations in `emberchain`. A seed runs the outbound
`NodeTunnel`, exposes `/seed/peers`, and relays signed scheduler work. It does not
create node identities, grant stake, choose an audit result, or mutate balances.
Any seed with the same finalized chain view can replace another seed.

## Request pricing

Profiles can commit separate input, output and context interaction prices.
The [pricing contract](PRICING.md) defines the formula, exact worst-case quote
API, tokenization commitments, consensus checks and refunds. Existing profiles
with zero input/context prices retain their output-only amounts; new pricing
requires a new signed profile and compatible validators and serving software.

## Trust and authority

- Validators remain the authority for accounts, stake, registrations, holds,
  audit assignment and settlement. The less-than-one-third Byzantine voting
  power assumption remains; validator admission is still static genesis policy.
- Seeds and gateways can censor, delay or observe prompts. They cannot forge a
  node profile, scheduler frame, stage request, receipt, audit verdict or chain
  transition. Consumers still use a gateway-custodial bearer-to-account-key map.
- Nodes pin the published genesis and manifest SHA-256 values. A seed presenting
  different bytes is rejected before model load or registration. Operators should
  configure multiple seeds; one unavailable seed remains an availability risk for
  a node configured with only that seed.
- Audit assignment is derived after receipt commitment from the prior finalized
  CometBFT block hash. A reject needs confirmation by a different assigned auditor;
  disagreement cannot slash. Cross-runtime verdicts remain observe-only.
- TLS hides traffic from passive network observers. Gateways, executors and
  auditors still see plaintext needed for their roles; Ember does not claim
  confidential inference.

## Node enrollment

A public network is forthcoming. No access endpoints or network snapshot are
included. For a network you operate, provision funded identities and reviewed
genesis/manifest artifacts. A join supplies the selected seed, RPC, ledger chain
ID, hashes, and authorized scheduler account. The following uses placeholders:

```bash
ember-node join \
  --state-dir /var/lib/ember/node \
  --coordinator seed.example.org:19091 \
  --tls-fingerprint SEED_CERT_SHA256 \
  --chain ember-qwen2.5-1.5b \
  --engine transformers --model-path /opt/models/qwen \
  --scheduler-id ON_CHAIN_SCHEDULER_ACCOUNT \
  --chain-rpc https://seed.example.org:26657 \
  --chain-cli /usr/local/bin/emberchain \
  --ledger-chain-id ember-testnet \
  --genesis-sha256 PUBLISHED_GENESIS_SHA256 \
  --manifest-sha256 PUBLISHED_MANIFEST_SHA256
```

The tunnel is outbound-only, so the executor needs no inbound public port. Signed
work-frame nonce watermarks persist under the node state directory with mode 0600.
Registrations expire by height and must be renewed; expired or under-staked nodes
cannot receive a new hold.

## Abuse controls

`emberd --rate-limit` applies a fixed-window quota per authenticated consumer
identity. Body size, token count, global concurrency, attempt timeout and stage
tensor size remain bounded. Holds have a chain-height expiry; any account can
submit `expire_hold`, and chain gateways periodically do so. Account transaction
nonces, persistent scheduler nonces, request/attempt IDs and append-only receipts
provide replay fencing across restarts.

Shard RPC no longer accepts a shared cluster token. Every request is signed by an
authorized scheduler identity and carries a persistent replay nonce; the stage TLS
certificate fingerprint and stage signer are bound in its chain-registered profile.

## Verification status

Operators can verify public onboarding by completing the [Quickstart](QUICKSTART.md)
with a fresh identity, checking a finalized settled hold, and following the exit
procedure through stake recovery. Release metadata and signed profile artifacts
are described in [profile releases](PROFILE_RELEASES.md). Contract tests in
[node/test_network.py](../node/test_network.py) and
[services/chain/routes_test.go](../services/chain/routes_test.go) cover discovery
and assignment behavior; they do not establish NAT reachability or independent
operator ownership. Public endpoint availability must be checked at deployment.
