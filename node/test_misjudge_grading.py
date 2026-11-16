import hashlib
import unittest

from auditor.grading import offense_grade
from ember_pb2 import ToleranceBand


class MisjudgeGradingTest(unittest.TestCase):
    def test_signed_band_and_prefix_class(self):
        band = ToleranceBand(producer_engine='gb10', reference_engine='thor', tolerance=.4)
        band_id = hashlib.sha256(band.SerializeToString(deterministic=True)).hexdigest()
        hold = {'model_id': 'model', 'audit_output_tokens': 3, 'misjudge_thresholds': [
            {'model': 'model', 'band': band_id, 'length_class': 'short', 'min_output': 0, 'max_output': 128}]}
        verdict, grade = offense_grade('dispute', .44, 'numeric mismatch', band, hold)
        self.assertEqual(verdict, 'reject')
        self.assertEqual(getattr(grade, 'class'), 'numerical')
        self.assertEqual((grade.band, grade.length_class, grade.score), (band_id, 'short', .44))
        hold['audit_output_tokens'] = 129
        self.assertEqual(offense_grade('dispute', .44, '', band, hold), ('inconclusive', None))
        self.assertEqual(offense_grade('dispute', float('nan'), '', band, hold), ('inconclusive', None))

    def test_only_current_deterministic_faults_are_objective(self):
        outcome, grade = offense_grade('reject', float('inf'), 'commitment mismatch', None, {})
        self.assertEqual((outcome, getattr(grade, 'class'), grade.fault), ('reject', 'objective', 'hash'))
        for reason in ('input token count mismatch', 'unknown failure'):
            self.assertEqual(offense_grade('reject', float('inf'), reason, None, {}), ('inconclusive', None))


if __name__ == '__main__':
    unittest.main()
