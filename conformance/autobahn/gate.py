#!/usr/bin/env python3
"""Fail unless every selected case in a complete Autobahn summary is accepted.

The run and summary steps keep every outcome as evidence. This gate is the
verdict: a case is accepted when both its behavior and its close behavior are
OK or INFORMATIONAL. INFORMATIONAL cases have no verdict in the suite. Every
other outcome, including NON-STRICT, WRONG CODE, FAILED BY CLIENT and
UNIMPLEMENTED, rejects the run, so a new warning is reviewed rather than
absorbed into a green check.
"""
import argparse
import json
from pathlib import Path

ACCEPTED = {"OK", "INFORMATIONAL"}


def rejected(summary):
    """Return "case: behavior / close" for each case the gate rejects.

    An incomplete summary is rejected as a whole, because missing or
    unvalidated cases have no outcome to judge.
    """
    if summary.get("complete") is not True:
        raise ValueError("summary is incomplete; no verdict is possible")
    if summary.get("reported_unique", summary.get("reported")) != summary.get("selected"):
        raise ValueError("summary does not report every selected case")
    return [case + ": " + row["behavior"] + " / " + row["behaviorClose"]
            for case, row in sorted(summary["non_ok_cases"].items())
            if row["behavior"] not in ACCEPTED or row["behaviorClose"] not in ACCEPTED]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("summary", type=Path, help="reports/summary.json from run.py")
    args = parser.parse_args()
    cases = rejected(json.loads(args.summary.read_text()))
    if cases:
        raise SystemExit("rejected Autobahn outcomes:\n" + "\n".join(cases))
    print("every selected Autobahn case is OK or INFORMATIONAL")


if __name__ == "__main__":
    main()
