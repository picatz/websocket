#!/usr/bin/env python3
"""Build and run one isolated Autobahn role/profile on a Docker-capable host."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys

IMAGE = "crossbario/autobahn-testsuite:25.10.1@sha256:519915fb568b04c9383f70a1c405ae3ff44ab9e35835b085239c258b6fac3074"
SOURCE = "6ed6f439dc7ed0d7432fe2cf7481b110905ecc5c"
HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
PROFILES = {
    "core": {"cases": ["*"], "exclude-cases": ["9.*", "12.*", "13.*"]},
    "compression": {"cases": ["12.*", "13.*"], "exclude-cases": []},
    "limits": {"cases": ["9.*"], "exclude-cases": []},
}


def run(*args, **kwargs):
    return subprocess.run(args, check=True, text=True, **kwargs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--role", choices=["server", "client"], required=True)
    parser.add_argument("--profile", choices=PROFILES, required=True)
    parser.add_argument("--output", type=Path, required=True,
                        help="new/empty directory; prior reports are never reused")
    args = parser.parse_args()
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
    compression = args.profile == "compression"
    case_timeout = 60 if args.profile == "core" else 600
    spec = dict(PROFILES[args.profile], **{"exclude-agent-cases": {}, "outdir": "/reports"})
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
        "max_message_bytes": 64 << 20, "case_timeout_seconds": case_timeout,
        "run_timeout_seconds": 1200, "network": "none (container loopback only)",
        "spec": spec,
    }
    (output / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    name = "websocket-autobahn-" + str(os.getpid())
    command = [
        "docker", "run", "--name", name, "--platform", "linux/amd64",
        "--network", "none", "--read-only", "--cap-drop", "ALL",
        "--security-opt", "no-new-privileges", "--pids-limit", "128",
        "--memory", "1g", "--memory-swap", "1g", "--cpus", "2",
        "--user", str(os.getuid()) + ":" + str(os.getgid()),
        "--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
        "--env", "PYTHONDONTWRITEBYTECODE=1", "--env", "PYTHONUNBUFFERED=1", "--env", "HOME=/tmp",
        "--mount", "type=bind,src=" + str(output / "testee") + ",dst=/testee,readonly",
        "--mount", "type=bind,src=" + str(HERE) + ",dst=/harness,readonly",
        "--mount", "type=bind,src=" + str(config) + ",dst=/config,readonly",
        "--mount", "type=bind,src=" + str(reports) + ",dst=/reports",
        "--entrypoint", "/bin/sh", IMAGE, "/harness/inside.sh",
        args.role, str(compression).lower(), agent, str(case_timeout) + "s",
    ]
    (output / "command.json").write_text(json.dumps(command, indent=2) + "\n")
    try:
        with (output / "container.log").open("w") as log:
            completed = subprocess.run(command, text=True, stdout=log, stderr=subprocess.STDOUT, timeout=1200)
        metadata["container_exit_code"] = completed.returncode
    except subprocess.TimeoutExpired:
        metadata["container_timed_out"] = True
        raise
    finally:
        # Stop this invocation's container, preserve exit/OOM diagnostics, then
        # remove it. --rm would discard the evidence needed to diagnose a kill.
        subprocess.run(["docker", "kill", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        state = subprocess.run(["docker", "inspect", name], text=True, capture_output=True)
        (output / "container-inspect.json").write_text(state.stdout or state.stderr)
        subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        (output / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    # Protocol failures are evidence, not an all-pass gate during baseline work.
    # Missing/malformed/unknown reports and container failures still fail the job.
    run(sys.executable, str(HERE / "summarize.py"), str(reports), "--agent", agent)
    if completed.returncode:
        raise SystemExit(completed.returncode)


if __name__ == "__main__":
    main()
