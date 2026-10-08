import json
from pathlib import Path
import tempfile
import unittest

from run import resource_diagnostics, selection


class ResourceTests(unittest.TestCase):
    def test_diagnostic_case_is_exact_and_within_profile(self):
        self.assertEqual(selection("compression", "12.1.9")["cases"], ["12.1.9"])
        for profile, case in [("compression", "12.*"), ("compression", "9.1.1"), ("core", "12.1.9")]:
            with self.assertRaises(ValueError):
                selection(profile, case)

    def inspect(self, root, oom):
        (root / "container-inspect.json").write_text(json.dumps([
            {"State": {"OOMKilled": oom, "ExitCode": 0}}]))

    def test_oom_is_not_hidden_by_zero_exit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.inspect(root, True)
            self.assertTrue(resource_diagnostics(root)["oom_failure"])

    def test_cgroup_oom_and_peak_are_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "reports").mkdir()
            self.inspect(root, False)
            (root / "reports/memory.events").write_text("max 4\noom 1\noom_kill 1\n")
            (root / "reports/memory.peak").write_text("1073741824\n")
            result = resource_diagnostics(root)
            self.assertTrue(result["oom_failure"])
            self.assertEqual(result["cgroup_memory_peak_bytes"], 1073741824)
            (root / "reports/memory.events").write_text("oom 0\noom_kill 0\n")
            self.assertFalse(resource_diagnostics(root)["oom_failure"])

    def test_missing_container_oom_state_is_not_success(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "container-inspect.json").write_text('[{"State": {}}]')
            with self.assertRaises(ValueError):
                resource_diagnostics(root)


if __name__ == "__main__":
    unittest.main()
