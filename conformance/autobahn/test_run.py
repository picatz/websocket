from contextlib import redirect_stdout
from io import StringIO
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from run import (COMPRESSION_CASE_COUNT, HERE, IMAGE, PYPY_GC_MAX, container_command,
                 execute_container, inspect_shard, load_json, partition_inventory,
                 reconcile_shards, resource_diagnostics, run_shards, selection,
                 write_aggregate, write_json)


class ResourceTests(unittest.TestCase):
    def test_diagnostic_case_is_exact_and_within_profile(self):
        self.assertEqual(selection("compression", "12.1.9")["cases"], ["12.1.9"])
        for profile, case in [("compression", "12.*"), ("compression", "9.1.1"), ("core", "12.1.9")]:
            with self.assertRaises(ValueError):
                selection(profile, case)

    def inspect(self, root, oom):
        write_json(root / "container-inspect.json", [{"State": {"OOMKilled": oom, "ExitCode": 0}}])

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

    def test_malformed_resources_are_not_success(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "reports").mkdir()
            for content in ('[{"State": {}}]', '{}', '[null]', '[{"State": null}]',
                            '[{"State": {"OOMKilled": 0}}]'):
                (root / "container-inspect.json").write_text(content)
                with self.assertRaises(ValueError):
                    resource_diagnostics(root)
            self.inspect(root, False)
            for content in ("oom 0\n", "oom_kill -1\n", "oom_kill 1\noom_kill 0\n", "oom_kill nope\n"):
                (root / "reports/memory.events").write_text(content)
                with self.assertRaises(ValueError):
                    resource_diagnostics(root)
            (root / "reports/memory.events").write_text("oom_kill 0\n")
            (root / "reports/memory.peak").write_text("-1\n")
            with self.assertRaises(ValueError):
                resource_diagnostics(root)


class ShardTests(unittest.TestCase):
    agent = "picatz-websocket-server-compression"

    def inventory(self):
        return {"all": ["1.1", "12.1.2", "12.1.10", "13.1.1"],
                "selected": ["12.1.2", "12.1.10", "13.1.1"], "excluded": ["1.1"]}

    def row(self, case, behavior="OK", close="OK"):
        return {"behavior": behavior, "behaviorClose": close, "reportfile": case + ".json"}

    def shard(self, number, cases, behavior="OK", close="OK", errors=None):
        return {"number": number, "directory": "shards/" + str(number).zfill(4),
                "cases": cases, "errors": errors or [],
                "report": {self.agent: {case: self.row(case, behavior, close) for case in cases}}}

    def write_report(self, output, inventory, cases, behavior="OK", close="OK"):
        root = output / "reports"
        root.mkdir(exist_ok=True)
        write_json(root / "inventory.json", {"all": inventory["all"], "selected": cases,
                   "excluded": [case for case in inventory["all"] if case not in cases]})
        report = {self.agent: {case: self.row(case, behavior, close) for case in cases}}
        write_json(root / "index.json", report)
        (root / "index.html").write_text("raw index")
        for case, row in report[self.agent].items():
            write_json(root / row["reportfile"], dict(row, id=case, agent=self.agent))
            (root / row["reportfile"]).with_suffix(".html").write_text("raw case")
        return report

    def metadata(self):
        return {"role": "server", "profile": "compression", "agent": self.agent,
                "compression_enabled": True, "case_timeout_seconds": 600,
                "profile_selection": {"cases": ["12.*", "13.*"], "exclude-cases": []},
                "spec": {"cases": ["12.*", "13.*"], "exclude-cases": [],
                         "exclude-agent-cases": {}, "outdir": "/reports",
                         "servers": [{"agent": self.agent, "url": "ws://127.0.0.1:9001"}]}}

    def test_partition_preserves_authoritative_order(self):
        inventory = self.inventory()
        self.assertEqual(partition_inventory(inventory, 2), [["12.1.2", "12.1.10"], ["13.1.1"]])
        self.assertEqual(partition_inventory(inventory, 100), [inventory["selected"]])
        self.assertEqual(partition_inventory(inventory, 1), [[case] for case in inventory["selected"]])

    def test_invalid_partition_rejected(self):
        for size in (0, -1, 1.5, True):
            with self.assertRaises(ValueError):
                partition_inventory(self.inventory(), size)
        for selected in ([], ["12.1.2", "12.1.2"], ["12.*"], [None], ["99.1"]):
            with self.assertRaises(ValueError):
                partition_inventory(dict(self.inventory(), selected=selected), 1)

    def test_complete_aggregate_preserves_both_dimensions_and_failures(self):
        inventory = self.inventory()
        plan = partition_inventory(inventory, 1)
        shards = [self.shard(1, plan[0]), self.shard(2, plan[1], "FAILED", "UNCLEAN"),
                  self.shard(3, plan[2], "UNIMPLEMENTED", "INFORMATIONAL")]
        result, report, links = reconcile_shards(inventory, plan, shards, self.agent)
        self.assertTrue(result["complete"])
        self.assertEqual(result["reported"], 3)
        self.assertEqual(result["behavior"], {"FAILED": 1, "OK": 1, "UNIMPLEMENTED": 1})
        self.assertEqual(result["behavior_close"], {"INFORMATIONAL": 1, "OK": 1, "UNCLEAN": 1})
        self.assertEqual(result["failed_case_ids"], ["12.1.10"])
        self.assertEqual(result["skipped_case_ids"], ["13.1.1"])
        self.assertEqual(set(report), {self.agent})
        self.assertEqual(links["12.1.2"], "../shards/0001/reports/12.1.2.json")

    def test_duplicate_missing_unexpected_and_wrong_plan_fail(self):
        inventory = self.inventory()
        plan = partition_inventory(inventory, 1)
        shards = [self.shard(1, plan[0]), self.shard(2, plan[1]), self.shard(3, plan[2])]
        shards[2]["report"][self.agent] = {"12.1.2": self.row("12.1.2"), "99.1": self.row("99.1")}
        result, _, _ = reconcile_shards(inventory, plan, shards, self.agent)
        self.assertFalse(result["complete"])
        self.assertEqual(result["duplicate_case_ids"], ["12.1.2"])
        self.assertEqual(result["missing"], ["13.1.1"])
        self.assertEqual(result["unexpected"], ["99.1"])
        with self.assertRaises(ValueError):
            reconcile_shards(inventory, list(reversed(plan)), shards, self.agent)

    def test_partial_aggregate_keeps_stopping_cause(self):
        inventory = self.inventory()
        plan = partition_inventory(inventory, 1)
        shards = [self.shard(1, plan[0], errors=["container OOM"])]
        result, _, _ = reconcile_shards(inventory, plan, shards, self.agent, "shard 1: container OOM")
        self.assertFalse(result["complete"])
        self.assertEqual(result["missing"], ["12.1.10", "13.1.1"])
        self.assertEqual(result["reported"], 1)
        self.assertEqual(result["completed_shards"], 0)
        self.assertIn("OOM", result["stopping_cause"])

    def test_malformed_and_wrong_agent_cannot_hide_in_aggregate(self):
        inventory = self.inventory()
        plan = partition_inventory(inventory, 3)
        for key in ("behavior", "behaviorClose"):
            for value in ("UNKNOWN", [], None):
                shard = self.shard(1, plan[0])
                shard["report"][self.agent]["12.1.2"][key] = value
                result, _, _ = reconcile_shards(inventory, plan, [shard], self.agent)
                self.assertFalse(result["complete"])
                self.assertEqual(result["malformed_case_ids"], ["12.1.2"])
        shard["report"] = {"different-agent": {}}
        result, _, _ = reconcile_shards(inventory, plan, [shard], self.agent)
        self.assertFalse(result["complete"])
        self.assertEqual(result["reported"], 0)

    def test_duplicate_json_keys_fail(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for content in ('{"agent": {}, "agent": {}}', '{"agent": {"12.1.2": {}, "12.1.2": {}}}'):
                (root / "index.json").write_text(content)
                with self.assertRaises(ValueError):
                    load_json(root / "index.json")

    def test_shard_inventory_and_every_raw_detail_are_checked(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            inventory = self.inventory()
            cases = ["12.1.2"]
            self.write_report(output, inventory, cases)
            self.assertEqual(inspect_shard(output, inventory, cases, self.agent)[1], [])
            (output / "reports/12.1.2.html").unlink()
            self.assertIn("missing case HTML", inspect_shard(output, inventory, cases, self.agent)[1][0])
            self.write_report(output, inventory, cases)
            write_json(output / "reports/inventory.json", inventory)
            self.assertIn("authoritative plan", inspect_shard(output, inventory, cases, self.agent)[1][0])
            self.write_report(output, inventory, cases)
            detail = load_json(output / "reports/12.1.2.json")
            detail["behaviorClose"] = "FAILED"
            write_json(output / "reports/12.1.2.json", detail)
            self.assertIn("disagrees", inspect_shard(output, inventory, cases, self.agent)[1][0])

    def test_aggregate_links_original_reports(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            (output / "reports").mkdir()
            inventory = self.inventory()
            plan = partition_inventory(inventory, 3)
            shards = [self.shard(1, plan[0])]
            result = write_aggregate(output, inventory, plan, shards, self.agent)
            self.assertTrue(result["complete"])
            html = (output / "reports/index.html").read_text()
            self.assertIn('../shards/0001/reports/12.1.2.html', html)
            self.assertIn("not a certification or all-pass gate", html)
            self.assertEqual(load_json(output / "reports/index.json")[self.agent]["12.1.2"]["reportfile"],
                             "../shards/0001/reports/12.1.2.json")

    def test_invalid_details_are_observed_but_never_classified_or_linked(self):
        for failure in ("missing", "mismatch"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                output = Path(directory)
                (output / "reports").mkdir()
                shard_output = output / "shards/0001"
                shard_output.mkdir(parents=True)
                inventory = self.inventory()
                plan = partition_inventory(inventory, 1)
                self.write_report(shard_output, inventory, plan[0])
                detail = shard_output / "reports/12.1.2.json"
                if failure == "missing":
                    detail.unlink()
                else:
                    row = load_json(detail)
                    row["behavior"] = "FAILED"
                    write_json(detail, row)
                report, errors = inspect_shard(shard_output, inventory, plan[0], self.agent)
                self.assertTrue(errors)
                shard = dict(self.shard(1, plan[0]), report=report, errors=errors)
                result = write_aggregate(output, inventory, plan, [shard], self.agent)
                self.assertEqual(result["reported"], 1)
                self.assertEqual(result["classified"], 0)
                self.assertEqual(result["behavior"], {})
                self.assertEqual(result["behavior_close"], {})
                self.assertEqual(result["unvalidated_case_ids"], ["12.1.2"])
                self.assertFalse(result["complete"])
                html = (output / "reports/index.html").read_text()
                self.assertIn("12.1.2: unvalidated report", html)
                self.assertNotIn("12.1.2.html", html)
                self.assertEqual(load_json(output / "reports/index.json"), {self.agent: {}})

    def test_aggregate_non_ok_links_resolve_to_original_details(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            (output / "reports").mkdir()
            shard_output = output / "shards/0001"
            shard_output.mkdir(parents=True)
            inventory = self.inventory()
            plan = partition_inventory(inventory, 3)
            self.write_report(shard_output, inventory, plan[0], "FAILED", "UNCLEAN")
            report, errors = inspect_shard(shard_output, inventory, plan[0], self.agent)
            self.assertEqual(errors, [])
            shard = dict(self.shard(1, plan[0]), report=report, errors=errors)
            result = write_aggregate(output, inventory, plan, [shard], self.agent)
            self.assertTrue(result["complete"])
            for case, row in result["non_ok_cases"].items():
                expected = shard_output / "reports" / (case + ".json")
                self.assertEqual((output / "reports" / row["reportfile"]).resolve(), expected.resolve())
                self.assertTrue(expected.is_file())
                self.assertEqual(load_json(shard_output / "reports/index.json")[self.agent][case]["reportfile"],
                                 case + ".json")

    def test_unsafe_url_filenames_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            inventory = self.inventory()
            cases = ["12.1.2"]
            for filename in ("..\\..\\elsewhere.json", "case?.json", "case#.json", "case%2f.json", "../elsewhere.json"):
                report = self.write_report(output, inventory, cases)
                report[self.agent][cases[0]]["reportfile"] = filename
                write_json(output / "reports/index.json", report)
                if "/" not in filename:
                    write_json(output / "reports" / filename, dict(report[self.agent][cases[0]], id=cases[0], agent=self.agent))
                    (output / "reports" / filename).with_suffix(".html").write_text("raw case")
                self.assertIn("unsafe case report filename", inspect_shard(output, inventory, cases, self.agent)[1][0])

    def test_shard_directories_must_match_the_numbered_plan_before_linking(self):
        for directory in ("../../elsewhere", "shards/0002", "shards/0001?escape", "shards\\0001"):
            with self.subTest(directory=directory), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary)
                (output / "reports").mkdir()
                inventory = self.inventory()
                plan = partition_inventory(inventory, 3)
                shard = dict(self.shard(1, plan[0]), directory=directory)
                result = write_aggregate(output, inventory, plan, [shard], self.agent)
                self.assertFalse(result["complete"])
                self.assertEqual(result["classified"], 0)
                self.assertEqual(result["unvalidated_case_ids"], sorted(plan[0]))
                html = (output / "reports/index.html").read_text()
                self.assertNotIn('href="../' + directory, html)
                self.assertIn("Invalid shard metadata", html)

    def test_every_container_uses_original_bounds_and_one_binary(self):
        command = container_command(Path("/run/shards/0001"), Path("/run/testee"), "test", self.metadata())
        for flag, value in (("--network", "none"), ("--memory", "1g"), ("--memory-swap", "1g"),
                            ("--cpus", "2"), ("--pids-limit", "128")):
            self.assertEqual(command[command.index(flag) + 1], value)
        self.assertIn("type=bind,src=/run/testee,dst=/testee,readonly", command)
        self.assertIn("PYPY_GC_MAX=" + PYPY_GC_MAX, command)
        self.assertIn(IMAGE, command)
        self.assertEqual(command[-4:], ["server", "true", self.agent, "600s"])

    def test_container_timeout_preserves_inspection_and_resources(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            (output / "reports").mkdir()
            metadata = self.metadata()
            inspection = json.dumps([{"State": {"OOMKilled": True, "ExitCode": 137}}])
            def subprocess_result(command, **kwargs):
                if command[1] == "run":
                    self.assertEqual(kwargs["timeout"], 20)
                    raise subprocess.TimeoutExpired(command, kwargs["timeout"])
                return subprocess.CompletedProcess(command, 0, inspection if command[1] == "inspect" else "", "")
            with patch("run.subprocess.run", side_effect=subprocess_result) as process, patch("run.time.monotonic", return_value=100):
                errors = execute_container(output, output / "testee", "test", metadata, 120)
            self.assertEqual([call.args[0][1] for call in process.call_args_list], ["run", "kill", "inspect", "rm"])
            self.assertTrue(metadata["container_timed_out"])
            self.assertTrue(metadata["resource_diagnostics"]["oom_failure"])
            self.assertTrue(any("deadline" in error for error in errors))
            self.assertTrue(any("OOM" in error for error in errors))
            self.assertTrue((output / "container-inspect.json").is_file())

    def test_zero_exit_container_oom_is_infrastructure_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            (output / "reports").mkdir()
            metadata = self.metadata()
            inspection = json.dumps([{"State": {"OOMKilled": True, "ExitCode": 0}}])
            def complete(command, **kwargs):
                return subprocess.CompletedProcess(command, 0, inspection if command[1] == "inspect" else "", "")
            with patch("run.subprocess.run", side_effect=complete), patch("run.time.monotonic", return_value=100):
                errors = execute_container(output, output / "testee", "test", metadata, 120)
            self.assertEqual(metadata["container_exit_code"], 0)
            self.assertTrue(any("OOM" in error for error in errors))

    def test_inventory_failure_never_starts_peers(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            (output / "reports").mkdir()
            with patch("run.execute_container", return_value=["container OOM"]) as executor:
                with self.assertRaises(RuntimeError):
                    run_shards(output, self.metadata(), 1, 1200)
            self.assertEqual(executor.call_count, 1)
            self.assertTrue(executor.call_args.kwargs["inventory_only"])
            result = load_json(output / "reports/summary.json")
            self.assertFalse(result["complete"])
            self.assertFalse(result["inventory_available"])
            self.assertIsNone(result["missing"])
            self.assertTrue((output / "reports/index.html").is_file())

    def test_no_container_starts_after_deadline(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            with patch("run.subprocess.run") as process, patch("run.time.monotonic", return_value=121):
                errors = execute_container(output, output / "testee", "test", self.metadata(), 120)
            process.assert_not_called()
            self.assertIn("deadline", errors[0])
            self.assertIn("deadline", load_json(output / "metadata.json")["infrastructure_errors"][0])

    def run_synthetic(self, failure=None):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        output = Path(directory.name)
        (output / "reports").mkdir()
        (output / "testee").write_text("one binary")
        cases = ["12.1." + str(number) for number in range(1, COMPRESSION_CASE_COUNT + 1)]
        inventory = {"all": ["1.1"] + cases, "selected": cases, "excluded": ["1.1"]}
        clock = [0]
        deadlines = []
        def execute(path, binary, name, metadata, deadline, inventory_only=False):
            self.assertEqual(binary, output / "testee")
            deadlines.append(deadline)
            if inventory_only:
                write_json(path / "reports/inventory.json", inventory)
                clock[0] += 1
                return []
            number = metadata["shard_number"]
            selected = metadata["shard_cases"]
            self.write_report(path, inventory, selected, "FAILED" if number == 1 else "UNIMPLEMENTED")
            clock[0] += 1
            if failure == "timeout" and number == 2:
                clock[0] = 1200
            if failure == "oom" and number == 2:
                return ["container OOM"]
            if failure == "missing" and number == 2:
                (path / "reports/index.json").unlink()
            return []
        with patch("run.execute_container", side_effect=execute) as executor, \
                patch("run.time.monotonic", side_effect=lambda: clock[0]), redirect_stdout(StringIO()):
            if failure:
                with self.assertRaises(RuntimeError):
                    run_shards(output, self.metadata(), 1, 1200)
            else:
                run_shards(output, self.metadata(), 1, 1200)
        self.assertTrue(all(deadline == 1200 for deadline in deadlines))
        return output, executor.call_count

    def test_all_216_cases_continue_through_protocol_failures(self):
        output, calls = self.run_synthetic()
        summary = load_json(output / "reports/summary.json")
        self.assertEqual(calls, 217)  # One inventory-only run, then all 216 cases.
        self.assertTrue(summary["complete"])
        self.assertEqual(summary["selected"], 216)
        self.assertEqual(summary["reported"], 216)
        self.assertEqual(summary["behavior"], {"FAILED": 1, "UNIMPLEMENTED": 215})
        self.assertEqual(summary["duplicate_case_ids"], [])
        self.assertEqual(summary["missing"], [])
        self.assertEqual(summary["unexpected"], [])
        self.assertEqual(len(list(output.rglob("testee"))), 1)
        metadata = load_json(output / "metadata.json")
        self.assertEqual(metadata["spec"]["cases"], ["12.*", "13.*"])
        self.assertEqual(len(metadata["shard_plan"]), 216)

    def test_infrastructure_failures_stop_after_first_bad_shard(self):
        for failure in ("oom", "missing", "timeout"):
            with self.subTest(failure=failure):
                output, calls = self.run_synthetic(failure)
                summary = load_json(output / "reports/summary.json")
                self.assertEqual(calls, 3)
                self.assertFalse(summary["complete"])
                self.assertEqual(summary["started_shards"], 2)
                self.assertEqual(len(summary["missing"]), 215 if failure == "missing" else 214)
                self.assertIsNotNone(summary["stopping_cause"])
                self.assertFalse((output / "shards/0003").exists())

    def test_cli_rejects_partial_diagnostic_with_sharding(self):
        for args in (["--role", "server", "--profile", "compression", "--case", "12.1.9"],
                     ["--role", "client", "--profile", "compression"],
                     ["--role", "server", "--profile", "core"]):
            result = subprocess.run(["python3", str(HERE / "run.py")] + args +
                                    ["--shard-size", "1", "--output", "/unused"],
                                    text=True, capture_output=True)
            self.assertEqual(result.returncode, 2)
            self.assertTrue("sharding" in result.stderr)


class PeerLivenessTests(unittest.TestCase):
    def invoke_inside(self, role, peer_exits):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "reports").mkdir()
            # Replace only absolute fixture paths; run the actual supervisor
            # logic with the system shell and disposable fake peer commands.
            script = (HERE / "inside.sh").read_text()
            for name in ("testee", "reports", "harness", "config"):
                suffix = " " if name == "testee" else "/"
                script = script.replace("/" + name + suffix, str(root / name) + suffix)
            (root / "inside.sh").write_text(script)
            (root / "python").write_text("#!/bin/sh\nexit 0\n")
            peer = "exit 0" if peer_exits else "exec sleep 5"
            (root / "testee").write_text(
                '#!/bin/sh\nif [ "$2" = server ]; then ' + peer + '; else sleep 0.1; fi\n')
            (root / "wstest").write_text(
                '#!/bin/sh\nif [ "$1" = -a ]; then exit 0; fi\n'
                'if [ "$2" = fuzzingserver ]; then ' + peer + '; else sleep 0.1; fi\n')
            for name in ("python", "testee", "wstest"):
                (root / name).chmod(0o755)
            return subprocess.run(["/bin/sh", str(root / "inside.sh"), role, "true", "test", "600s"],
                                  text=True, capture_output=True, timeout=3,
                                  env=dict(os.environ, PATH=str(root) + ":" + os.environ["PATH"]))

    def test_dead_background_peer_is_not_hidden_by_foreground_success(self):
        for role in ("server", "client"):
            with self.subTest(role=role):
                result = self.invoke_inside(role, True)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn("background peer exited unexpectedly (status 0)", result.stderr)

    def test_live_peers_and_inventory_only_finish_normally(self):
        for role in ("server", "client", "inventory"):
            with self.subTest(role=role):
                result = self.invoke_inside(role, False)
                self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
