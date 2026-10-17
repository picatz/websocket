import unittest

from gate import rejected
from summarize import summarize


class GateTests(unittest.TestCase):
    inventory = {"all": ["1.1", "1.2", "9.1"], "selected": ["1.1", "1.2"], "excluded": ["9.1"]}

    def summary(self, **outcomes):
        results = {case.replace("_", "."): {"behavior": b, "behaviorClose": c}
                   for case, (b, c) in outcomes.items()}
        return summarize(self.inventory, {"test": results}, "test")

    def test_ok_and_informational_are_accepted(self):
        self.assertEqual(rejected(self.summary(
            **{"1_1": ("OK", "OK"), "1_2": ("INFORMATIONAL", "INFORMATIONAL")})), [])

    def test_every_other_outcome_is_rejected(self):
        for behavior, close in (("FAILED", "OK"), ("OK", "UNCLEAN"), ("NON-STRICT", "OK"),
                                ("OK", "WRONG CODE"), ("OK", "FAILED BY CLIENT"),
                                ("UNIMPLEMENTED", "OK")):
            with self.subTest(behavior=behavior, close=close):
                got = rejected(self.summary(**{"1_1": ("OK", "OK"), "1_2": (behavior, close)}))
                self.assertEqual(got, ["1.2: " + behavior + " / " + close])

    def test_incomplete_summary_has_no_verdict(self):
        incomplete = summarize(self.inventory, {"test": {
            "1.1": {"behavior": "OK", "behaviorClose": "OK"}}}, "test")
        with self.assertRaises(ValueError):
            rejected(incomplete)
        with self.assertRaises(ValueError):
            rejected(dict(self.summary(**{"1_1": ("OK", "OK"), "1_2": ("OK", "OK")}), reported=1))


if __name__ == "__main__":
    unittest.main()
