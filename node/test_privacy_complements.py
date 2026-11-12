import json
from pathlib import Path
import tempfile
import unittest
from privacy.redaction import measure,redact,rehydrate
from privacy.canary import plant,scan


class ComplementTests(unittest.TestCase):
    def test_golden_measurement_and_rehydration(self):
        cases=json.loads((Path(__file__).resolve().parents[1]/'testdata/privacy/redaction-golden.json').read_text())
        result=measure(cases)
        print('REDACTION_METRICS',json.dumps(result,sort_keys=True))
        self.assertEqual(result['lossless'],len(cases))
        self.assertGreater(result['tp'],0)
        address='alice' + '@' + 'example.org'
        text=f'{address} twice {address} <EMBER_REDACTED_0>'
        redacted,mapping=redact(text)
        self.assertNotIn(address,redacted)
        self.assertEqual(rehydrate(redacted,mapping),text)
    def test_canary_is_telemetry(self):
        prompt,record=plant('hello',['executor','gateway','relay'])
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'surface';path.write_text(prompt)
            findings=scan([record],[path])
            self.assertEqual(findings[0]['path_participants'],['executor','gateway','relay'])
            self.assertTrue(findings[0]['telemetry_only'])
            path.write_text('nothing');self.assertEqual(scan([record],[path]),[])
