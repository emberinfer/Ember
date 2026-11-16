"""Real node-side host for standalone Engine SPI adapter conformance."""
import argparse
import importlib.util
import json
import math
from pathlib import Path
import sys


CASES = (('hello', 7), ('Reply briefly: conformance', 19))


def load_adapter(path, endpoint):
    path = Path(path).resolve()
    sys.path.insert(0, str(path.parent))
    spec = importlib.util.spec_from_file_location('ember_external_adapter', path)
    if spec is None or spec.loader is None:
        raise ValueError('cannot load adapter module')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.create_adapter(endpoint)


def validate_step(step):
    if type(step.token) is not int or step.token < 0 or not isinstance(step.text, str):
        raise ValueError('invalid token or text')
    if not isinstance(step.topk, list) or not step.topk:
        raise ValueError('missing top-k evidence')
    seen = set()
    for ident, score in step.topk:
        if type(ident) is not int or ident < 0 or ident in seen or not math.isfinite(score):
            raise ValueError('invalid top-k entry')
        seen.add(ident)
    if step.token not in seen or not isinstance(step.activations, dict):
        raise ValueError('chosen token/evidence mismatch')


def main(argv=None):
    parser = argparse.ArgumentParser(prog='ember-node conformance')
    parser.add_argument('--adapter', required=True)
    parser.add_argument('--endpoint', required=True)
    args = parser.parse_args(argv)
    adapter = load_adapter(args.adapter, args.endpoint)
    caps = adapter.capabilities()
    if caps.spi_version != 'ember-engine-spi-v1' or not caps.engine_name:
        raise ValueError('invalid SPI capabilities')
    rows = []
    for prompt, seed in CASES:
        steps = list(adapter.generate(prompt, seed, 2))
        if not 1 <= len(steps) <= 2:
            raise ValueError('adapter returned invalid token count')
        for step in steps:
            validate_step(step)
        replay = list(adapter.replay(prompt, seed, [step.token for step in steps]))
        if [step.token for step in replay] != [step.token for step in steps]:
            raise ValueError('forced-prefix replay changed token IDs')
        rows.append({'prompt': prompt, 'seed': seed, 'tokens': len(steps),
                     'text': ''.join(step.text for step in steps)})
    adapter.close()
    print(json.dumps({'result': 'PASS', 'engine': caps.engine_name,
                      'spi_version': caps.spi_version, 'cases': rows}, sort_keys=True))
