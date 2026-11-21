import pytest
from auditor.spot import audit, verdict_kind, SPOT_ESCALATION_BLOCKS

@pytest.mark.parametrize('corroborated', [False, True])
@pytest.mark.parametrize('additional', [False, True])
def test_bounded_verdict(corroborated, additional):
    context = dict(version=15, first_per_step=[dict(step=3, outcome='mismatch'), dict(step=7, outcome='match')])
    results = [dict(step=3, outcome='mismatch' if corroborated else 'match'),
               dict(step=9, outcome='mismatch' if additional else 'match')]
    expected = 'escalation_bounded_reject' if corroborated and additional else 'escalation_bounded_accept'
    assert verdict_kind(context, results, 'escalation') == expected
    assert SPOT_ESCALATION_BLOCKS == 600


def test_bounded_replays_only_challenged_steps(monkeypatch):
    import auditor.spot as spot
    calls = []
    class Spool:
        def get(self, context, step):
            calls.append(step)
            return {}
    monkeypatch.setattr(spot, 'audit_step', lambda *a: dict(step=calls[-1], outcome='match', first_divergence_distance=0.0))
    context = dict(version=15, total_steps=94, steps=[3, 9], first_per_step=[dict(step=3, outcome='mismatch')])
    result = audit(context, Spool(), None, None, None, mode='escalation')
    assert calls == [3, 9]
    assert result['verdict_kind'] == 'escalation_bounded_accept'

@pytest.mark.parametrize('corroborated', [False, True])
@pytest.mark.parametrize('additional', [False, True])
def test_pipeline_bounded_replay_matrix(tmp_path, monkeypatch, corroborated, additional):
    from test_spot_pipeline import fixture
    from auditor.spot import DirectorySpool, durable_json
    from types import SimpleNamespace
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    import auditor.spot_pipeline as pipeline
    context, packages, actual = fixture()
    context.update(version=15, steps=[1, 2], first_per_step=[dict(step=1, outcome='mismatch')])
    durable_json(tmp_path/'opening.json', dict(kind='opening', request_id=context['request_id'], challenge_height=10, packages=packages))
    # Real Merkle/record binding and prefix execution; control only the numeric
    # distances so all four adjudication branches are independent of hardware.
    distances = iter([1.0 if corroborated else 0.0, 1.0 if additional else 0.0])
    monkeypatch.setattr(pipeline, 'compare', lambda *a: (next(distances), {'reason': 'test distance'}))
    calls = []
    def replay(prompt, seed, tokens):
        calls.append(tokens)
        return actual
    result = audit(context, DirectorySpool(tmp_path), SimpleNamespace(replay=replay),
                   Ed25519PrivateKey.generate(), tmp_path/'evidence', mode='escalation', height=20)
    assert calls == [[7, 8]]
    assert result['verdict_kind'] == ('escalation_bounded_reject' if corroborated and additional else 'escalation_bounded_accept')
    assert [r['step'] for r in result['per_step']] == context['steps']


def test_pipeline_bounded_subset_needs_no_full_job(tmp_path):
    from test_spot_pipeline import fixture
    from auditor.spot import DirectorySpool, durable_json
    from types import SimpleNamespace
    context, packages, actual = fixture()
    context.update(version=15, steps=[1], first_per_step=[dict(step=1, outcome='mismatch')])
    durable_json(tmp_path/'opening.json', dict(kind='opening', request_id=context['request_id'], challenge_height=10, packages=[packages[0]]))
    calls = []
    def replay(prompt, seed, tokens):
        calls.append(tokens)
        return actual[:len(tokens)]
    result = audit(context, DirectorySpool(tmp_path), SimpleNamespace(replay=replay), None, tmp_path/'evidence', mode='escalation', height=20)
    assert calls == [[7]]
    assert result['verdict_kind'] == 'escalation_bounded_accept'
    assert len(result['per_step']) == 1


def test_finalized_context_overrides_local_escalation_claims(tmp_path):
    import base64
    from test_spot_context import setup
    from auditor.spot_worker import context_for
    from auditor.spot_wire import SpotVerdict
    agent, frame, job, hold, query, _ = setup(tmp_path)
    first = SpotVerdict(verdict='reject', verdict_kind='reject_step')
    first.per_step.add(step=1, outcome='mismatch')
    challenge = hold['pipeline']['spot']
    challenge.update(round='esc', step_set=[1, 2], first_verdict=base64.b64encode(first.SerializeToString()).decode())
    def finalized_query(path):
        return dict(version=15) if path == '/state' else query(path)
    context = context_for(agent, frame, hold, job, finalized_query)
    assert context['version'] == 15
    assert context['steps'] == [1, 2]
    assert context['first_per_step'] == [dict(step=1, outcome='mismatch')]
