#!/usr/bin/env python3
"""Keep baseline failures visible; incomplete or unrecognized reports are errors."""
import argparse
from collections import Counter
import json
from pathlib import Path

BEHAVIOR = {"OK", "NON-STRICT", "INFORMATIONAL", "UNIMPLEMENTED", "FAILED"}
CLOSE = {"OK", "INFORMATIONAL", "FAILED BY CLIENT", "WRONG CODE", "UNCLEAN", "FAILED"}


def summarize(inventory, report, agent):
    selected = inventory["selected"]
    if not selected or len(selected) != len(set(selected)):
        raise ValueError("selected case inventory is empty or duplicated")
    all_ids, excluded = inventory["all"], inventory["excluded"]
    if (len(all_ids) != len(set(all_ids)) or len(excluded) != len(set(excluded))
            or set(selected) & set(excluded) or set(all_ids) != set(selected) | set(excluded)):
        raise ValueError("inventory does not partition available cases exactly")
    if set(report) != {agent}:
        raise ValueError("report agents do not match the requested single agent")
    results = report[agent]
    missing = sorted(set(selected) - set(results))
    unexpected = sorted(set(results) - set(selected))
    malformed = [case for case, result in results.items()
                 if not isinstance(result, dict)
                 or result.get("behavior") not in BEHAVIOR
                 or result.get("behaviorClose") not in CLOSE]
    if malformed:
        raise ValueError("unrecognized case outcomes: " + ", ".join(malformed))
    behavior = Counter(r["behavior"] for r in results.values())
    close = Counter(r["behaviorClose"] for r in results.values())
    failed = sorted(case for case, r in results.items()
                    if r["behavior"] == "FAILED" or r["behaviorClose"] in {"FAILED", "UNCLEAN"})
    skipped = sorted(case for case, r in results.items() if r["behavior"] == "UNIMPLEMENTED")
    non_ok = {case: result for case, result in results.items()
              if result["behavior"] != "OK" or result["behaviorClose"] != "OK"}
    return {"agent": agent, "available": len(inventory["all"]),
            "selected": len(selected), "reported": len(results),
            "excluded": len(inventory["excluded"]), "excluded_case_ids": inventory["excluded"],
            "missing": missing, "unexpected": unexpected,
            "behavior": dict(sorted(behavior.items())),
            "behavior_close": dict(sorted(close.items())),
            "failed_case_ids": failed, "skipped_case_ids": skipped,
            "non_ok_cases": non_ok,
            "complete": not missing and not unexpected}


def validate_details(root, report, agent):
    if not (root / "index.html").is_file():
        raise ValueError("missing HTML index")
    for case, result in report[agent].items():
        filename = result.get("reportfile")
        if not isinstance(filename, str) or Path(filename).name != filename or not filename.endswith(".json"):
            raise ValueError("invalid case report filename: " + case)
        detail = json.loads((root / filename).read_text())
        if (detail.get("id"), detail.get("agent"), detail.get("behavior"), detail.get("behaviorClose")) != (
                case, agent, result["behavior"], result["behaviorClose"]):
            raise ValueError("case detail disagrees with index: " + case)
        if not (root / filename).with_suffix(".html").is_file():
            raise ValueError("missing case HTML report: " + case)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--agent", required=True)
    args = parser.parse_args()
    root = args.directory
    report = json.loads((root / "index.json").read_text())
    result = summarize(json.loads((root / "inventory.json").read_text()), report, args.agent)
    validate_details(root, report, args.agent)
    (root / "summary.json").write_text(json.dumps(result, indent=2) + "\n")
    text = "\n".join([
        "# Autobahn baseline: " + args.agent,
        "",
        "Informational protocol baseline, not a certification or all-pass gate.",
        "",
        "- Available: {available}; selected: {selected}; reported: {reported}; excluded: {excluded}".format(**result),
        "- Behavior: " + json.dumps(result["behavior"], sort_keys=True),
        "- Close behavior: " + json.dumps(result["behavior_close"], sort_keys=True),
        "- Failed cases (either behavior dimension): " + str(len(result["failed_case_ids"])),
        "- Skipped/unimplemented: " + str(len(result["skipped_case_ids"])),
        "- Missing: " + str(len(result["missing"])),
        "- Unexpected: " + str(len(result["unexpected"])),
        "",
        "See summary.json for every non-OK outcome and exclusion; index.html/JSON for raw reports.",
        "NON-STRICT, INFORMATIONAL, WRONG CODE, and FAILED BY CLIENT remain separate from failures.",
        "",
    ])
    (root / "summary.md").write_text(text)
    print(text)
    if not result["complete"]:
        raise SystemExit("incomplete or unexpected case coverage")


if __name__ == "__main__":
    main()
