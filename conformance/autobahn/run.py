#!/usr/bin/env python3
"""Build and run one isolated Autobahn role/profile on a Docker-capable host."""
import argparse
from collections import Counter
from fnmatch import fnmatchcase
from html import escape
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

from summarize import BEHAVIOR, CLOSE, summarize, validate_details

IMAGE = "crossbario/autobahn-testsuite:25.10.1@sha256:519915fb568b04c9383f70a1c405ae3ff44ab9e35835b085239c258b6fac3074"
SOURCE = "6ed6f439dc7ed0d7432fe2cf7481b110905ecc5c"
# Bound the legacy suite's GC-managed heap below the shared container limit.
# This is not a total-RSS limit and does not configure the Go testee's runtime.
PYPY_GC_MAX = "512MB"
RUN_TIMEOUT = 1200
COMPRESSION_CASE_COUNT = 216
HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
PROFILES = {
    "core": {"cases": ["*"], "exclude-cases": ["9.*", "12.*", "13.*"]},
    "compression": {"cases": ["12.*", "13.*"], "exclude-cases": []},
    "limits": {"cases": ["9.*"], "exclude-cases": []},
}


def run(*args, **kwargs):
    return subprocess.run(args, check=True, text=True, **kwargs)


def selection(profile, case):
    spec = dict(PROFILES[profile])
    if case:
        if (not re.fullmatch(r"[0-9]+(?:\.[0-9]+)+", case)
                or not any(fnmatchcase(case, pattern) for pattern in spec["cases"])
                or any(fnmatchcase(case, pattern) for pattern in spec["exclude-cases"])):
            raise ValueError("diagnostic case must be one exact ID within the selected profile")
        spec["cases"] = [case]
    return spec


def resource_diagnostics(output):
    """An OOM is an infrastructure failure even if a shell later exits zero."""
    inspection = load_json(output / "container-inspect.json")
    if (not isinstance(inspection, list) or len(inspection) != 1
            or not isinstance(inspection[0], dict) or not isinstance(inspection[0].get("State"), dict)
            or not isinstance(inspection[0]["State"].get("OOMKilled"), bool)):
        raise ValueError("missing or malformed container OOM state")
    result = {"container_oom_killed": inspection[0]["State"]["OOMKilled"]}
    events = output / "reports" / "memory.events"
    if events.is_file():
        counters = {}
        for line in events.read_text().splitlines():
            name, count = line.split()
            if name in counters:
                raise ValueError("duplicate cgroup memory counter")
            counters[name] = int(count)
            if counters[name] < 0:
                raise ValueError("negative cgroup memory counter")
        if "oom_kill" not in counters:
            raise ValueError("cgroup memory events omit oom_kill")
        result["cgroup_memory_events"] = counters
    peak = output / "reports" / "memory.peak"
    if peak.is_file():
        result["cgroup_memory_peak_bytes"] = int(peak.read_text())
        if result["cgroup_memory_peak_bytes"] < 0:
            raise ValueError("negative cgroup memory peak")
    result["oom_failure"] = (result["container_oom_killed"]
                             or result.get("cgroup_memory_events", {}).get("oom_kill", 0) > 0)
    return result


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def load_json(path):
    # Duplicate JSON keys must not silently discard duplicate cases or agents.
    def unique_keys(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate JSON key: " + key)
            result[key] = value
        return result
    return json.loads(path.read_text(), object_pairs_hook=unique_keys)


def partition_inventory(inventory, size):
    """Preserve the suite's authoritative order, with no host-side filtering."""
    if isinstance(size, bool) or not isinstance(size, int) or size < 1:
        raise ValueError("shard size must be a positive integer")
    if not isinstance(inventory, dict) or set(inventory) != {"all", "selected", "excluded"}:
        raise ValueError("malformed authoritative inventory")
    for values in inventory.values():
        if (not isinstance(values, list) or any(not isinstance(case, str)
                or not re.fullmatch(r"[0-9]+(?:\.[0-9]+)+", case) for case in values)):
            raise ValueError("inventory must contain exact case IDs")
    # Reuse the report summarizer's nonempty/unique/exact-partition checks.
    summarize(inventory, {"inventory": {}}, "inventory")
    selected = inventory["selected"]
    return [selected[start:start + size] for start in range(0, len(selected), size)]


def container_command(output, binary, name, metadata, inventory_only=False):
    return [
        "docker", "run", "--name", name, "--platform", "linux/amd64",
        "--network", "none", "--read-only", "--cap-drop", "ALL",
        "--security-opt", "no-new-privileges", "--pids-limit", "128",
        "--memory", "1g", "--memory-swap", "1g", "--cpus", "2",
        "--user", str(os.getuid()) + ":" + str(os.getgid()),
        "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
        "--env", "PYTHONDONTWRITEBYTECODE=1", "--env", "PYTHONUNBUFFERED=1", "--env", "HOME=/tmp",
        "--env", "PYPY_GC_MAX=" + PYPY_GC_MAX,
        "--mount", "type=bind,src=" + str(binary) + ",dst=/testee,readonly",
        "--mount", "type=bind,src=" + str(HERE) + ",dst=/harness,readonly",
        "--mount", "type=bind,src=" + str(output / "config") + ",dst=/config,readonly",
        "--mount", "type=bind,src=" + str(output / "reports") + ",dst=/reports",
        "--entrypoint", "/bin/sh", IMAGE, "/harness/inside.sh",
        "inventory" if inventory_only else metadata["role"],
        str(metadata["compression_enabled"]).lower(), metadata["agent"],
        str(metadata["case_timeout_seconds"]) + "s",
    ]


def execute_container(output, binary, name, metadata, deadline, inventory_only=False):
    """Return infrastructure errors, retaining evidence even on a deadline/OOM."""
    command = container_command(output, binary, name, metadata, inventory_only)
    write_json(output / "command.json", command)
    write_json(output / "metadata.json", metadata)
    errors = []
    started = time.monotonic()
    remaining = deadline - started
    if remaining <= 0:
        errors.append("aggregate execution deadline exceeded before container launch")
        metadata["infrastructure_errors"] = errors
        write_json(output / "metadata.json", metadata)
        return errors
    try:
        with (output / "container.log").open("w") as log:
            completed = subprocess.run(command, text=True, stdout=log,
                                       stderr=subprocess.STDOUT, timeout=remaining)
        metadata["container_exit_code"] = completed.returncode
        if completed.returncode:
            errors.append("container exited with status " + str(completed.returncode))
    except subprocess.TimeoutExpired:
        metadata["container_timed_out"] = True
        errors.append("aggregate execution deadline exceeded while running container")
    except OSError as error:
        errors.append("container launch failed: " + str(error))
    finally:
        # Cleanup is always attempted, even after the aggregate deadline. Its
        # bounded calls do not grant the next shard a fresh execution budget.
        for action in (["kill", name], ["inspect", name], ["rm", "-f", name]):
            try:
                result = subprocess.run(["docker"] + action, text=True,
                                        capture_output=True, timeout=10)
                if action[0] == "inspect":
                    (output / "container-inspect.json").write_text(result.stdout or result.stderr)
                    if result.returncode:
                        errors.append("container inspection failed: " + result.stderr.strip())
                elif action[0] == "rm" and result.returncode:
                    errors.append("container removal failed: " + result.stderr.strip())
            except (OSError, subprocess.TimeoutExpired) as error:
                errors.append("container " + action[0] + " failed: " + str(error))
        try:
            metadata["resource_diagnostics"] = resource_diagnostics(output)
            if metadata["resource_diagnostics"]["oom_failure"]:
                errors.append("container OOM: resource failure, not a protocol verdict")
        except (OSError, ValueError, TypeError, AttributeError) as error:
            errors.append("invalid resource evidence: " + str(error))
        metadata["elapsed_seconds"] = time.monotonic() - started
        if time.monotonic() >= deadline and not any("deadline" in error for error in errors):
            errors.append("aggregate execution deadline exceeded")
        metadata["infrastructure_errors"] = errors
        write_json(output / "metadata.json", metadata)
    return errors


def safe_reportfile(filename):
    # Suite-generated filenames are simple ASCII basenames. In particular,
    # backslashes are URL separators in browsers even on a Linux filesystem.
    return isinstance(filename, str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*\.json", filename) is not None


def safe_shard_directory(shard, number):
    return (type(shard["number"]) is int and shard["number"] == number
            and shard["directory"] == "shards/" + str(number).zfill(4))


def inspect_shard(output, inventory, cases, agent):
    """Validate raw shard details before an outcome contributes to the baseline."""
    reports = output / "reports"
    errors = []
    report = {}
    try:
        report = load_json(reports / "index.json")
        if not isinstance(report, dict) or set(report) != {agent} or not isinstance(report[agent], dict):
            raise ValueError("report agents do not match the requested single agent")
        shard_inventory = load_json(reports / "inventory.json")
        expected = {"all": inventory["all"], "selected": cases,
                    "excluded": [case for case in inventory["all"] if case not in cases]}
        if shard_inventory != expected:
            raise ValueError("shard inventory differs from the authoritative plan")
        summary = summarize(shard_inventory, report, agent)
        if any(not safe_reportfile(row.get("reportfile")) for row in report[agent].values()):
            raise ValueError("unsafe case report filename")
        validate_details(reports, report, agent)
        write_json(reports / "summary.json", summary)
        if not summary["complete"]:
            raise ValueError("incomplete or unexpected shard case coverage")
    except (OSError, ValueError, TypeError, KeyError, AttributeError) as error:
        errors.append("invalid shard reports: " + str(error))
    return report, errors


def reconcile_shards(inventory, plan, shards, agent, stopping_cause=None):
    """Reconcile all observed IDs, retaining failures and partial-run evidence."""
    flattened = [case for batch in plan for case in batch]
    if flattened != inventory["selected"] or any(not batch for batch in plan):
        raise ValueError("shard plan does not match the ordered selected inventory")
    partition_inventory(inventory, 1)
    seen = Counter()
    malformed = set()
    unvalidated = set()
    valid = {}
    links = {}
    infrastructure = []
    for index, shard in enumerate(shards):
        matches_plan = (index < len(plan) and safe_shard_directory(shard, index + 1)
                        and shard["cases"] == plan[index])
        if not matches_plan:
            infrastructure.append("executed shard differs from the authoritative plan")
        infrastructure.extend(shard["errors"])
        report = shard["report"]
        if not isinstance(report, dict) or set(report) != {agent} or not isinstance(report.get(agent), dict):
            if not shard["errors"]:
                infrastructure.append("invalid report agent in " + shard["directory"])
            continue
        for case, result in report[agent].items():
            seen[case] += 1
            if (not isinstance(result, dict) or not isinstance(result.get("behavior"), str)
                    or result["behavior"] not in BEHAVIOR
                    or not isinstance(result.get("behaviorClose"), str)
                    or result["behaviorClose"] not in CLOSE):
                malformed.add(case)
                continue
            filename = result.get("reportfile")
            if shard["errors"] or not matches_plan or not safe_reportfile(filename):
                unvalidated.add(case)
                continue
            if case not in valid:
                valid[case] = result
                links[case] = "../" + shard["directory"] + "/reports/" + filename
    unvalidated.update(malformed)
    unvalidated.update(case for case, count in seen.items() if count > 1)
    for case in unvalidated:
        valid.pop(case, None)
        links.pop(case, None)
    result = summarize(inventory, {agent: valid}, agent)
    result.update({
        "reported": sum(seen.values()), "reported_unique": len(seen),
        "classified": len(valid), "reported_case_ids": sorted(seen),
        "missing": sorted(set(inventory["selected"]) - set(seen)),
        "unexpected": sorted(set(seen) - set(inventory["selected"])),
        "duplicate_case_ids": sorted(case for case, count in seen.items() if count > 1),
        "malformed_case_ids": sorted(malformed), "unvalidated_case_ids": sorted(unvalidated),
        "planned_shards": len(plan), "started_shards": len(shards),
        "completed_shards": sum(not shard["errors"] for shard in shards),
        "infrastructure_errors": infrastructure, "stopping_cause": stopping_cause,
        "shards": [{key: value for key, value in shard.items() if key != "report"} for shard in shards],
    })
    result["complete"] = (not any(result[key] for key in (
        "missing", "unexpected", "duplicate_case_ids", "malformed_case_ids",
        "unvalidated_case_ids", "infrastructure_errors"))
        and stopping_cause is None and result["completed_shards"] == len(plan))
    return result, {agent: valid}, links


def write_aggregate(output, inventory, plan, shards, agent, stopping_cause=None):
    result, report, links = reconcile_shards(inventory, plan, shards, agent, stopping_cause)
    reports = output / "reports"
    # The aggregate is an index of original raw reports, not a replacement for
    # them. Shard index/detail validation failures remain explicit in summary.json.
    linked = {agent: {case: dict(row, reportfile=links.get(case))
                      for case, row in report[agent].items()}}
    result["non_ok_cases"] = {case: linked[agent][case] for case in result["non_ok_cases"]}
    write_json(reports / "summary.json", result)
    write_json(reports / "index.json", linked)
    text = "\n".join([
        "# Autobahn baseline: " + agent, "",
        "Informational protocol baseline, not a certification or all-pass gate.", "",
        "- Available: {available}; selected: {selected}; reported: {reported}; excluded: {excluded}".format(**result),
        "- Complete infrastructure and coverage: " + str(result["complete"]),
        "- Shards: {completed_shards} complete / {started_shards} started / {planned_shards} planned".format(**result),
        "- Behavior: " + json.dumps(result["behavior"], sort_keys=True),
        "- Close behavior: " + json.dumps(result["behavior_close"], sort_keys=True),
        "- Missing: " + str(len(result["missing"])),
        "- Unexpected: " + str(len(result["unexpected"])),
        "- Duplicated: " + str(len(result["duplicate_case_ids"])),
        "- Unvalidated observed cases: " + str(len(result["unvalidated_case_ids"])),
        "- Stopping cause: " + (stopping_cause or "none"), "",
        "See summary.json for all outcomes and exclusions; index.html links original shard reports.", "",
    ])
    (reports / "summary.md").write_text(text)
    rows = []
    for case in inventory["selected"]:
        row = report[agent].get(case)
        if row and case in links:
            href = escape(links[case][:-5] + ".html", quote=True)
            rows.append('<li><a href="' + href + '">' + escape(case) + '</a>: '
                        + escape(row["behavior"]) + " / " + escape(row["behaviorClose"]) + '</li>')
        elif case in result["unvalidated_case_ids"]:
            rows.append("<li>" + escape(case) + ": unvalidated report</li>")
        else:
            rows.append("<li>" + escape(case) + ": missing report</li>")
    shard_links = []
    for number, shard in enumerate(shards, 1):
        if not safe_shard_directory(shard, number):
            shard_links.append("<li>Invalid shard metadata; no report link promoted</li>")
            continue
        shard_links.append('<li><a href="../' + shard["directory"]
                           + '/reports/index.html">Shard ' + str(number) + '</a>: '
                           + escape(", ".join(shard["cases"])) + "; "
                           + escape("; ".join(shard["errors"]) or "complete") + '</li>')
    (reports / "index.html").write_text(
        '<!doctype html><html lang="en"><meta charset="utf-8"><title>Autobahn baseline</title>'
        '<h1>' + escape(agent) + '</h1><pre>' + escape(text) + '</pre>'
        '<p><a href="summary.json">Full aggregate summary</a> | '
        '<a href="index.json">Aggregate JSON index</a> | '
        '<a href="../metadata.json">Run metadata and shard plan</a></p>'
        '<h2>Cases: behavior / close behavior</h2><ul>' + "".join(rows) + '</ul>'
        '<h2>Original shard reports</h2><ul>' + "".join(shard_links) + '</ul></html>')
    return result


def run_shards(output, metadata, size, deadline):
    """Inventory first, then fail-stop sequential fresh-container batches."""
    binary = output / "testee"
    inventory_output = output / "inventory"
    (inventory_output / "config").mkdir(parents=True)
    (inventory_output / "reports").mkdir()
    write_json(inventory_output / "config/spec.json", metadata["spec"])
    inventory_metadata = dict(metadata, invocation="inventory-only")
    errors = execute_container(inventory_output, binary, "websocket-autobahn-" + str(os.getpid()) + "-inventory",
                               inventory_metadata, deadline, inventory_only=True)
    try:
        if errors:
            raise ValueError("; ".join(errors))
        inventory = load_json(inventory_output / "reports/inventory.json")
        plan = partition_inventory(inventory, size)
        if len(inventory["selected"]) != COMPRESSION_CASE_COUNT:
            raise ValueError("pinned compression inventory must select exactly 216 cases")
    except (OSError, ValueError, TypeError, KeyError) as error:
        cause = "authoritative inventory failed: " + str(error)
        metadata.update(infrastructure_errors=[cause], stopping_cause=cause, complete=False)
        write_json(output / "metadata.json", metadata)
        write_json(output / "reports/summary.json", {
            "agent": metadata["agent"], "complete": False, "inventory_available": False,
            "expected_selected": COMPRESSION_CASE_COUNT, "reported": 0, "missing": None,
            "stopping_cause": cause})
        (output / "reports/summary.md").write_text("Incomplete Autobahn baseline\n\n" + cause + "\n")
        (output / "reports/index.html").write_text(
            '<!doctype html><html lang="en"><meta charset="utf-8"><title>Incomplete Autobahn baseline</title>'
            '<h1>Incomplete Autobahn baseline</h1><p>' + escape(cause) + '</p>'
            '<p>No case execution started. Selected case IDs are unavailable.</p>'
            '<a href="../inventory/container.log">Inventory container log</a></html>')
        raise RuntimeError(cause) from error
    write_json(output / "reports/inventory.json", inventory)
    metadata.update(shard_size=size, shard_plan=plan, expected_selected=len(inventory["selected"]))
    write_json(output / "metadata.json", metadata)
    shards = []
    stopping_cause = None
    for number, cases in enumerate(plan, 1):
        if time.monotonic() >= deadline:
            stopping_cause = "aggregate execution deadline exceeded before shard " + str(number)
            break
        directory = "shards/" + str(number).zfill(4)
        shard_output = output / directory
        (shard_output / "config").mkdir(parents=True)
        (shard_output / "reports").mkdir()
        spec = dict(metadata["spec"], cases=cases, **{"exclude-cases": []})
        write_json(shard_output / "config/spec.json", spec)
        shard_metadata = dict(metadata, spec=spec, shard_number=number, shard_cases=cases)
        # The complete plan lives once in root metadata; each shard names it.
        shard_metadata.pop("shard_plan")
        shard_metadata["aggregate_metadata"] = "../../metadata.json"
        errors = execute_container(shard_output, binary,
                                   "websocket-autobahn-" + str(os.getpid()) + "-" + str(number),
                                   shard_metadata, deadline)
        report, report_errors = inspect_shard(shard_output, inventory, cases, metadata["agent"])
        errors.extend(report_errors)
        shards.append({"number": number, "directory": directory, "cases": cases,
                       "report": report, "errors": errors,
                       "resource_diagnostics": shard_metadata.get("resource_diagnostics")})
        shard_metadata["infrastructure_errors"] = errors
        write_json(shard_output / "metadata.json", shard_metadata)
        if errors:
            stopping_cause = "shard " + str(number) + ": " + "; ".join(errors)
        elif time.monotonic() >= deadline:
            stopping_cause = "aggregate execution deadline exceeded after shard " + str(number)
        if stopping_cause:
            break
    result = write_aggregate(output, inventory, plan, shards, metadata["agent"], stopping_cause)
    metadata.update(complete=result["complete"], stopping_cause=stopping_cause,
                    started_shards=len(shards), completed_shards=result["completed_shards"])
    write_json(output / "metadata.json", metadata)
    print((output / "reports/summary.md").read_text())
    if not result["complete"]:
        raise RuntimeError(stopping_cause or "aggregate shard reconciliation failed")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--role", choices=["server", "client"], required=True)
    parser.add_argument("--profile", choices=PROFILES, required=True)
    parser.add_argument("--case", default="", help="one exact case ID for an explicitly partial diagnostic")
    parser.add_argument("--shard-size", type=int, default=0,
                        help="sequential server-compression batch size; 0 disables sharding")
    parser.add_argument("--output", type=Path, required=True,
                        help="new/empty directory; prior reports are never reused")
    args = parser.parse_args()
    if args.shard_size < 0 or (args.shard_size and (args.role, args.profile) != ("server", "compression")):
        parser.error("sharding is supported only for server compression, with a positive size")
    if args.shard_size and args.case:
        parser.error("--case cannot be combined with sharding")
    try:
        selected = selection(args.profile, args.case)
    except ValueError as error:
        parser.error(str(error))
    if os.getuid() == 0:
        parser.error("run as a non-root user; the container uses the invoking UID")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        parser.error("output directory must be empty")
    config = output / "config"
    reports = output / "reports"
    config.mkdir()
    reports.mkdir()
    agent = "picatz-websocket-" + args.role + "-" + args.profile
    if args.case:
        agent += "-case-" + args.case
    compression = args.profile == "compression"
    case_timeout = 60 if args.profile == "core" else 600
    spec = dict(selected, **{"exclude-agent-cases": {}, "outdir": "/reports"})
    if args.role == "server":
        spec["servers"] = [{"agent": agent, "url": "ws://127.0.0.1:9001"}]
    else:
        spec["url"] = "ws://127.0.0.1:9001"
    (config / "spec.json").write_text(json.dumps(spec, indent=2) + "\n")
    env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="amd64")
    run("go", "build", "-trimpath", "-o", str(output / "testee"),
        "./conformance/autobahn/cmd/testee", cwd=ROOT, env=env)
    run("docker", "pull", "--platform", "linux/amd64", IMAGE)
    inspection = run("docker", "image", "inspect", IMAGE, capture_output=True).stdout
    (output / "image-inspect.json").write_text(inspection)
    image = json.loads(inspection)[0]
    labels = image.get("Config", {}).get("Labels", {}) or {}
    revision = labels.get("org.label-schema.vcs-ref", "")
    if labels.get("org.label-schema.version") != "25.10.1" or revision not in {SOURCE, SOURCE[:7]}:
        raise RuntimeError("official image version/revision labels differ from the verified pin")
    metadata = {
        "testee_commit": run("git", "rev-parse", "HEAD", cwd=ROOT, capture_output=True).stdout.strip(),
        "working_tree_dirty": bool(run("git", "status", "--porcelain", cwd=ROOT, capture_output=True).stdout),
        "go_version": run("go", "version", capture_output=True).stdout.strip(),
        "image": IMAGE, "suite_source": SOURCE, "role": args.role,
        "profile": args.profile, "agent": agent, "compression_enabled": compression,
        "diagnostic_case": args.case or None, "profile_selection": PROFILES[args.profile],
        "max_message_bytes": 64 << 20, "case_timeout_seconds": case_timeout,
        "run_timeout_seconds": RUN_TIMEOUT, "shard_size": args.shard_size, "network": "none (container loopback only)",
        "suite_environment": {"PYPY_GC_MAX": PYPY_GC_MAX,
                              "PYTHONDONTWRITEBYTECODE": "1", "PYTHONUNBUFFERED": "1"},
        "spec": spec,
    }
    (output / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    deadline = time.monotonic() + RUN_TIMEOUT
    if args.shard_size:
        run_shards(output, metadata, args.shard_size, deadline)
        return
    errors = execute_container(output, output / "testee", "websocket-autobahn-" + str(os.getpid()),
                               metadata, deadline)
    if errors:
        raise SystemExit("; ".join(errors))
    # Protocol failures are evidence, not an all-pass gate during baseline work.
    run(sys.executable, str(HERE / "summarize.py"), str(reports), "--agent", agent)


if __name__ == "__main__":
    main()
