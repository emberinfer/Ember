"""Pricing-v2 arithmetic; consensus independently verifies every count and fee."""
from .crypto import unb64


def scale_fee(base, ppm):
    if base < 0 or base > 2**63-1 or type(ppm) is not int or ppm <= 0:
        raise ValueError('invalid price or multiplier')
    fee = base * ppm // 1000000
    if fee > 2**63-1:
        raise ValueError('scaled fee overflow')
    return fee


def frozen_terms(chain, request_id, engine, prompt, max_tokens, output_tokens):
    from ember_pb2 import ExecutionCommitment, ChainGenesis
    frozen = chain.hold(request_id).get('token_fee')
    if not frozen:
        return 0, 0
    profile = ChainGenesis.FromString(unb64(frozen['profile']))
    raw = unb64(frozen['commitment'])
    if raw:
        commitment = ExecutionCommitment.FromString(raw)
        from tokenization import verify_count
        count = verify_count(engine,prompt,commitment.input_tokens,commitment.tokenizer_version)
        if max_tokens != commitment.max_output_tokens:
            raise ValueError('generation differs from frozen tokenization')
    else:
        count = 0
    fee = (profile.price_input*count + profile.price_output*output_tokens
           + profile.price_context*count*output_tokens//1024)
    return count, scale_fee(fee, frozen.get('multiplier_ppm') or 1000000)
