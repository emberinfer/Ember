import unittest
from auditor.pnl import epoch_report


class AuditorPNLTest(unittest.TestCase):
    def test_paid_unsettled_missing_cost_and_idle_auditors(self):
        work = dict(auditor='a', epoch=0, replayed=True)
        state = dict(auditors={'a': True, 'idle': True}, holds={
            'paid': dict(audit_works=[work], audit_receipts=[dict(
                audit_comp=dict(auditor='a', comp_subunits=20), audit_shortfall_subunits=4)]),
            'pending': dict(audit_works=[dict(work, epoch=1)])})
        report = epoch_report(state, [dict(hold_id='paid', auditor='a', cost_subunits=18)])
        rows = {(r['epoch'], r['auditor']): r for r in report['rows']}
        self.assertEqual(rows[0, 'a']['pnl_subunits'], 2)
        self.assertEqual(rows[0, 'a']['share_shortfalls'], 4)
        self.assertIsNone(rows[1, 'a']['pnl_subunits'])
        self.assertEqual(rows[1, 'a']['comp_earned'], 0)
        self.assertEqual(rows[0, 'idle']['assigned'], 0)
        self.assertEqual(rows[1, 'idle']['pnl_subunits'], 0)

    def test_duplicate_costs_fail_closed(self):
        cost = dict(hold_id='h', auditor='a', cost_subunits=18)
        with self.assertRaises(ValueError):
            epoch_report({}, [cost, cost])
