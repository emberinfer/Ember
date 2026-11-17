"""Exact tokenization identities and failure classes shared by serving and replay."""
import hashlib
import json
from pathlib import Path


class TokenizerUnavailable(ValueError):
    pass


class InputTokenMismatch(ValueError):
    def __init__(self, count, version):
        super().__init__('input token count mismatch')
        self.count, self.version = count, version


def artifact_version(path):
    import transformers
    import tokenizers
    root = Path(path)
    names = ('tokenizer.json', 'tokenizer_config.json', 'special_tokens_map.json',
             'added_tokens.json', 'vocab.json', 'merges.txt', 'tokenizer.model', 'chat_template.jinja')
    artifacts = {name: hashlib.sha256((root/name).read_bytes()).hexdigest()
                 for name in names if (root/name).is_file()}
    if not any(name in artifacts for name in ('tokenizer.json', 'tokenizer.model', 'vocab.json')):
        raise TokenizerUnavailable('tokenizer artifacts unavailable')
    identity = dict(algorithm='hf-input-ids-v1', transformers=transformers.__version__,
                    tokenizers=tokenizers.__version__, artifacts=artifacts)
    return 'sha256:' + hashlib.sha256(json.dumps(identity,sort_keys=True,separators=(',',':')).encode()).hexdigest()


def version(engine):
    try:
        value = engine.tokenizer_version
        if not isinstance(value, str) or not value:
            raise ValueError('missing tokenizer version')
        return value
    except Exception as exc:
        raise TokenizerUnavailable('tokenizer version unavailable') from exc


def count(engine, prompt, expected=None):
    try:
        actual = version(engine)
        if expected is not None and (not expected or actual != expected):
            raise TokenizerUnavailable('pinned tokenizer version unavailable')
        value = engine.count_input_tokens(prompt)
        if type(value) is not int or not 0 < value <= 0xffffffff:
            raise TokenizerUnavailable('invalid tokenizer result')
        # Never accept an engine that changed version during tokenization.
        if version(engine) != actual:
            raise TokenizerUnavailable('tokenizer changed during tokenization')
        return value, actual
    except TokenizerUnavailable:
        raise
    except Exception as exc:
        raise TokenizerUnavailable('tokenizer unavailable') from exc


def verify_count(engine, prompt, committed, expected):
    value, actual = count(engine,prompt,expected)
    if value != committed:
        raise InputTokenMismatch(value,actual)
    return value


def _byte_unicode_inverse():
    # Missing bytes are represented by successive codepoints above Latin-1.
    originals = list(range(33, 127)) + list(range(161, 173)) + list(range(174, 256))
    mapping = {chr(value): value for value in originals}
    mapping.update({chr(256 + index): value for index, value in
                    enumerate(value for value in range(256) if value not in originals)})
    return mapping


_BYTE_UNICODE_INVERSE = _byte_unicode_inverse()


class TokenTextDecoder:
    """Decode token bytes incrementally, independent of transport boundaries.

    At generation end, flush returns exactly one U+FFFD for a truncated trailing
    UTF-8 sequence, matching whole-sequence replace decoding; a complete tail
    returns ''.
    """
    def __init__(self, tokenizer):
        import codecs
        self.tokenizer = tokenizer
        self._decoder = codecs.getincrementaldecoder('utf-8')(errors='replace')
        self._cache = {}

    def _token_bytes(self, token_id):
        if token_id in self._cache:
            return self._cache[token_id]
        tokenizer = self.tokenizer
        try:
            piece = tokenizer.convert_ids_to_tokens([token_id])[0]
            if not isinstance(piece, str):
                raise TypeError('token piece must be text')
        except Exception:
            # Compatibility only for decode-only test doubles, not byte BPE.
            ids = [token_id]
            value = tokenizer.decode(ids).encode('utf-8')
        else:
            inverse = _BYTE_UNICODE_INVERSE
            encoder = getattr(tokenizer, 'byte_encoder', None)
            backend = getattr(tokenizer, 'backend_tokenizer', None)
            decoder = getattr(backend, 'decoder', None)
            identity = type(tokenizer).__module__ + type(tokenizer).__name__
            # Known fast byte-BPE families can lack an exposed ByteLevel decoder.
            family = any(name in identity for name in ('Qwen', 'GPT2', 'Llama'))
            byte_level = (encoder is not None or 'ByteLevel' in type(decoder).__name__
                          or (getattr(tokenizer, 'is_fast', False) and family
                              and all(char in inverse for char in piece)))
            if encoder is not None:
                inverse = {char: value for value, char in encoder.items()}
            if byte_level:
                value = (bytes(inverse[char] for char in piece)
                         if all(char in inverse for char in piece) else piece.encode('utf-8'))
            else:
                value = piece.replace('▁', ' ').encode('utf-8')
        self._cache[token_id] = value
        return value

    def feed(self, token_id) -> str:
        return self._decoder.decode(self._token_bytes(token_id))

    def flush(self) -> str:
        """Finalize: one U+FFFD for a truncated UTF-8 tail, otherwise ''."""
        return self._decoder.decode(b'', final=True)


def decode_token_texts(tokenizer, token_ids) -> list[str]:
    decoder = TokenTextDecoder(tokenizer)
    texts = [decoder.feed(token) for token in token_ids]
    if texts:
        texts[-1] += decoder.flush()
    return texts
