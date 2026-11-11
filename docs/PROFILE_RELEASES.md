# Profile release records

Read the [disclaimer](DISCLAIMER.md) before using a release or participating.

`release/profile_pages.py` renders `site/profiles/` from publisher-signed
`genesis.pb`/`manifest.pb` artifacts and a consensus-verified chain query.
The local signed artifact sets select the release catalog; unrelated permissionless
submissions do not become official records or block this build.
No model hash, registration height, endorser, origin revision or audit band is
maintained by hand in HTML. A release record names its chain explicitly;
each record applies only to its named network. A public network is forthcoming.

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
chosen genesis validator set before any release file is rendered. Downloadable
artifacts, executable origin/recomputation and conformance commands, signed
audit parameters and current active endorsement accounts all come from these
inputs. Each machine record carries its chain snapshot height and proof summary.

The Pages workflow runs `check-profile-pages.sh` before deployment. It refreshes
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
