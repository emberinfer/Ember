# Model and layout identities

Exact signed card byte roots remain the registration trust anchor. Source locators
and node runtime/device declarations do not enter model identity.

Weights v1: sort weight artifacts (.safetensors, .bin, .gguf) by path; encode the
Go JSON array of {Name,SHA256,Size} with SHA256 lowercase hex, Size integer, compact
UTF-8. Hash `ember-weights-v1\0` followed by those bytes.

Model v1: SHA256(`ember-model-v1\0` || weight root bytes || tokenizer hash bytes ||
SHA256(canonical_config)). canonical_config is compact sorted-key JSON UTF-8
using Go encoding/json numeric normalization (finite float64), containing the actual complete model
configuration. Exact config_bytes must hash to the config.json artifact root, and
the chain recomputes canonical_config from these bytes. Each root component is exposed in model.identity on /state.
Legacy cards missing canonical_config cannot be assigned a fabricated identity.

Layout v1: SHA256(`ember-layout-v1\0` || deterministic protobuf LayoutScheme).
The full scheme is stored in signed_manifest/publication.manifest.schemes. Its
atoms, minimum granularity, allowed cuts, per-layer/embedding/head compute and
optional MoE layout all enter the hash. A profile binds scheme_hash in addition
to the exact card root and layer range. A frozen pipeline cannot mix schemes;
step receipts bind the same scheme. Range auditors must bind the same scheme.

Production migration requires full manifest/config recovery. The stockcheck
records missing components; synthetic rehearsal identities are not live identities.
