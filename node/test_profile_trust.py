import json
import tempfile
import unittest
from pathlib import Path

from agent.trust import check_model_trust


class ProfileTrustTests(unittest.TestCase):
    def test_revocation_distinct_accounts_and_conservative_default(self):
        state = {'models': {'p': {'publication': {'publisher': 'alice', 'manifest_root': 'root'}}},
                 'governors': {'governor': True},
                 'endorsements': {'root': {'bob': {'revoked': False}}}}
        with self.assertRaises(ValueError):
            check_model_trust(state, 'p')
        with tempfile.TemporaryDirectory() as d:
            policy = Path(d) / 'trust.json'
            policy.write_text(json.dumps({'profiles': {'p': {'min_endorsers': 2}}}))
            with self.assertRaises(ValueError):
                check_model_trust(state, 'p', policy)
            state['endorsements']['root']['carol'] = {'revoked': False}
            check_model_trust(state, 'p', policy)
            state['endorsements']['root']['bob']['revoked'] = True
            with self.assertRaises(ValueError):
                check_model_trust(state, 'p', policy)
            policy.write_text(json.dumps({'profiles': {'p': {'publishers': ['alice'], 'min_endorsers': 3}}}))
            check_model_trust(state, 'p', policy)
            state['models']['p']['publication']['invalid'] = True
            with self.assertRaises(ValueError):
                check_model_trust(state, 'p', policy)

    def test_legacy_and_governor_endorsement(self):
        state = {'models': {'old': {}, 'new': {'publication': {'publisher': 'alice', 'manifest_root': 'r'}}},
                 'governors': {'governor': True}, 'endorsements': {}}
        check_model_trust(state, 'old')
        state['endorsements']['r'] = {'governor': {'revoked': False}}
        check_model_trust(state, 'new')
        state['endorsements']['r']['governor']['revoked'] = True
        with self.assertRaises(ValueError):
            check_model_trust(state, 'new')


if __name__ == '__main__':
    unittest.main()
