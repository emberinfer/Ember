"""Executor and auditor integration with the existing Engine SPI/numeric replay."""
import hashlib
import json
import math
from .crypto import canonical, digest, public, b64, unb64, seal, unwrap
from .envelope import validate, context
from .escrow import identity, reconstruct
from .evidence import commit, levels, decrypt_bundle
from engine import Step
from auditor.numeric import compare, RANK_PROB, score_map


class Executor:
    def __init__(self, chain, signing_key, wrap_key, engine, store, model_id):
        self.chain, self.signing_key, self.wrap_key = chain, signing_key, wrap_key
        self.engine, self.store, self.model_id = engine, store, model_id

    def prepare_pricing(self, raw):
        """Serving tokenizer signs N_in before the consumer freezes the fee."""
        from ember_pb2 import ExecutionCommitment, ChainGenesis
        from ember_core import sign
        from .crypto import open_seal
        e,blob=validate(raw)
        actor=identity(public(self.signing_key))
        if e['tier']!='P0' or e['model_id']!=self.model_id:
            raise ValueError('unsupported tier or model')
        try:
            self.chain.request(e['request_id'])
        except KeyError:
            if e['wrapped_keys']['executor']!=actor or self.chain.operator(actor)['wrap_key']!=b64(public(self.wrap_key)):
                raise ValueError('wrong executor')
            key=unwrap(self.wrap_key,unb64(e['wrapped_keys']['key']),context(e,'executor'))
        else:
            key=self.unwrap_request(raw)
        body=json.loads(open_seal(key,blob,context(e,'prompt')))
        if not isinstance(body['prompt'],str) or type(body['max_tokens']) is not int or not 1<=body['max_tokens']<=64:
            raise ValueError('invalid generation parameters')
        profile=ChainGenesis.FromString(unb64(self.chain.model(self.model_id)['signed_profile']))
        c=ExecutionCommitment(request_id=e['request_id'],chain_id=self.model_id,
            profile_ver=profile.execution_profile_ver,input_tokens=self.engine.count_input_tokens(body['prompt']),
            max_output_tokens=body['max_tokens'],input_commitment=bytes.fromhex(e['ciphertext_blob_ref']))
        sign(c,self.signing_key)
        fee=profile.price_input*c.input_tokens+profile.price_output*c.max_output_tokens+profile.price_context*c.input_tokens*c.max_output_tokens//1024
        return dict(pricing_commitment=b64(c.SerializeToString(deterministic=True)),amount=fee)

    def execute(self, raw, checkpoint_tokens=None, prior_ciphertexts=()):
        e, blob = validate(raw)
        if e['tier'] != 'P0':
            raise ValueError('TIER_UNAVAILABLE')
        record = self.chain.request(e['request_id'])
        actor = identity(public(self.signing_key))
        if (record['status'] not in ('requested', 'checkpoint') or record['envelope_hash'] != digest(e)
                or record['executor'] != actor or record['consumer'] != identity(unb64(e['consumer_key']))
                or e['wrapped_keys']['epoch'] != record['epoch']
                or e['model_id'] != self.model_id or record['model_id'] != self.model_id):
            raise ValueError('request differs from chain registration')
        policy = dict(top_k=16, metric=RANK_PROB, tolerance=.001, layers=['0','14','27'], delete_after_finality=True)
        if e['evidence_policy'] != policy:
            raise ValueError('unsupported evidence policy')
        from .migration import executor_key
        key = executor_key(self.chain,e['request_id'],actor,self.wrap_key,e)
        from .crypto import open_seal
        prompt = json.loads(open_seal(key,blob,context(e,'prompt')))
        if (not isinstance(prompt['prompt'],str) or not 1 <= prompt['max_tokens'] <= 64
                or type(prompt['seed']) is not int):
            raise ValueError('invalid generation parameters')
        from .fees import frozen_terms
        input_tokens, _ = frozen_terms(self.chain,e['request_id'],self.engine,prompt['prompt'],prompt['max_tokens'],0)
        prefix = []
        segments = record.get('segments', [])
        if len(prior_ciphertexts) != len(segments):
            raise ValueError('missing migration evidence')
        for segment, ciphertext in zip(segments, prior_ciphertexts):
            if hashlib.sha256(ciphertext).hexdigest() != segment['bundle_ref']:
                raise ValueError('wrong checkpoint bundle')
            previous = decrypt_bundle(key,e['request_id'],ciphertext)
            items = previous['items']
            if (previous['envelope_hash'] != record['envelope_hash'] or previous['root'] != segment['root']
                    or levels(items)[-1][0].hex() != segment['root'] or len(items) != segment['tokens']
                    or items[:len(prefix)] != prefix):
                raise ValueError('checkpoint continuity failure')
            prefix = items
        limit = checkpoint_tokens if checkpoint_tokens is not None else prompt['max_tokens']
        if not len(prefix) < limit <= prompt['max_tokens']:
            raise ValueError('invalid checkpoint boundary')
        # Greedy SPI: recompute the prefix, verify tokens, preserve its exact salted openings.
        import itertools
        generation = self.engine.generate(prompt['prompt'],prompt['seed'],prompt['max_tokens'])
        try:
            steps = list(itertools.islice(generation,limit))
        finally:
            close = getattr(generation,'close',None)
            if close: close()
        if len(steps) <= len(prefix) or any(s.token != item['step']['token'] for s,item in zip(steps,prefix)):
            raise ValueError('generation diverged from checkpoint')
        if prefix:
            committed_steps = [Step(item['step']['token'],'',item['step']['topk'],
                {k:bytes.fromhex(v) for k,v in item['step']['activations'].items()}) for item in prefix]
            distance,_ = compare(committed_steps,steps[:len(prefix)],policy['metric'])
            if distance > policy['tolerance']:
                raise ValueError('checkpoint replay failed')
        root, items = commit(steps)
        items[:len(prefix)] = prefix
        root = levels(items)[-1][0].hex()
        bundle = dict(root=root, items=items, prompt=prompt['prompt'], seed=prompt['seed'],
                      envelope_hash=digest(e), evidence_policy=policy)
        ref = self.store.put(key,e['request_id'],bundle)
        input_tokens, actual = frozen_terms(self.chain,e['request_id'],self.engine,prompt['prompt'],prompt['max_tokens'],len(items))
        op = 'checkpoint' if checkpoint_tokens is not None else 'commit'
        self.chain.submit(op,request_id=e['request_id'],root=root,bundle_ref=ref,
                          input_tokens=input_tokens,output_tokens=len(items),actual=actual)
        output = seal(key,canonical(dict(text=''.join(s.text for s in steps))),context(e,'output'))
        receipt = dict(request_id=e['request_id'],tier='P0',executor=actor,envelope_hash=digest(e),
            root=root,bundle_ref=ref,input_tokens=input_tokens,output_tokens=len(items),actual=actual,output_hash=hashlib.sha256(output).hexdigest())
        return dict(receipt=receipt,signature=b64(self.signing_key.sign(b'ember/privacy-receipt/v2\0'+canonical(receipt))),output=b64(output))


    def unwrap_request(self, raw):
        from .migration import executor_key
        e,_ = validate(raw)
        record = self.chain.request(e['request_id'])
        if digest(e) != record['envelope_hash'] or record['status'] not in ('requested','checkpoint'):
            raise ValueError('inactive or wrong envelope')
        return executor_key(self.chain,e['request_id'],identity(public(self.signing_key)),self.wrap_key,e)

    def bundle(self, request):
        from .migration import authorize_bundle
        return self.store.get(authorize_bundle(self.chain, request))

    def retire(self, request_id):
        from .migration import retired
        record = self.chain.request(request_id)
        if not retired(record,identity(public(self.signing_key))):
            raise ValueError('not retired executor')
        refs = [record.get('bundle_ref')] + [s['bundle_ref'] for s in record.get('segments', [])]
        failed = False
        for ref in set(filter(None,refs)):
            try: (self.store.directory/ref).unlink(missing_ok=True)
            except OSError: failed = True
        self.chain.event('migration_delete_failed' if failed else 'migration_deleted',request_id=request_id)


def audit(chain, signing_key, wrapping_key, envelope, custodian_releases, ciphertext, engine, prior_ciphertexts=()):
    """Assigned operator sees plaintext; intermediaries receive only verdict/hash."""
    request_id = envelope['request_id']
    record = chain.request(request_id)
    if record['status'] != 'assigned' or record['auditor'] != identity(public(signing_key)):
        raise ValueError('not assigned auditor')
    epoch = chain.epoch(record['epoch'])
    seen, shares = set(), []
    for actor, release in custodian_releases:
        if actor in seen or actor not in epoch['members']:
            raise ValueError('duplicate or unregistered custodian')
        seen.add(actor)
        shares.append(json.loads(unwrap(wrapping_key,release,(request_id+'/auditor/'+record['epoch']).encode())))
    key = reconstruct(shares,epoch['threshold'])
    if hashlib.sha256(ciphertext).hexdigest() != record['bundle_ref']:
        raise ValueError('wrong bundle reference')
    bundle = decrypt_bundle(key,request_id,ciphertext)
    if bundle['envelope_hash'] != record['envelope_hash'] or digest(envelope) != record['envelope_hash']:
        raise ValueError('wrong envelope')
    if levels(bundle['items'])[-1][0].hex() != record['root'] or bundle['root'] != record['root']:
        raise ValueError('commitment mismatch')
    segments = record.get('segments', [])
    if len(prior_ciphertexts) != len(segments):
        raise ValueError('missing checkpoint audit evidence')
    for segment, prior in zip(segments,prior_ciphertexts):
        if hashlib.sha256(prior).hexdigest() != segment['bundle_ref']:
            raise ValueError('wrong checkpoint reference')
        before = decrypt_bundle(key,request_id,prior)
        if (levels(before['items'])[-1][0].hex() != segment['root']
                or len(before['items']) != segment['tokens']
                or bundle['items'][:len(before['items'])] != before['items']):
            raise ValueError('migration evidence discontinuity')
    from .fees import frozen_terms
    frozen = chain.hold(request_id).get('token_fee')
    if frozen and frozen.get('commitment'):
        from ember_pb2 import ExecutionCommitment
        c = ExecutionCommitment.FromString(unb64(frozen['commitment']))
        frozen_terms(chain,request_id,engine,bundle['prompt'],c.max_output_tokens,len(bundle['items']))
    policy = envelope['evidence_policy']
    if bundle['evidence_policy'] != policy:
        raise ValueError('policy mismatch')
    steps = []
    for i,item in enumerate(bundle['items']):
        s=item['step']; scores=score_map(s['topk'])
        if (item['index']!=i or len(bytes.fromhex(item['salt']))!=32 or len(scores)!=16
                or set(s['activations'])!=set(policy['layers']) or s.get('score_kind','raw-logits')!='raw-logits'
                or s['token'] not in scores or scores[s['token']]!=max(scores.values())):
            raise ValueError('invalid committed evidence')
        steps.append(Step(s['token'],'',s['topk'],{k:bytes.fromhex(v) for k,v in s['activations'].items()}))
    replay = engine.replay(bundle['prompt'],bundle['seed'],[s.token for s in steps])
    distance, stats = compare(steps,replay,policy['metric'])
    verdict = 'accept' if distance <= policy['tolerance'] else 'reject'
    stats['distance'] = distance if math.isfinite(distance) else 1e30
    stats['metric'],stats['tolerance']=policy['metric'],policy['tolerance']
    fields = {}
    if verdict == 'reject':
        # A mismatch alone no longer authorizes a penalty. Use the frozen
        # model/band/length thresholds, or report inconclusive for review.
        hold = chain.hold(request_id)
        for pair in hold.get('misjudge_thresholds', []):
            if (pair['model'] == hold.get('model_id') and pair['band'] == digest(policy)
                    and pair['min_output'] <= len(steps) <= pair['max_output']):
                fields['offense'] = {'class': 'numerical', 'score': stats['distance'],
                    'band': digest(policy), 'length_class': pair['length_class']}
                break
        else:
            verdict = 'inconclusive'
    chain.submit('verdict',request_id=request_id,verdict=verdict,stats_hash=digest(stats),**fields)
    return verdict,stats
