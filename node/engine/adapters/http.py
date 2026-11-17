"""Strict HTTP adapters with streaming vLLM completions. These services expose logprobs, not raw logits.

No server is spawned. Neither adapter claims activations or forced-prefix replay.
"""
import json
import math
import os
import socket
import time
import urllib.error
import urllib.parse
import urllib.request

from tokenization import TokenTextDecoder

from ..spi import (EngineAdapter, EngineCapabilities, Step, EngineProtocolError,
                   EngineTimeout, EngineUnavailable, UnsupportedCapability)

MAX_RESPONSE = 8 * 1024**2
SCORE_TOP_K = 16


def token_id(value):
    if type(value) is int and value >= 0:
        return value
    if isinstance(value, str) and value.startswith('token_id:'):
        value = value[len('token_id:'):]
        if value.isascii() and value.isdecimal():
            return int(value)
    raise EngineProtocolError('server did not provide actual token IDs')


def scored_token_id(value):
    """Token ID keys in prompt_logprobs entries are bare decimal strings."""
    try:
        return token_id(value)
    except EngineProtocolError:
        if isinstance(value, str) and value.isascii() and value.isdecimal():
            return int(value)
        raise


def pairs(values, limit):
    if not isinstance(values, list) or not values or len(values) > limit:
        raise EngineProtocolError('missing or oversized probability candidates')
    result = []
    for ident, value in values:
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value > 0:
            raise EngineProtocolError('invalid log probability')
        result.append((token_id(ident), float(value)))
    if len({ident for ident, _ in result}) != len(result):
        raise EngineProtocolError('duplicate candidate token')
    return result


def prompt_logprob_pairs(entry, limit):
    """Parse one prompt_logprobs entry into a sorted top-k (id, logprob) row.

    vLLM returns the top-k candidates plus the actually-committed token when
    it falls outside the top-k, so the entry can hold limit+1 candidates;
    the row is the `limit` highest by logprob. Validation mirrors pairs():
    finite, non-positive, duplicate-free token IDs.
    """
    if not isinstance(entry, dict) or not entry:
        raise EngineProtocolError('missing prompt logprob candidates')
    result = []
    for ident, value in entry.items():
        if not isinstance(value, dict) or 'logprob' not in value:
            raise EngineProtocolError('malformed prompt logprob candidate')
        logprob = value['logprob']
        if isinstance(logprob, bool) or not isinstance(logprob, (int, float)) or not math.isfinite(logprob) or logprob > 0:
            raise EngineProtocolError('invalid prompt log probability')
        result.append((scored_token_id(ident), float(logprob)))
    if len({ident for ident, _ in result}) != len(result):
        raise EngineProtocolError('duplicate candidate token')
    result.sort(key=lambda item: (-item[1], item[0]))
    return result[:limit]


class HTTPCompletionEngine(EngineAdapter):
    device = 'remote'
    def __init__(self, endpoint, model, api_key=None, server_version='unknown'):
        parsed = urllib.parse.urlsplit(endpoint)
        if parsed.scheme not in ('http', 'https') or not parsed.netloc or parsed.username or parsed.password or parsed.query or parsed.fragment:
            raise ValueError('invalid completion endpoint')
        if not model:
            raise ValueError('remote model must be explicitly configured')
        self.endpoint, self.model, self.api_key = endpoint.rstrip('/'), model, api_key
        self.server_version = server_version

    def capabilities(self):
        return EngineCapabilities(self.name, self.server_version, 'remote', 'unknown',
                                  ('topk-logprobs',), 'logprobs', 20, False,
                                  native_incremental=False)

    def _post(self, route, payload, timeout, max_response=MAX_RESPONSE):
        headers = {'Content-Type': 'application/json'}
        if self.api_key:
            headers['Authorization'] = 'Bearer ' + self.api_key
        request = urllib.request.Request(self.endpoint + route,
                   json.dumps(payload, allow_nan=False).encode(), headers)
        deadline = time.monotonic() + timeout
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                raw = bytearray()
                while True:
                    if time.monotonic() >= deadline:
                        raise EngineTimeout('HTTP completion deadline expired')
                    block = response.read(min(65536, max_response + 1 - len(raw)))
                    raw.extend(block)
                    if len(raw) > max_response:
                        raise EngineProtocolError('completion response too large')
                    if not block:
                        break
            return json.loads(raw)
        except (socket.timeout, TimeoutError) as exc:
            raise EngineTimeout('HTTP completion timed out') from exc
        except urllib.error.HTTPError as exc:
            exc.close()
            if exc.code == 400:
                raise EngineProtocolError('HTTP completion validation failed (400)') from exc
            raise EngineUnavailable('HTTP completion service unavailable') from exc
        except (urllib.error.URLError, OSError) as exc:
            raise EngineUnavailable('HTTP completion service unavailable') from exc
        except (ValueError, UnicodeError) as exc:
            raise EngineProtocolError('invalid completion JSON') from exc

    def _step_text(self, ident, text, index):
        return text if index == 0 else ''

    def _generate_request(self, prompt, sampling):
        self.thinking_truncated = False
        data = self._post(self.route, self.payload(prompt, sampling), sampling.timeout_seconds)
        try:
            text, ids, rows = self.parse(data, sampling.top_k)
            if not isinstance(text, str) or not isinstance(ids, list) or not ids or len(ids) != len(rows) or len(ids) > sampling.max_tokens:
                raise EngineProtocolError('misaligned completion tokens/probabilities')
            steps = []
            for index, (ident, row) in enumerate(zip(ids, rows)):
                ident = token_id(ident)
                if ident not in {key for key, _ in row}:
                    raise EngineProtocolError('chosen token absent from evidence')
                steps.append(Step(ident, self._step_text(ident, text, index), row, {}, 'logprobs'))
        except (KeyError, TypeError, IndexError, ValueError) as exc:
            raise EngineProtocolError('malformed completion evidence') from exc
        yield from steps


class LlamaCppEngine(HTTPCompletionEngine):
    name, route = 'llama.cpp', '/completion'

    def payload(self, prompt, sampling):
        return {'prompt': prompt, 'n_predict': sampling.max_tokens,
                'seed': sampling.seed, 'temperature': 0, 'n_probs': sampling.top_k,
                'post_sampling_probs': False, 'return_tokens': True, 'stream': False}

    def parse(self, data, limit):
        rows = [pairs([(p['id'], p['logprob']) for p in item['top_logprobs']], limit)
                for item in data['completion_probabilities']]
        return data['content'], data['tokens'], rows


class VLLMEngine(HTTPCompletionEngine):
    name, route = 'vllm', '/v1/completions'
    # reasoning.prepare applies thinking_prompt for raw completions; the
    # payload also forwards the switch for template-applying servers.
    thinking_via_payload = True

    def __init__(self, endpoint, model, api_key=None, server_version='unknown',
                 eos_token_ids=(), streaming=None):
        super().__init__(endpoint, model, api_key, server_version)
        eos = tuple(eos_token_ids or ())
        if any(type(token) is not int or token < 0 for token in eos):
            raise ValueError('eos token IDs must be non-negative integers')
        self.eos_token_ids = eos
        self.streaming = os.environ.get("EMBER_VLLM_STREAM") != "0" if streaming is None else streaming

        self._tokenizer = None
        # Tokenizer identity for priced (v2) commitments: the same
        # artifact hash the transformers adapter pins, taken from the
        # local model directory this endpoint serves. A served model
        # name that is not a local directory leaves the version unset
        # and priced tokenization fails closed (TokenizerUnavailable).
        self._tokenizer_version = None
        try:
            from tokenization import artifact_version
            self._tokenizer_version = artifact_version(model)
        except Exception:
            self._tokenizer_version = None

    @property
    def tokenizer_version(self):
        from tokenization import artifact_version, TokenizerUnavailable
        if self._tokenizer_version is None:
            raise TokenizerUnavailable('tokenizer version unavailable')
        if artifact_version(self.model) != self._tokenizer_version:
            raise TokenizerUnavailable('tokenizer artifacts changed since load')
        return self._tokenizer_version

    @property
    def tokenizer(self):
        # The auditor's reasoning-usage reconstruction decodes evidence
        # tokens through engine.tokenizer - the interface the
        # transformers adapter exposes natively. Load the same pinned
        # tokenizer lazily for both generation and audit reconstruction.
        if self._tokenizer is None:
            from transformers import AutoTokenizer
            self._tokenizer = AutoTokenizer.from_pretrained(
                self.model, local_files_only=True, trust_remote_code=False)
        return self._tokenizer

    def thinking_prompt(self, prompt, enabled):
        # This lineage serves the gateway's flattened transcript prompt
        # through /v1/completions, where no chat template is applied and
        # chat_template_kwargs is ignored (measured on vLLM 0.33: the
        # off payload produced byte-identical output). The chat
        # template's own off state is an already-closed empty think
        # block; appending it makes the model answer directly, and the
        # reasoning Budget check (no reasoning segment may appear while
        # disabled) then passes. When thinking is enabled the committed
        # prompt is returned untouched. The payload kwarg above stays
        # as a second, harmless channel for template-applying servers.
        if enabled:
            return prompt
        return prompt + '<think>\n\n</think>\n\n'

    def _step_text(self, ident, text, index):
        if ident in self.eos_token_ids:
            return ''
        if getattr(self, '_text_decoder', None) is None:
            self._text_decoder = TokenTextDecoder(self.tokenizer)
        return self._text_decoder.feed(ident)

    def _flush_text(self, steps):
        if steps and getattr(self, '_text_decoder', None) is not None:
            # An EOS step receives the flush text (one U+FFFD only for a
            # genuinely truncated tail); no synthetic Step is introduced.
            steps[-1].text += self._text_decoder.flush()
        return steps

    def count_input_tokens(self, prompt):
        data = self._post('/tokenize', {'model': self.model, 'prompt': prompt}, 30)
        count = data.get('count') if isinstance(data, dict) else None
        if type(count) is not int or count <= 0:
            raise EngineProtocolError('tokenize endpoint returned no exact count')
        return count

    def _forced_prompt_ids(self, prompt):
        """Token IDs of the conditioning prefix, tokenized exactly as the
        generation path presents the prompt to the server.

        Merge seam: this lineage generates from the raw prompt string, so
        the prefix is the string tokenization. A lineage whose generation
        renders a chat template must override this one method to tokenize
        the rendered messages (POST /tokenize with `messages` and
        `add_generation_prompt`); everything downstream of the prefix IDs
        is presentation-independent.
        """
        data = self._post('/tokenize', {'model': self.model, 'prompt': prompt}, 30)
        ids = data.get('tokens') if isinstance(data, dict) else None
        if not isinstance(ids, list) or not ids or any(type(i) is not int or i < 0 for i in ids):
            raise EngineProtocolError('tokenize endpoint returned no exact token IDs')
        return ids

    def _post_scoring(self, route, payload, timeout, max_response):
        """POST for the scoring request only: a client error here means the
        server does not offer prompt_logprobs scoring, which is a missing
        capability (the audit worker falls back to regeneration), not a
        service outage."""
        headers = {'Content-Type': 'application/json'}
        if self.api_key:
            headers['Authorization'] = 'Bearer ' + self.api_key
        request = urllib.request.Request(self.endpoint + route,
                   json.dumps(payload, allow_nan=False).encode(), headers)
        deadline = time.monotonic() + timeout
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                raw = bytearray()
                while True:
                    if time.monotonic() >= deadline:
                        raise EngineTimeout('HTTP completion deadline expired')
                    block = response.read(min(65536, max_response + 1 - len(raw)))
                    raw.extend(block)
                    if len(raw) > max_response:
                        raise EngineProtocolError('scoring response too large')
                    if not block:
                        break
            return json.loads(raw)
        except urllib.error.HTTPError as exc:
            if 400 <= exc.code < 500:
                raise UnsupportedCapability(
                    'server rejected prompt_logprobs scoring') from exc
            raise EngineUnavailable('HTTP completion service unavailable') from exc
        except (socket.timeout, TimeoutError) as exc:
            raise EngineTimeout('HTTP completion timed out') from exc
        except (urllib.error.URLError, OSError) as exc:
            raise EngineUnavailable('HTTP completion service unavailable') from exc
        except (ValueError, UnicodeError) as exc:
            raise EngineProtocolError('invalid completion JSON') from exc

    def score_forced(self, prompt, seed, tokens):
        """Teacher-forced scoring of a committed sequence in one request.

        The committed token IDs are appended to the tokenized prompt and
        the server scores the whole sequence with prompt_logprobs: the
        entry at position len(prompt_ids)+j is the distribution for
        committed token j under the prompt plus the committed prefix, the
        same distribution generate() would report at that position along
        the committed path. One Step per committed position is returned,
        isomorphic to the transformers adapter's scored steps (greedy
        token, top-16 scores, EOS stop when configured).
        """
        if not tokens:
            return []
        committed = []
        for token in tokens:
            if type(token) is not int or token < 0:
                raise ValueError('committed tokens must be non-negative token IDs')
            committed.append(token)
        prompt_ids = self._forced_prompt_ids(prompt)
        full = prompt_ids + committed
        payload = {'model': self.model, 'prompt': full, 'max_tokens': 1,
                   'temperature': 0, 'prompt_logprobs': SCORE_TOP_K}
        data = self._post_scoring(self.route, payload,
                                  self.generation_timeout_seconds,
                                  max(MAX_RESPONSE, 8192 * len(full)))
        try:
            if len(data['choices']) != 1:
                raise EngineProtocolError('expected one completion choice')
            choice = data['choices'][0]
            scored = choice.get('prompt_logprobs')
            usage = data.get('usage')
            if isinstance(usage, dict) and usage.get('prompt_tokens') not in (None, len(full)):
                raise EngineProtocolError('scoring response token count mismatch')
        except EngineProtocolError:
            raise
        except (KeyError, TypeError, AttributeError) as exc:
            raise EngineProtocolError('malformed scoring evidence') from exc
        if scored is None:
            raise UnsupportedCapability('server returned no prompt logprobs')
        if not isinstance(scored, list) or len(scored) != len(full):
            raise EngineProtocolError('misaligned prompt logprobs')
        steps = []
        base = len(prompt_ids)
        for index in range(len(committed)):
            entry = scored[base + index]
            if entry is None:
                raise EngineProtocolError('missing prompt logprob distribution')
            row = prompt_logprob_pairs(entry, SCORE_TOP_K)
            token = row[0][0]
            steps.append(Step(token, '', row, {}, 'logprobs'))
            if token in self.eos_token_ids:
                break
        return steps

    def payload(self, prompt, sampling, stream=False):
        return {'model': self.model, 'prompt': prompt, 'max_tokens': sampling.max_tokens,
                'seed': sampling.seed, 'temperature': 0, 'logprobs': sampling.top_k,
                'return_tokens_as_token_ids': True, 'stream': stream,
                'chat_template_kwargs': {'enable_thinking': sampling.enable_thinking}}

    def parse(self, data, limit):
        if len(data['choices']) != 1:
            raise EngineProtocolError('expected one completion choice')
        choice = data['choices'][0]
        rows = [pairs(list(row.items()), limit) for row in choice['logprobs']['top_logprobs']]
        # Truncation marker: the whole budget was consumed before any
        # answer text existed (finish_reason 'length', empty text).
        # Surfaced on the receipt as output_category 'thinking-truncated'
        # by the agent; the gateway owns any fallback resend policy.
        if choice.get('finish_reason') == 'length' and not choice['text']:
            self.thinking_truncated = True
        return choice['text'], choice['logprobs']['tokens'], rows

    def parse_stream_chunk(self, data, limit):
        try:
            if len(data['choices']) != 1:
                raise EngineProtocolError('expected one completion choice')
            choice = data['choices'][0]
            text, finish = choice['text'], choice.get('finish_reason')
            if not isinstance(text, str):
                raise EngineProtocolError('invalid completion text')
            probs = choice.get('logprobs')
            if not probs or (not probs.get('tokens') and not probs.get('top_logprobs')):
                if text == '' and finish:
                    return [], finish
                raise EngineProtocolError('missing completion logprobs')
            ids, raw_rows = probs['tokens'], probs['top_logprobs']
            if not isinstance(ids, list) or not ids or len(ids) != len(raw_rows):
                raise EngineProtocolError('misaligned completion tokens/probabilities')
            steps = []
            for index, (ident, raw) in enumerate(zip(ids, raw_rows)):
                ident, row = token_id(ident), pairs(list(raw.items()), limit)
                if ident not in {key for key, _ in row}:
                    raise EngineProtocolError('chosen token absent from evidence')
                steps.append(Step(ident, self._step_text(ident, text, index), row, {}, 'logprobs'))
            if finish:
                self._flush_text(steps)
            return steps, finish
        except (KeyError, TypeError, IndexError, ValueError, AttributeError) as exc:
            raise EngineProtocolError('malformed completion evidence') from exc

    def _generate_request(self, prompt, sampling):
        self._text_decoder = None
        # EMBER_VLLM_STREAM=0 restores buffering. Fallback is allowed only at
        # response admission, before any Step; stream errors never retry.
        if not self.streaming:
            yield from self._flush_text(list(super()._generate_request(prompt, sampling)))
            return
        self.thinking_truncated = False
        headers = {'Content-Type': 'application/json'}
        if self.api_key:
            headers['Authorization'] = 'Bearer ' + self.api_key
        request = urllib.request.Request(self.endpoint + self.route,
            json.dumps(self.payload(prompt, sampling, stream=True), allow_nan=False).encode(), headers)
        deadline = time.monotonic() + sampling.timeout_seconds
        try:
            response = urllib.request.urlopen(request, timeout=sampling.timeout_seconds)
        except (socket.timeout, TimeoutError) as exc:
            raise EngineTimeout('HTTP completion timed out') from exc
        except urllib.error.HTTPError as exc:
            exc.close()
            if exc.code == 400:
                raise EngineProtocolError('HTTP completion validation failed (400)') from exc
            yield from self._flush_text(list(super()._generate_request(prompt, sampling)))
            return
        except (urllib.error.URLError, OSError) as exc:
            if isinstance(getattr(exc, 'reason', None), (socket.timeout, TimeoutError)):
                raise EngineTimeout('HTTP completion timed out') from exc
            yield from self._flush_text(list(super()._generate_request(prompt, sampling)))
            return
        with response:
            content_type = response.headers.get_content_type()
            if content_type != 'text/event-stream' and not (content_type == 'application/json' or content_type.endswith('+json')):
                response.close()
                yield from self._flush_text(list(super()._generate_request(prompt, sampling)))
                return
            total_bytes, count, finished, has_text = 0, 0, False, False
            pending = bytearray()

            def read_block():
                nonlocal total_bytes
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise EngineTimeout('HTTP completion deadline expired')
                # urllib's buffered reader otherwise retains the initial timeout
                # for every read, allowing a slow trickle to extend the deadline.
                if response.fp is None:
                    return b''
                sock = response.fp.raw._sock
                sock.settimeout(remaining)
                block = response.read1(min(65536, MAX_RESPONSE + 1 - total_bytes))
                total_bytes += len(block)
                if total_bytes > MAX_RESPONSE:
                    raise EngineProtocolError('completion response too large')
                if time.monotonic() >= deadline:
                    raise EngineTimeout('HTTP completion deadline expired')
                return block

            try:
                if content_type != 'text/event-stream':
                    raw = bytearray()
                    while block := read_block():
                        raw.extend(block)
                    data = json.loads(raw)
                    try:
                        text, ids, rows = self.parse(data, sampling.top_k)
                        if (not isinstance(text, str) or not isinstance(ids, list) or
                                not ids or len(ids) != len(rows) or len(ids) > sampling.max_tokens):
                            raise EngineProtocolError('misaligned completion tokens/probabilities')
                        steps = []
                        for index, (ident, row) in enumerate(zip(ids, rows)):
                            ident = token_id(ident)
                            if ident not in {key for key, _ in row}:
                                raise EngineProtocolError('chosen token absent from evidence')
                            steps.append(Step(ident, self._step_text(ident, text, index), row, {}, 'logprobs'))
                    except (KeyError, TypeError, IndexError, ValueError, AttributeError) as exc:
                        raise EngineProtocolError('malformed completion evidence') from exc
                    yield from self._flush_text(steps)
                    return
                done = False
                while not done:
                    block = read_block()
                    pending.extend(block)
                    # Process a final unterminated line too, then enforce EOF.
                    lines = pending.split(b'\n')
                    pending = bytearray(lines.pop())
                    if not block and pending:
                        lines.append(bytes(pending))
                        pending.clear()
                    for line in lines:
                        if not line.startswith(b'data:'):
                            continue
                        payload = line[5:].strip()
                        if payload == b'[DONE]':
                            finished, done = True, True
                            break
                        data = json.loads(payload)
                        steps, finish = self.parse_stream_chunk(data, sampling.top_k)
                        finished = finished or bool(finish)
                        has_text = has_text or bool(data['choices'][0]['text'])
                        if finish == 'length':
                            self.thinking_truncated = not has_text
                        count += len(steps)
                        if count > sampling.max_tokens:
                            raise EngineProtocolError('server exceeded token limit')
                        yield from steps
                    if not block:
                        break
                if not finished or not count:
                    raise EngineProtocolError('incomplete completion stream')
            except (socket.timeout, TimeoutError) as exc:
                raise EngineTimeout('HTTP completion timed out') from exc
            except urllib.error.HTTPError as exc:
                exc.close()
                if exc.code == 400:
                    raise EngineProtocolError('HTTP completion validation failed (400)') from exc
                raise EngineUnavailable('HTTP completion service unavailable') from exc
            except (urllib.error.URLError, OSError) as exc:
                raise EngineUnavailable('HTTP completion service unavailable') from exc
            except (ValueError, UnicodeError) as exc:
                raise EngineProtocolError('invalid completion JSON') from exc
