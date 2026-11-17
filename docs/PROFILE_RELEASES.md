# Profile release records

Read the [disclaimer](DISCLAIMER.md) before using a release or participating.

`release/profile_pages.py` renders `site/profiles/` from publisher-signed
`genesis.pb`/`manifest.pb` artifacts and a consensus-verified chain query.
The local signed artifact sets select the release catalog; unrelated permissionless
submissions do not become official records or block this build.
No model hash, registration height, endorser, origin revision or audit band is
maintained by hand in HTML. A release record names its chain explicitly;
staging records do not change the live public testnet profile.

Use the runtime pinned for the profile. Place signed artifacts under
`chain-genesis/MODEL_ID/`. Obtain the ledger genesis through the chain's
advertised public path and verify it against your trusted network parameters.
The chain CLI requires the verification anchor file to have mode 0600.

```bash
chmod 600 "$LEDGER_GENESIS"
python release/profile_pages.py --rpc "$RPC" --chain-cli emberchain \
  --ledger-genesis "$LEDGER_GENESIS" --artifacts chain-genesis \
  --output site/profiles
PROFILE_CHAIN_CLI=emberchain bash release/check-profile-pages.sh
```

Generation verifies both publisher signatures, publisher identity, exact chain
artifact bytes, raw genesis SHA-256, domain-separated profile/manifest digests,
chain ID and version links. It rejects cycles and missing predecessor anchors.
The chain CLI verifies the whole-state hash against a CometBFT commit and the
chosen genesis Ledger node set before any release file is rendered. Downloadable
artifacts, executable origin/recomputation and conformance commands, signed
audit parameters and current active endorsement accounts all come from these
inputs. Each machine record carries its chain snapshot height and proof summary.

The repository supplies `release/check-profile-pages.sh` for verification, but the current public tree has no Pages deployment workflow. The live emberinfer.org website is built from a separate repository. When run, the script refreshes
verified chain state, compares every release fact (including endorsements) to
that state, and confirms HTML is exactly the rendering of the machine record.
It also checks downloadable artifacts and the ledger anchor bytes. A changed
endorsement set requires regeneration; network failure or disagreement fails
the deployment. Snapshot height itself may advance without invalidating
otherwise identical facts. The public RPC for every recorded chain must remain
available while its record participates in this deployment gate.

These are build-time endorsement snapshots. The page includes commands for
current finalized state and revocations; no stale snapshot is labeled live.
An initial release has one history entry with no predecessor. A successor's
`previous_profile_hash` comes from its signed on-chain submission, as described
in [PUBLISHING.md](PUBLISHING.md). A link expresses publisher-declared history,
not endorsement by the predecessor's publisher or the project.

The executable conformance probe verifies artifact bytes, signatures, roots,
short generation and forced-prefix replay. It does not certify model quality
or extend a tolerance band's hardware/runtime calibration. Serving policy is
local, and publication or an endorsement does not activate execution by itself.

## Profile content policies

Content filtering is a property of a signed model profile. Node CLI options,
environment variables and local configuration cannot enable a content filter or
override a profile policy. Models may naturally produce refusals without a
policy; these remain ordinary outputs under the existing numerical audit rules.

`ModelManifest.content_policy` optionally pins `id`, `version`,
`denied_prompt_sha256`, `refusal_tokens` and `refusal_text` inside the manifest
root referenced by the signed genesis. An absent declaration means no content
policy. A present declaration must be complete; unknown identifiers and versions,
malformed hashes and duplicate hashes are rejected during model publication,
genesis validation and node authentication. Changing any declaration changes the
manifest root and requires a new immutable profile.

The supported evaluator is `prompt-sha256-denylist`, version `1`. It compares
SHA-256 of the exact UTF-8 prompt, without normalization, against the manifest's
32-byte hashes. A match emits the declared nonempty refusal token sequence and
text; a nonmatch executes the model normally. Publishers must provide tokens
corresponding to the displayed refusal under the profile tokenizer. If a
request's output limit cannot fit the entire refusal, execution fails rather
than silently truncating the policy response. This exact-match evaluator does
not claim to classify paraphrases or provide a general semantic safety policy.

The receipt signs `output_category="policy-refusal"`. The auditor checks the
signature, chunk roots and output commitment first, then independently evaluates
the same manifest policy and checks the complete declared output. A compliant
refusal yields `accept`, distance zero and no offense grade, without applying
numerical logit/layer comparison to this output category. A forged or unwarranted
category fails. Ordinary outputs retain their existing evidence and numerical
checks. Content refusals do no forced-prefix model replay, so cited replay output
is zero; existing compensation formulas and fee splits still apply unchanged.

Publishers can supply policy JSON to `chain-genesis/build.py --content-policy`.
This is a profile construction input, not an operator filtering switch. Hashes
use protobuf JSON's base64 bytes representation. Both the manifest and genesis
must retain their signatures and matching signer. The supported category uses
whole-model plaintext receipts; policy-bearing shard manifests and encrypted
request admission are rejected until those protocols support the category.
External inference backends must implement the pinned execution behavior; a
backend's independently configured moderation cannot authorize an audit pass.

## Current catalog

A model is one set of weights; a profile is a deployment form. Forms are **Whole model** and **Sharded** (by layer, by expert, or layer + expert). A unified profile covers whole-model and sharded forms under one card. CPU/GPU describes a node, not a form. The 1.5B `-segmented` listing is the unified profile's sharded entry point, not another profile.

| Profile | Model | Context tokens | Availability |
| --- | --- | ---: | --- |
| `ember-qwen2.5-1.5b-unified-v1` | Qwen2.5-1.5B-Instruct | 32768 | Serving; unified Whole model and Sharded forms |
| `ember-qwen3.6-35b-a3b-fp8-unified-v1` | Qwen3.6-35B-A3B FP8 | 262144 | Serving via vLLM on GPU nodes |
| `ember-olmoe-1b-7b-moe-v1` | OLMoE-1B-7B | 4096 | Serving; Sharded by layer + expert; sampled segment-replay audits pass |
| `ember-kimi-k3.1-pro-v1` | Kimi K3.1 Pro | Not specified | Published on-chain; not deployed; no serving nodes |

Historical generated records remain immutable and must not be treated as a live serving directory.
