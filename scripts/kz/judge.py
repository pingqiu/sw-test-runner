#!/usr/bin/env python3
"""Judge the product test's complete two-role acceptance reports, never log substrings."""
import argparse
import json
from pathlib import Path
import re


def judge(report, role, run, rc):
    if role not in ("source", "target") or rc != 0 or not isinstance(report, dict):
        return False
    if (report.get("run") != run or report.get("role") != role
            or report.get("bytes") != 1 << 40 or report.get("device") is not True):
        return False
    expected = ({(n, v) for v in ("kzext4", "kzxfs") for n in (1, 2)} | {(5, "kzc1")}
                if role == "source" else {(n, v) for v in ("kzext4r", "kzext4f", "kzxfsr", "kzxfsf") for n in (3, 4)})
    lines = report.get("lines")
    if not isinstance(lines, list) or len(lines) != len(expected):
        return False
    observed = set()
    for line in lines:
        m = re.fullmatch(r"KZ STEP ([1-5]) PASS ([a-z0-9]+) (.+)", line) if isinstance(line, str) else None
        if not m or (int(m[1]), m[2]) in observed:
            return False
        observed.add((int(m[1]), m[2]))
    return observed == expected


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("report"); p.add_argument("role", choices=("source", "target"))
    p.add_argument("run"); p.add_argument("rc_file")
    a = p.parse_args()
    try:
        e = json.loads(Path(a.rc_file).read_text())
        good = (e.get("run") == a.run and e.get("role") == a.role and type(e.get("rc")) is int
                and judge(json.loads(Path(a.report).read_text()), a.role, a.run, e["rc"]))
    except (OSError, ValueError, TypeError):
        good = False
    print(json.dumps(dict(role=a.role, run=a.run, verdict="PASS" if good else "FAIL")))
    return 0 if good else 1


if __name__ == "__main__": raise SystemExit(main())
