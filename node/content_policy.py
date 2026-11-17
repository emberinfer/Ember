"""Content decisions come exclusively from the signed profile manifest."""
import hashlib

from ember_core import digest, verify
from engine import Step


def validate(policy):
    if (policy.id != 'prompt-sha256-denylist' or policy.version != '1'
            or not policy.denied_prompt_sha256 or not policy.refusal_tokens
            or not policy.refusal_text
            or len(set(policy.denied_prompt_sha256)) != len(policy.denied_prompt_sha256)
            or any(len(value) != 32 for value in policy.denied_prompt_sha256)):
        raise ValueError('unsupported or incomplete content policy')


def from_manifest(genesis, manifest):
    verify(genesis)
    verify(manifest)
    if (manifest.chain_id != genesis.chain_id or manifest.signer != genesis.signer
            or digest(manifest) != genesis.model_manifest_root):
        raise ValueError('content policy manifest anchor mismatch')
    if not manifest.HasField('content_policy'):
        return None
    if manifest.shards or manifest.dynamic_ranges:
        raise ValueError('content policy requires whole-model profile')
    validate(manifest.content_policy)
    return manifest.content_policy


def refusal_steps(policy, prompt):
    if policy is None:
        return None
    validate(policy)
    if hashlib.sha256(prompt.encode()).digest() not in policy.denied_prompt_sha256:
        return None
    # Refusals are a declared output category, not fabricated model logits.
    return [Step(token, policy.refusal_text if i == 0 else '', [], {})
            for i, token in enumerate(policy.refusal_tokens)]
