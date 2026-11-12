"""Beta local detectors; maps must never be submitted or logged remotely."""
import re
import secrets
from dataclasses import dataclass

PATTERNS = {
    'private_key': re.compile(r'-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----[\s\S]*?-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----'),
    'credential': re.compile(r'\b(?:AKIA[A-Z0-9]{16}|sk-[A-Za-z0-9_-]{12,}|ghp_[A-Za-z0-9]{20,}|(?:password|passwd|api[_-]?key|token)\s*[:=]\s*["\']?[^\s"\',;]{4,})', re.I),
    'email': re.compile(r'(?<![\w.+-])[A-Za-z0-9.!#$%&\x27*+/=?^_`{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?\.[A-Za-z]{2,}(?![\w.-])'),
    'phone': re.compile(r'(?<!\w)(?:\+\d{1,3}[ .-]?)?(?:\(\d{3}\)|\d{3})[ .-]\d{3}[ .-]\d{4}(?!\d)'),
    'account_id': re.compile(r'\b(?:account|acct|id|ssn)\s*[:=#]\s*[A-Za-z0-9][A-Za-z0-9-]{3,}', re.I),
}


@dataclass(frozen=True)
class Match:
    start: int
    end: int
    kind: str


def detect(text):
    candidates = [Match(m.start(),m.end(),kind) for kind,pattern in PATTERNS.items() for m in pattern.finditer(text)]
    # Prefer the longest span at the same position; skip nested matches.
    result = []
    for match in sorted(candidates,key=lambda m:(m.start,-m.end)):
        if not result or match.start >= result[-1].end:
            result.append(match)
    return result


def redact(text):
    nonce = secrets.token_hex(16)
    while nonce in text:
        nonce = secrets.token_hex(16)
    mapping, output, cursor = {}, [], 0
    for i,match in enumerate(detect(text)):
        placeholder = f'<EMBER_REDACTED_{nonce}_{i}>'
        mapping[placeholder] = text[match.start:match.end]
        output.extend([text[cursor:match.start],placeholder]);cursor=match.end
    output.append(text[cursor:])
    return ''.join(output),mapping


def rehydrate(text, mapping):
    if not mapping:
        return text
    # One substitution pass prevents cascades through original sensitive text.
    pattern = re.compile('|'.join(re.escape(k) for k in mapping))
    return pattern.sub(lambda m:mapping[m.group()],text)


def measure(cases):
    tp, fp, fn, roundtrips = 0,0,0,0
    by_class = {kind:dict(tp=0,fp=0,fn=0) for kind in PATTERNS}
    for case in cases:
        text = case['text']
        expected = set()
        for kind,secret in case['sensitive']:
            start = text.index(secret);expected.add((start,start+len(secret),kind))
        found = {(m.start,m.end,m.kind) for m in detect(text)}
        tp+=len(found&expected);fp+=len(found-expected);fn+=len(expected-found)
        for kind in by_class:
            by_class[kind]['tp']+=sum(m[2]==kind for m in found&expected)
            by_class[kind]['fp']+=sum(m[2]==kind for m in found-expected)
            by_class[kind]['fn']+=sum(m[2]==kind for m in expected-found)
        redacted,mapping=redact(text)
        roundtrips+=rehydrate(redacted,mapping)==text
    return dict(tp=tp,fp=fp,fn=fn,precision=tp/(tp+fp) if tp+fp else 1.,
                recall=tp/(tp+fn) if tp+fn else 1.,cases=len(cases),lossless=roundtrips,by_class=by_class)
