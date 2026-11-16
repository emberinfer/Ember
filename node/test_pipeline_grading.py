"""Pipeline replay grading and CLI output consumed by the signing daemon."""
import contextlib
import io
import json
import unittest
from types import SimpleNamespace
from unittest.mock import patch
from auditor import __main__ as worker
from auditor.pipeline import pipeline_offense


class PipelineGradingTest(unittest.TestCase):
    def test_worker_verdicts(self):
        cases = [
            ('accept', [('accept', .1, 'ok', [])], None),
            ('reject', [('dispute', .6, 'far', ['stage']),
                        ('dispute', .7, 'far', ['stage'])], .7),
            ('inconclusive', [('dispute', .6, 'far', ['stage']),
                              ('accept', .1, 'ok', [])], None),
        ]
        for expected, results, score in cases:
            with self.subTest(verdict=expected):
                engine = SimpleNamespace(device='cpu', close=lambda: None)
                output = io.StringIO()
                with patch.object(worker, 'load_engine', return_value=engine), \
                     patch.object(worker, 'load_pipeline_policy', return_value=('metric', .5, 'ref')), \
                     patch.object(worker, 'audit_pipeline', side_effect=results), \
                     contextlib.redirect_stdout(output):
                    worker.pipeline_main({'engine': 'mock'})
                result = json.loads(output.getvalue())
                self.assertEqual(result['verdict'], expected)
                if score is None:
                    self.assertIsNone(result['offense'])
                else:
                    self.assertEqual(result['offense'], {
                        'class': 'numerical', 'score': score, 'band': 'pipeline-replay-v1'})
                    grade = pipeline_offense(expected, score)
                    self.assertEqual(grade.length_class, '')
                    self.assertEqual(grade.fault, '')
                    self.assertEqual(result['fault_node_ids'], ['stage'])
