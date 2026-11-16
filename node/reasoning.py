"""Versioned generation parameters and fail-closed independent token budgets."""
import hashlib

def sampling_hash(job):
    if 'thinking_enabled' not in job:
        return hashlib.sha256(str(job['max_tokens']).encode()).digest()
    on = job['thinking_enabled']
    thinking, answer = job.get('thinking_budget', 0), job.get('answer_budget', 0)
    if (type(on) is not bool or type(thinking) is not int or type(answer) is not int
            or answer <= 0 or thinking < 0 or thinking + answer > job['max_tokens']
            or (on and thinking == 0) or (not on and thinking != 0)):
        raise ValueError('invalid reasoning mode or budgets')
    raw = (f'ember-sampling-v2|max_tokens={job["max_tokens"]}|thinking_enabled={str(on).lower()}'
           f'|thinking_budget={thinking}|answer_budget={answer}')
    return hashlib.sha256(raw.encode()).digest()

def prepare(engine, prompt, job):
    sampling_hash(job)
    if 'thinking_enabled' not in job:
        return prompt
    method = getattr(engine, 'thinking_prompt', None)
    if method is not None:
        return method(prompt, job['thinking_enabled'])
    if getattr(engine, 'thinking_via_payload', False):
        # The adapter forwards enable_thinking to the engine in its
        # payload (vLLM chat_template_kwargs); the prompt itself is
        # already the committed input and must not change.
        return prompt
    raise ValueError('engine cannot enforce thinking chat template')

class Budget:
    def __init__(self, job):
        self.job = job
        self.reasoning = self.content = 0
        self.active = job.get('thinking_enabled', False)
        self.tail = ''
    def add(self, text):
        if 'thinking_enabled' not in self.job:
            self.content += 1
            return
        joined = self.tail + text
        if not self.job['thinking_enabled'] and ('<think>' in joined or '</think>' in joined):
            raise ValueError('thinking disabled: model emitted a reasoning segment')
        if self.active:
            self.reasoning += 1
            if self.reasoning > self.job.get('thinking_budget', 0):
                raise ValueError('thinking budget exhausted; request aborted without settlement receipt')
            if '</think>' in joined:
                self.active = False
        else:
            self.content += 1
            if self.content > self.job['answer_budget']:
                raise ValueError('answer budget exhausted; request aborted without settlement receipt')
        self.tail = joined[-16:]
