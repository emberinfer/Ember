# Threat Model Summary

Ember assumes less than one third Byzantine CometBFT voting power and does not
assume inference nodes, gateways or auditors are honest. Signed evidence provides
attribution, not a proof of correct execution. Numeric audit is probabilistic and
uncalibrated runtime pairs cannot slash.

Main residual risks are operator/auditor collusion, stake concentration, gateway
key custody, validator-key compromise, denial of service, model supply-chain
attacks and prompt disclosure to gateways/executors/auditors. There is no TEE,
MPC, confidential inference, hardware attestation or mainnet economics claim.
See [the threat model](THREAT_MODEL.md) for the full inventory and controls, and
[audit design](AUDIT_DESIGN.md) for replay, confirmation and economic limits.
