import json
import re
import subprocess
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
CHAIN = ROOT / "services" / "chain"
CONTEXT_DIVISOR = 1024


def reference_fee(table):
    ref = table["reference"]
    row = next(c for c in table["classes"] if c["class"] == ref["class"])
    n_in, n_out = ref["input_tokens"], ref["output_tokens"]
    fee = (
        row["price_input"] * n_in
        + row["price_output"] * n_out
        + row["price_context"] * n_in * n_out // CONTEXT_DIVISOR
    )
    return row, fee


class GenesisPricesTest(unittest.TestCase):
    def test_generated_constants_match_price_table(self):
        table = json.loads((CHAIN / "genesis_prices.json").read_text())
        row, fee = reference_fee(table)
        gen = (CHAIN / "genesis_prices_gen.go").read_text()

        def const(name):
            m = re.search(rf"^\t{name}\s+int64 = (\d+)$", gen, re.M)
            self.assertIsNotNone(m, f"{name} missing from generated file")
            return int(m.group(1))

        self.assertEqual(const("GenesisReferencePriceInput"), row["price_input"])
        self.assertEqual(const("GenesisReferencePriceOutput"), row["price_output"])
        self.assertEqual(const("GenesisReferencePriceContext"), row["price_context"])
        self.assertEqual(const("SubsidyReferenceInput"), table["reference"]["input_tokens"])
        self.assertEqual(const("SubsidyReferenceOutput"), table["reference"]["output_tokens"])
        self.assertEqual(const("SubsidyEquivalentSubunits"), fee)

    def test_generator_check_mode_is_clean(self):
        result = subprocess.run(
            [sys.executable, str(CHAIN / "gen_prices.py"), "--check"],
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
