# Publish a model profile

Publication is permissionless and locks the bond reported by `/publishing`.
A model identity has one publication slot: confirm the pinned source, artifact
bytes and measured audit policy before submitting. Testnet credit has no value.

Use a matching release's `ember-node` and `emberchain`. Download the complete
model at a full Git commit revision. Supply a measured NumericAuditPolicy JSON
with valid actual layer indices, calibration_ref, top_k, metric and runtime/device
bands. There is no default calibration: conformance is not calibration. A CPU
policy cannot authorize a CUDA runtime. Unsupported architectures or compute
layouts fail before submission; this command builds original whole-model
safetensors profiles, not derived shards or MoE serving layouts.

Set these values to your local paths and chosen publication identity:

```sh
MODEL="$HOME/models/Qwen2.5-0.5B-Instruct"
POLICY="$HOME/calibration/policy.json"
PUBLISHER_STATE="$HOME/.ember-publisher"
OUTPUT="$HOME/ember-publication"
MODEL_ID=ember-my-model-v1
EXECUTION_PROFILE=transformers-my-model-v1
ORIGIN=https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct
REVISION=7ae557604adf67be50417f59c2c2f167def9a775
RPC=http://relay.emberinfer.org:39657
LEDGER_CHAIN_ID=ember-testnet-1
ember-node id --state-dir "$PUBLISHER_STATE"
ember-node profile publish --model-path "$MODEL" --policy "$POLICY" \
  --state-dir "$PUBLISHER_STATE" --output "$OUTPUT" --model-id "$MODEL_ID" \
  --profile-version "$EXECUTION_PROFILE" --origin "$ORIGIN" --revision "$REVISION" \
  --chain-rpc "$RPC" --ledger-chain-id "$LEDGER_CHAIN_ID" --device cpu --dry-run
```

Review the output, query `/publishing`, and fund your publisher account through
the published testnet faucet. Run the same command without `--dry-run` to submit.
The command builds with the existing genesis/audit builders, hashes and verifies
every artifact, derives compute from real tensor shapes through the chain tool,
signs with the publisher identity, checks runtime policy and runs generation plus
forced-prefix replay conformance. Construction failures discard temporary output.
A successful dry run preserves the prepared output and does not submit a tx.

The submit path checks the ledger ID and balance, waits for CheckTx and committed
TxResult success, then reads `/model/ID`. It requires a positive registered_height,
the expected publisher, both exact original protobuf byte strings, genesis raw
SHA256, profile digest and manifest root. Protocol digests retain the signer and
clear the signature. Keep the prepared output and publisher identity private and
back them up. Repeating against an identical committed ID only reads back; it
never pays another bond. Different bytes or publisher fail closed. A lost local
output is not permission to republish: recover the original artifacts first.

If submission times out, the output's `submission-pending` marker blocks automatic
rebroadcast. Resolve the tx and committed model/account state using the trusted
RPC before removing that marker. Do not choose a new ID to evade uniqueness.
The lower-level `profile prepare`, `verify` and `submit` commands remain available
for controlled workflows; `prepare` now includes the required tensor compute
facts. `--previous-profile-hash` provides lineage only; it does not bypass model
identity uniqueness.

After publication, the public gateway supervisor automatically discovers valid
publications and eligible supply; no manual gateway admission is required.
Availability still requires a supported runtime/layout and reachable, qualified
whole-model supply or complete pipeline coverage. Inspect `/v1/catalog` for
ready/reason and the website's live model catalog. Zero executors, invalid
publications or unsupported contexts remain unavailable. Nodes retain their own
publisher trust decisions.

Report the fixed revision, model ID, tx hash, registered height, publisher,
genesis SHA256, manifest root/profile digest, chain-derived model identity and
compute, bond, conformance result and gateway readiness reason. Numerical audit
bands require measured cross-runtime calibration before production use.

For endorsement, challenges, bond release and the provenance threat model, see
`services/chain/publishing.go` and the `ember-node profile` subcommands. Claiming
the bond uses the same identity and the chain's release-height/settled-request
conditions. Never assume checksums alone authenticate a software release.
