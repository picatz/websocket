# Autobahn conformance evidence

This is a repeatable **informational baseline**, not a claim of complete RFC
6455 compliance. The driver exercises this module's public `Dial`, `Upgrade`,
`ReadMessage`, `WriteMessage`, and `Close` entry points. It does not repair
frames, supply protocol-error close codes, or bypass production validation.
An application closes after a read/write error, as currently documented by the
library. Any remaining protocol failures belong in separate, focused fixes.

## Reproduce

Requirements: Go matching `go.mod`, Python 3 (standard library only), and Docker
on a Linux-capable host. The image is pinned for linux/amd64. Docker Desktop can
use its Linux VM; other architectures require working amd64 emulation. No
Docker daemon is installed or configured by these scripts.

From the repository root, use a **new, empty** output directory each time:

```sh
python3 -m unittest discover -s conformance/autobahn -p 'test_*.py'
go test -race ./conformance/autobahn/cmd/...
python3 conformance/autobahn/run.py --role server --profile core --output /tmp/ws-server-core
python3 conformance/autobahn/run.py --role client --profile core --output /tmp/ws-client-core
```

Repeat both roles with `--profile limits` and `--profile compression`, each
with its own output directory, for all three disjoint selections. A role names
the Go implementation being tested: `server` runs Autobahn's `fuzzingclient`,
and `client` runs its `fuzzingserver`.

The `Autobahn baseline` workflow runs all six combinations for changes to this
harness and through **Actions → Autobahn baseline → Run workflow** after the
workflow is on the default branch. PR jobs check out the exact PR head commit.
Other library changes do not automatically run this expensive baseline yet.
The existing test workflow remains the ordinary correctness gate.

## Selections and limits

| Profile | Selected | Explicit exclusions | Compression |
| --- | --- | --- | --- |
| core | `*` | `9.*`, `12.*`, `13.*` | Disabled, the library default |
| limits | `9.*` | None within that selection | Disabled |
| compression | `12.*`, `13.*` | None within that selection | Explicitly enabled, experimental |

Each artifact contains the exact spec and the authoritative runtime inventory
from the pinned suite's `CaseSet.parseSpecCases`. The excluded inventory lists
all cases outside that run's selection. No agent-specific exclusions or
known-failure exclusions are allowed. Compression being `UNIMPLEMENTED` is a
skip, not a passing test. Compression results cannot establish default-mode
conformance, and default-mode results cannot establish RFC 7692 support.

All profiles deliberately set a **test-only 64 MiB message cap** and a 20-minute
whole-run deadline. The per-connection watchdog is 60 seconds for core and 600
seconds for limits/compression, which contain upstream 480-second cases. These values allow
the suite's large messages while bounding this disposable test infrastructure.
They do not test the library's unlimited default or prove resource-policy
defaults safe. Watchdog expirations are logged and can influence close results;
inspect them before attributing failures to protocol handling. Limits timings
are diagnostics, not controlled performance benchmarks.

## Isolation and provenance

Both peers run inside the same container with Docker `--network none`. Only its
loopback interface is used; no host ports are published. The Go driver rejects
DNS names and non-loopback addresses. The container has a read-only root,
dropped capabilities, no-new-privileges, a non-root host UID, a 128-process cap,
two CPUs, 1 GiB memory with no additional swap, and a bounded temporary mount.
Only the test binary, this harness, and generated config are mounted read-only;
only the report directory is writable. No Docker socket, credentials, checkout,
or external targets are passed into the container. Pulling the official image
occurs on the host before this network-disabled run.

Pinned suite:

- Image: `crossbario/autobahn-testsuite:25.10.1@sha256:519915fb568b04c9383f70a1c405ae3ff44ab9e35835b085239c258b6fac3074`
- [Official Docker Hub manifest and build metadata](https://hub.docker.com/layers/crossbario/autobahn-testsuite/25.10.1/images/sha256-519915fb568b04c9383f70a1c405ae3ff44ab9e35835b085239c258b6fac3074)
- [Upstream release revision](https://github.com/crossbario/autobahn-testsuite/tree/6ed6f439dc7ed0d7432fe2cf7481b110905ecc5c), tag `v25.10.1`
- [Official usage and legacy-runtime explanation](https://github.com/crossbario/autobahn-testsuite/blob/v25.10.1/README.md)
- [Result/control protocol source](https://github.com/crossbario/autobahn-testsuite/blob/v25.10.1/autobahntestsuite/autobahntestsuite/fuzzing.py)
- [Case selection source](https://github.com/crossbario/autobahn-testsuite/blob/v25.10.1/autobahntestsuite/autobahntestsuite/caseset.py)

Upstream intentionally preserves a Python 2-era reference environment. The
runner verifies version/revision image labels, saves Docker inspection data,
and records actual suite/Python versions rather than trusting runtime versions
mentioned in prose. Update the digest and revision together only after checking
the official image and reviewing any changes to cases/result semantics.

## Reading a baseline

Artifacts include `metadata.json` (testee commit, dirty-tree status, Go version,
image/revision, options, exit status), `command.json`, image/container inspection,
available cgroup memory/OOM counters, unbuffered suite output, config,
selected/excluded case IDs, raw HTML/JSON reports, suite/testee logs, and
`reports/summary.json` plus a short Markdown summary. GitHub retains them for
30 days. Download and preserve an artifact if it must remain release evidence.

The summary records `behavior` and `behaviorClose` **independently**, including
every non-OK row. `FAILED` and `UNCLEAN` are failures; `UNIMPLEMENTED` is skipped.
`NON-STRICT`, `WRONG CODE`, `FAILED BY CLIENT`, and `INFORMATIONAL` stay visible
as their own categories. Treating every non-OK value as a protocol failure would
misrepresent upstream's classifications. A green informational job means the
run/report was complete; it does **not** mean every selected case passed.

Missing, unexpected, empty, malformed, or unknown-status reports fail the job,
as do container failures and timeouts. Known baseline protocol failures are
reported without failing all of main. There is no badge or allowed-failure
list. Establish a reviewed baseline before proposing a stricter regression
gate. The summarizer has independent synthetic tests for both result dimensions,
missing/unexpected cases, warnings, skips, and unknown outcomes.

Autobahn is one source of evidence. Its opening-handshake coverage is incomplete;
keep the repository's targeted handshake/security tests. This run is not a real
Chrome/Firefox/WebKit interoperability test, an RFC certification, a security
audit, or a statement about licensing/provenance. Known upstream limitations
must remain visible in interpretation, including [case 7.7.8's server close-code
1010 question](https://github.com/crossbario/autobahn-testsuite/issues/123).
