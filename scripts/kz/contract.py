#!/usr/bin/env python3
"""Fail closed before host work if the pinned product's target recipe drifts.

This enumerates the current Go test's literal filesystem/kind lists and the
target-name expression, rather than executing the device acceptance test.
Changing that source shape requires an explicit review of this contract.
"""
import ast
import json
import os
from pathlib import Path
import re
import subprocess

PRODUCT_TEST = "cmd/blockvolume/known_zero_acceptance_integration_test.go"


def literal(source, name):
    values = [ast.literal_eval(n.value) for n in ast.parse(source).body
              if isinstance(n, ast.Assign) and any(isinstance(t, ast.Name) and t.id == name for t in n.targets)]
    if len(values) != 1:
        raise ValueError("missing or duplicate runner contract: " + name)
    return values[0]


def check(product, host, judge):
    # The counted invocation fixes KZ_FS=ext4,xfs; pin the test's default too.
    def strings(pattern):
        matches = re.findall(pattern, product)
        if len(matches) != 1:
            raise ValueError("product target recipe changed")
        return re.findall(r'"([a-z0-9]+)"', matches[0])
    fss = strings(r'fss\s*=\s*\[\]string\{([^}]+)\}')
    kinds = strings(r'for\s+_,\s*kind\s*:=\s*range\s*\[\]string\{([^}]+)\}')
    if not re.search(r'tb\s*:=\s*env\.hosts\["kz"\+fs\+kind\[:1\]\]', product):
        raise ValueError("product target name expression changed")
    if len(re.findall(r'ho\.Targets\s*=\s*append\(ho\.Targets,\s*kzHandoffTarget\{Kind:\s*kind,\s*Volume:\s*tb\.volume', product)) != 1:
        raise ValueError("product handoff writer changed")
    targets = tuple("kz" + fs + kind[0] for fs in fss for kind in kinds)
    if (not targets or len(set(targets)) != len(targets)
            or len(targets) != literal(host, "TARGET_COUNT")
            or targets != literal(judge, "TARGET_VOLUMES")):
        raise ValueError("product/runner target list mismatch: " + repr(targets))
    return targets


def main():
    sha = os.environ["KZ_RC"]
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("full product source pin required")
    source = subprocess.run(["git", "-C", os.environ["KZ_PRODUCT_SRC"], "show", sha+":"+PRODUCT_TEST],
                            check=True, capture_output=True, text=True, timeout=30).stdout
    here = Path(__file__).parent
    targets = check(source, (here/"host.py").read_text(), (here/"judge.py").read_text())
    print(json.dumps(dict(product_sha=sha, targets=targets, target_rows=len(targets)*2, total_rows=5+len(targets)*2)))


if __name__ == "__main__":
    main()
