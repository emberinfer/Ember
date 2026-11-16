# Security Policy

Report vulnerabilities privately to the operator responsible for the deployment.
Do not include private keys, consumer prompts, model artifacts or exploit details
in public issues. Preserve affected release versions, transaction hashes and logs.

The signed release line is `0.22.x`; this repository snapshot tracks the live
testnet implementation (application version 9), which is ahead of the last
signed release. Security fixes may require a chain halt and hard
fork; operators must follow `docs/UPGRADE.md`. The current system does not claim
confidential inference, hardware attestation or independent-operator audit.
