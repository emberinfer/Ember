# Privacy and encrypted requests

The encrypted API is `POST /v2/requests`. The existing OpenAI-compatible
`/v1/chat/completions` API accepts plaintext and has no end-to-end privacy
property. Do not submit secrets through that legacy API expecting these protections.

## Visibility and tiers

| Participant | P0 encrypted request |
| --- | --- |
| Consumer | Original prompt, output, local redaction map and request key |
| Gateway and public relay | Ciphertext, public envelope metadata and traffic timing |
| Storage and unassigned nodes | Encrypted bundles and their sizes; no request key |
| Chain observers | Request/model/operator identifiers, tier, epoch, envelope hash, Merkle root, verdict, tolerance-stats hash and policy/release/deletion records |
| Individual custodians | One encrypted Shamir share per request; no prompt, output or evidence data |
| Threshold of custodians colluding | Can reconstruct the request key; threshold escrow assumes fewer than the threshold collude |
| Executor operator | Plaintext prompt, output, evidence and request key in process |
| Assigned auditor operator | Plaintext prompt and evidence after authorized threshold release |

`P1` requires attested execution. No P1 executor or TEE is implemented.
P1 requests fail with `TIER_UNAVAILABLE`; there is no P0 fallback. The client
verifies the executor's signature, request/envelope binding and honored tier in
the receipt. A P0 receipt cannot satisfy a P1 request.

Phase 0 does not hide plaintext from the executor operator or the assigned
auditor operator. Retention by either is undetectable. It does not prevent a
malicious authorized operator from copying or disclosing plaintext. Deletion
is best-effort policy, not a cryptographic guarantee. Metadata, sizes within
buckets, timing, endpoint identity, inference structure and semantic content
can leak information. These protections do not provide anonymity.

## Envelope, keys and migration

The consumer SDK in [the privacy package](../node/privacy/envelope.py) creates a
fresh random 256-bit request key. AES-256-GCM with HKDF domain separation protects
the prompt, output and evidence. X25519 ephemeral key agreement wraps the request
key to the registered executor and wraps Shamir shares to registered custodians.
The default escrow is 3-of-5; threshold and members are registered in a custodian
epoch on chain. Custodians hold shares only and do not receive data bundles.

Envelope v2 contains the tier, model identifier, ciphertext blob hash reference,
wrapped executor key and custodian shares, session nonce, dispute-consent flag
and arbitration public key, evidence policy and padding profile. Its consumer
signature binds all of these fields. The transport carries the encrypted blob
and zero padding. The published `buckets-v1` boundaries in bytes are:
4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288 and 1048576.
The complete client-to-gateway JSON body occupies one bucket; oversized bodies
are refused. Output and bundle sizes are not padded by this profile.

The consumer can migrate a requested or checkpointed request under the same id.
It submits `migrate` with a fresh migration id and a registered target executor,
then `rewrap` with K_r encrypted to the target's snapshotted registered X25519
key. The wrapper's authenticated context binds the request id, original envelope
hash, migration id, source, target and target key. The consumer signs both chain
transactions; wrong targets and stale/repeated rewraps are refused. The original
envelope hash and custodian shares remain unchanged. The receiving executor checks
the finalized migration before unwrapping. Registered model, stake and exposure
checks apply to the receiving executor, and the existing fee hold follows it.

An executor can checkpoint encrypted evidence before migration. The receiving
executor obtains those bundles using signed bundle access, regenerates and checks
the greedy prefix, preserves its exact salted openings and continues generation.
This recomputes the prefix; it does not transfer a KV cache. The final root covers
all tokens across the boundary. The final executor accepts responsibility for the
full committed output; settlement credits and any confirmed-cheating slash apply
to that executor. The assigned auditor checks every checkpoint's prefix against
the full output and decrypts both old and new bundles through the unchanged escrow.

After migration finalizes, chain/API checks refuse the old executor execution,
key unwrap, tokenization, escrow release and evidence-bundle access. Retired
executors are excluded from subsequent auditor selection for that request. Public
unauthenticated bundle retrieval is disabled for migrated requests; signed access
is limited to the consumer, current executor and currently assigned auditor.
An operator invokes `Executor.retire` to delete its local bundles and log
`migration_deleted` or `migration_delete_failed`. This is best-effort disposal.
Rewrapping a previously disclosed key cannot revoke the old executor's offline
copy of that key or an original wrapper it retained. No test or API refusal
establishes offline revocation; per-request keys protect other requests.

## Evidence and escrow

The executor commits a Merkle root over independently salted per-token top-16
raw-logit openings. Activation openings needed for numerical replay remain inside
the encrypted bundle. Full vectors are not posted on chain. Before migration and
finality, anyone can fetch bundle ciphertext; only request-key holders can decrypt
it. Migrated requests require signed bundle access as described above. Each token salt is
256 random bits. Merkle proofs authenticate bounded token windows.

After on-chain audit assignment, each custodian verifies the
request id, epoch, finalized assignment proof and an Ed25519 signature from the
assigned auditor's registered identity. Release is encrypted to that auditor's
registered X25519 key. Release and refusal events include the epoch id and are
submitted on chain before a share is returned. A release fails if its event
cannot be committed. Unauthenticated or unavailable requests get a generic refusal;
if the chain itself is unavailable, recording that refusal is also unavailable.

The auditor decrypts and performs forced-prefix replay through the existing
Engine SPI and rank/probability numerical comparator. The signed envelope pins
top-16 evidence, sampled layers and tolerance. The chain records the verdict and
a hash of private tolerance statistics. Each encrypted request uses a fee hold
with the same request id. Registration freezes the pricing-v2 maximum fee using
the serving executor's signed input-token commitment. Execution and audit check
the exact input count; completion verifies input/output/context fee terms through
the common ledger path. A model must have an anchored pricing profile and an
80/15/5 executor/auditor/consumer policy. Settlement uses the same scarcity
multipliers, integer allocation and unused-budget refund as legacy requests.

A coordinator triggers assignment in a block after the output commitment; it
cannot name an auditor. Selection uses the common block-entropy algorithm over
eligible independent auditors with registered wrapping keys. An adverse verdict
requires the same second independent confirmation as legacy audits before the
common stake floor and repeat-offense slashing ladder applies. Inconclusive or
unconfirmed rejection is a service failure. `finalize` settles the common hold
after the dispute window and marks the privacy record final. Escrow release and
refusal events, checkpoint/migration records, audit-statistics hashes and the hold
all reference that request id. `cancel` and timeout `expire` refund through the
common hold path; they do not establish accepted work.

Challenge–reveal requires the consumer's pre-consented arbitration key in the
signed envelope. The helper encrypts only the selected token window, salts and
Merkle authentication paths to that key. It does not include the prompt, other
token openings or their salts. Authentication paths reveal sibling hashes.
There is no automatic dispute adjudication or enforcement service for this API.

After finality, an operator invokes bundle deletion, then commits a deletion
policy event. Epoch end requires final requests; custodians delete their old
share files and commit destruction policy events. These actions remove local
files only, do not securely erase backups or RAM, and cannot detect retained
copies. A crash between local deletion and event commit requires operator
reconciliation; the event is not proof of physical erasure.

## Operating the encrypted API

Register each operator's signing identity, X25519 wrapping key and machine-readable
policy using the chain `privacy` action. A policy includes `retention_seconds`,
`deletion_practice` and `evidence_handling`; the signed registration transaction
lets consumers compare these statements mechanically. These are operator claims,
not independently verified retention guarantees. Governors register epochs;
consumers register request/envelope hashes with `amount` and `pricing_commitment`
returned by the executor's encrypted `POST /v2/tokenize`; executors post commitments;
coordinators trigger entropy-based assignment. See [consensus actions](../services/chain/privacy.go).

Run `python -m privacy.server executor --help` or `custodian --help` with
`node`, `core-py` and `proto` on `PYTHONPATH`. Each service uses genesis-verified
chain queries. Provision signing and wrapping keys with restrictive file
permissions. Protect public ingress with TLS and access controls. Set
`emberd privacy-gateway --executor https://executor.example/v2/requests` and
point the public relay's gateway upstream to that gateway. The proxy holds no
request keys. Its optional `--capture-dir` records ciphertext request/response
bodies for inspection. The normal gateway also has an injectable privacy handler;
its legacy inference hooks are never used by the encrypted proxy.

The executor role also exposes `POST /v2/resume` on authenticated operator ingress.
Its JSON body contains base64 `transport`, optional `checkpoint_tokens`, and a
`prior_ciphertexts` array in chain checkpoint order. `POST /bundles/access` accepts
a signed `privacy.migration.bundle_request`; it returns base64 ciphertext after
checking current chain authorization. The gateway/relay route remains
`/v2/requests`; tokenization, checkpoint/resume and bundle orchestration use the
operator endpoints. The consumer library's `privacy.migration.migrate` performs
the signed migration/re-wrap handshake. No request key is returned by an HTTP API.

The Python library exposes request construction, share deposit/release,
receipt verification, bundle replay, challenge–reveal and policy deletion.
It does not provide an automated consumer CLI or a distributed custodian
provisioning controller. Operator orchestration must perform these steps and
retain the signed envelope locally for replay and disputes.

## Redaction beta

[Local detectors](../node/privacy/redaction.py) cover credential patterns, PEM
private keys, email addresses, selected phone formats and labelled account/ID
numbers. Redact before envelope creation. The replacement map remains on the
consumer and must never enter requests, logs or telemetry. Rehydration performs
a single local substitution pass. Model rewriting of placeholders can prevent
output rehydration; arbitrary model output cannot be promised lossless.

The [golden set](../testdata/privacy/redaction-golden.json) includes positive,
negative and unsupported formats. Measurement uses exact span and class matches.
It is a small synthetic benchmark, not a real-world coverage guarantee.
Residual classes are detector misses, structure leakage, output-side leakage and
semantic content. In particular compact/international phone formats, obfuscated
addresses, unlabelled identifiers, non-PEM keys and prose credentials can escape
detection. A public example resembling a credential can be a false positive.

## Canary telemetry and claim discipline

`privacy.canary.plant` appends a random consumer marker and records a local
path-participant set. Consumers must supply the actual gateway, relay, executor
and auditor identities for their path. Keep this marker list local and share it
only with operators authorized to perform a surfacing check.

An operator checks authorized logs, exports or data surfaces with
`python -m privacy.canary --records local-canaries.json path-to-surface`.
The JSON result reports marker hashes, source paths and all recorded path
participants. A surfaced marker is telemetry, not proof that any particular
participant leaked it. Canaries never trigger slashing, attribution decisions,
or any enforcement. Scanning a finite set of surfaces cannot establish that
no retention or leak occurred elsewhere.

Do not claim confidential execution, operator-blind inference, guaranteed erasure,
leak detection completeness, anonymity, perfect redaction, automatic TEE protection,
or prevention of custodian-threshold collusion. Do not imply that a signature or
policy digest proves an operator followed its policy. Describe only the enabled
endpoint and actual tier, cryptographic boundaries and limitations above.

For a dedicated single-host GPU integration exercise, build `cmd/emberchain` and
`cmd/emberd`, then run [the integration runner](../ops/privacy_staging.py) with
`--work-dir`, `--emberchain`, `--emberd` and `--model-path`. The runner requires
CUDA and at least 16 GiB available system memory and refuses CPU substitution.
It creates its own validator, role identities and ciphertext captures, checks
P0 escrow/replay and P1 refusal, then stops only its own processes. Its sequential
reference replay reuses one real model instance; it does not demonstrate
independent-operator or cross-device numerical agreement. The directory contains
private keys and must remain local and outside source control.

### Assignment and rejection policy

Encrypted freezes pin the same request identity, future-block assignment window
and audit work policy as ordinary freezes. Assigned auditors must have registered
wrapping keys; retired executors remain excluded. Private rejection transactions
include authenticated offense grading and use the common confirmation and
misjudgment policy. Numerical grading uses the SHA-256 digest of the canonical
envelope evidence policy as its band identifier and the hold's frozen model and
output-length thresholds. A replay mismatch without a matching frozen threshold
is inconclusive and cannot authorize a penalty.
