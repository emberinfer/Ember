"""Pricing-v2 arithmetic; consensus independently verifies every count and fee."""
from .crypto import unb64


def frozen_terms(chain, request_id, engine, prompt, max_tokens, output_tokens):
    from ember_pb2 import ExecutionCommitment, ChainGenesis
    frozen = chain.hold(request_id).get('token_fee')
    if not frozen:
        return 0, 0
    profile = ChainGenesis.FromString(unb64(frozen['profile']))
    raw = unb64(frozen['commitment'])
    if raw:
        commitment = ExecutionCommitment.FromString(raw)
        count = engine.count_input_tokens(prompt)
        if count != commitment.input_tokens or max_tokens != commitment.max_output_tokens:
            raise ValueError('generation differs from frozen tokenization')
    else:
        count = 0
    fee = (profile.price_input*count + profile.price_output*output_tokens
           + profile.price_context*count*output_tokens//1024)
    return count, fee
