import json
from pathlib import Path
import tempfile
import unittest

from summarize import summarize, validate_details


class SummaryTests(unittest.TestCase):
    def result(self, behavior="OK", close="OK"):
        return {"behavior": behavior, "behaviorClose": close}

    def inventory(self):
        return {"all": ["1.1", "1.2", "9.1"], "selected": ["1.1", "1.2"], "excluded": ["9.1"]}

    def test_both_dimensions_and_exclusions_are_visible(self):
        result = summarize(self.inventory(), {"test": {
            "1.1": self.result("OK", "UNCLEAN"), "1.2": self.result("UNIMPLEMENTED")}}, "test")
        self.assertTrue(result["complete"])
        self.assertEqual(result["failed_case_ids"], ["1.1"])
        self.assertEqual(result["skipped_case_ids"], ["1.2"])
        self.assertEqual(result["excluded"], 1)
        self.assertEqual(len(result["non_ok_cases"]), 2)

    def test_missing_and_unexpected_cases_are_not_success(self):
        result = summarize(self.inventory(), {"test": {"1.1": self.result(), "8.1": self.result()}}, "test")
        self.assertFalse(result["complete"])
        self.assertEqual(result["missing"], ["1.2"])
        self.assertEqual(result["unexpected"], ["8.1"])

    def test_warnings_are_not_relabelled_failures(self):
        result = summarize(self.inventory(), {"test": {
            "1.1": self.result("NON-STRICT", "WRONG CODE"),
            "1.2": self.result("INFORMATIONAL", "FAILED BY CLIENT")}}, "test")
        self.assertEqual(result["failed_case_ids"], [])
        self.assertEqual(len(result["non_ok_cases"]), 2)

    def test_missing_agent_and_unknown_outcomes_fail(self):
        with self.assertRaises(ValueError):
            summarize(self.inventory(), {}, "test")
        with self.assertRaises(ValueError):
            summarize(self.inventory(), {"test": {"1.1": self.result("UNKNOWN")}}, "test")

    def test_invalid_inventory_fails(self):
        for selected in ([], ["1.1", "1.1"], ["absent"]):
            inventory = dict(self.inventory(), selected=selected)
            with self.assertRaises(ValueError):
                summarize(inventory, {"test": {}}, "test")

    def test_incomplete_detail_generation_fails(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "index.html").write_text("report")
            result = dict(self.result(), reportfile="case.json")
            report = {"test": {"1.1": result}}
            with self.assertRaises(FileNotFoundError):
                validate_details(root, report, "test")
            detail = dict(result, id="1.1", agent="test")
            (root / "case.json").write_text(json.dumps(detail))
            with self.assertRaises(ValueError):
                validate_details(root, report, "test")
            (root / "case.html").write_text("report")
            validate_details(root, report, "test")
            detail["behavior"] = "FAILED"
            (root / "case.json").write_text(json.dumps(detail))
            with self.assertRaises(ValueError):
                validate_details(root, report, "test")
            result["reportfile"] = "../elsewhere.json"
            with self.assertRaises(ValueError):
                validate_details(root, report, "test")


if __name__ == "__main__":
    unittest.main()
