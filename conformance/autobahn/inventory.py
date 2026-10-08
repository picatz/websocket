#!/usr/bin/env python
"""Run with the pinned suite's Python 2, using its own selection semantics."""
import json
import sys

from autobahntestsuite.case import (Cases, CaseCategories, CaseSubCategories,
                                  CaseSetname, CaseBasename)
from autobahntestsuite.caseset import CaseSet

spec = json.load(open(sys.argv[1]))
cases = CaseSet(CaseSetname, CaseBasename, Cases, CaseCategories, CaseSubCategories)
all_ids = cases.parseSpecCases({"cases": ["*"], "exclude-cases": []})
selected = cases.parseSpecCases(spec)
assert not spec.get("exclude-agent-cases"), "agent-specific exclusions are forbidden"
json.dump({"all": all_ids, "selected": selected,
           "excluded": [case for case in all_ids if case not in selected]},
          open(sys.argv[2], "w"), indent=2)
