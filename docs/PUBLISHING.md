# Publish and endorse an Ember profile

Publishing is permissionless on chains running the publishing state machine.
Use a chain whose validators support the publishing state machine.
Endorsements express local trust;
they do not activate execution or change consensus membership.

Start with signed template genesis/manifest artifacts and complete local origin
weights downloaded from an immutable Git revision. Keep your publisher identity
in a private state directory. The profile command creates it with mode 0600.
Use your own funded account; a governor key is unnecessary.

```bash
ember-node profile prepare --state-dir /secure/publisher \
  --genesis template/genesis.pb --manifest template/manifest.pb \
  --model-path /opt/model --model-id ember-example-cpu-v2 \
  --profile-version transformers-example-cpu-v2 \
  --origin https://huggingface.co/OWNER/MODEL --revision GIT_COMMIT_ID \
  --output ./release
emberchain query --rpc "$RPC" --path /publishing
ember-node profile submit --state-dir /secure/publisher \
  --chain-rpc "$RPC" --ledger-chain-id "$CHAIN_ID" \
  --action-file ./release/registration.json
```

`prepare` verifies every local artifact, pins each origin path, changes the model
and execution profile identifiers, and signs both protobuf artifacts using your
publisher key. `submit` checks the ledger ID and submits a signed account
transaction. Consensus recomputes the manifest digest and profile digest and
verifies both signatures, artifact paths, config/tokenizer bindings and hashes.
A model ID is immutable. To release a successor, choose a new ID and supply
`--previous-profile-hash` with the prior chain profile hash to `prepare`.

The genesis anchor is SHA-256 over the complete signed `genesis.pb` bytes.
For new publications the profile hash is `ember_core.digest(ChainGenesis)`;
the manifest root is `ember_core.digest(ModelManifest)`. These domain-separated
digests clear only the protobuf signature field and retain the signer. Historical
registrations retain their original hash conventions.

The `/publishing` query exposes the bond, challenge window, required settled
requests and minimum blocks between submissions by one account. A rejected
transaction changes neither balance nor nonce. A successful registration locks
the bond separately from serving stake. Only the publisher can claim it after
the first N accepted settlements or after the challenge window, whichever comes
first. Use `ember-node profile claim-bond` with the same state directory and
`--model-id`, `--chain-rpc` and `--ledger-chain-id`.

An origin challenge includes the raw pinned Git commit, the binary Git trees
along one artifact path and the original Git blob (an LFS pointer for large
weights). Consensus recomputes Git object IDs from the pinned revision down to
that blob, then compares its SHA-256 and byte size to the manifest declaration.
A proven mismatch invalidates the publication and forfeits its remaining bond
into a chain-visible forfeiture counter. There is no challenger reward or
external network access in consensus. Arbitrary downloaded bytes cannot prove
fraud: their Git object ID must authenticate to the pinned commit. The proof is
bounded by the transaction size limit. `ember-node profile challenge` accepts
an action file with `type: challenge_publication`, `model_id` and `origin_proof`
(`artifact`, base64 `commit`, list of base64 `trees`, base64 `blob`).

Limits: Git SHA-1 object security is inherited from the chosen origin. No claim
is made that the origin author or model is trustworthy. Challenges currently
cover artifact provenance/hash/size contradictions, not model quality, licenses,
numerical calibration or arbitrary execution bugs. Early release after N
settlements closes financial recourse once the bond is returned. Forfeiture is
accounted separately from circulating balances; serving stakes are unaffected.
Derived shard artifacts require their own reproducible build binding and are
not produced by this original-artifact profile command.

## Endorsements and serving policy

Any funded account can endorse a published manifest root and revoke its own
endorsement. The chain account transaction signs the endorsement; the transaction
history retains its signature, chain ID and nonce. `/endorsements` and `/state`
return the current distinct account list, heights and revocation flags.

```bash
ember-node profile endorse --state-dir /secure/endorser \
  --chain-rpc "$RPC" --ledger-chain-id "$CHAIN_ID" --manifest-root "$ROOT"
ember-node profile revoke --state-dir /secure/endorser \
  --chain-rpc "$RPC" --ledger-chain-id "$CHAIN_ID" --manifest-root "$ROOT"
```

Gateway `--trust-policy` and node join `--trust-policy` accept the same JSON file;
role templates expose `EMBER_PROFILE_TRUST_POLICY`. For example:

```json
{"profiles":{"ember-example-cpu-v2":{"min_endorsers":2,"publishers":[],"trusted_endorsers":[]}}}
```

For each profile the policy accepts a publisher in `publishers`, OR at least
`min_endorsers` distinct active endorsements (only positive thresholds enable
this rule), OR any active endorsement from `trusted_endorsers`. The default
accepts historical registered anchors and requires a governor endorsement for
new publications. Governors use the same endorsement transactions as everyone
else. A proven-invalid publication is always denied. The gateway refreshes
finalized trust before admitting each request; nodes refresh before execution
and audit. Revocation does not cancel requests already admitted. Account counts
are not Sybil resistance; pick trusted endorsers when that matters.

Verify a release from the downloaded origin files, including a short executable
adapter conformance probe when requested:

```bash
ember-node profile verify --genesis ./release/genesis.pb \
  --manifest ./release/manifest.pb --model-path /opt/model \
  --conformance --device cpu
```

This checks artifact bytes and signed roots, then generation and forced-prefix
replay. It is a conformance probe, not cross-hardware numerical calibration.
