#!/usr/bin/env python3
"""Allow only the complete, reviewed synthetic redaction corpus, byte for byte."""
import hashlib
from pathlib import Path
import re
import subprocess
import sys

FIXTURE = 'testdata/privacy/redaction-golden.json'
REVIEWED_SHA256 = '2001eb31b733d85b0f53a84ec6757dc23895df8bc1e7c19eb564c5819e9271f6'
# Original public synthetic corpus, before placeholder names were made explicit.
# Accept it only in history; the working tree must use the current reviewed blob.
HISTORICAL_SHA256 = '700b0c1938f3888bfa4b4dcbab0b2677af79b906b00b34e97473212e67fa3477'
HIT = re.compile(r'(?:(?P<revision>[0-9a-f]{40}):)?' + re.escape(FIXTURE)
                 + r':(?P<line>[1-9][0-9]*):(?P<text>.*)')


def read_fixture(revision):
    if revision:
        return subprocess.check_output(['git', 'show', revision + ':' + FIXTURE])
    return Path(FIXTURE).read_bytes()


def filter_matches(lines, reader=read_fixture):
    cache, kept, allowed = {}, [], 0
    for line in lines:
        hit = HIT.fullmatch(line.rstrip('\n'))
        if hit:
            revision = hit['revision']
            if revision not in cache:
                blob = reader(revision)
                cache[revision] = (blob.decode().splitlines()
                    if (hashlib.sha256(blob).hexdigest() == REVIEWED_SHA256
                        or revision and hashlib.sha256(blob).hexdigest() == HISTORICAL_SHA256) else [])
            contents = cache[revision]
            index = int(hit['line']) - 1
            if index < len(contents) and contents[index] == hit['text']:
                allowed += 1
                continue
        kept.append(line)
    return kept, allowed


if __name__ == '__main__':
    path = Path(sys.argv[1])
    kept, allowed = filter_matches(path.read_text().splitlines(keepends=True))
    path.write_text(''.join(kept))
    print(allowed)
