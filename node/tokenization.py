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
