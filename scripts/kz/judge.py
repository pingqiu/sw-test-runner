#!/usr/bin/env python3
"""Judge the product test's complete two-role acceptance reports, never log substrings."""
import os
import tarfile
import json
import re

TARGET_VOLUMES = ("kzext4r", "kzxfsr")

def judge(report, role, run, rc):
    if role not in ("source", "target") or type(rc) is not int or rc != 0 or not isinstance(report, dict):
        return False
    if (report.get("run") != run or report.get("role") != role
            or type(report.get("bytes")) is not int or report.get("bytes") != 1 << 40 or report.get("device") is not True):
        return False
    expected = ({(n, v) for v in ("kzext4", "kzxfs") for n in (1, 2)} | {(5, "kzc1")}
                if role == "source" else {(n, v) for v in TARGET_VOLUMES for n in (3, 4)})
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


def judge_archive(path, role, run):
    try:
        with tarfile.open(path, "r:gz") as archive:
            data = {}
            for name, maximum in (("out/report.json", 16 << 20), ("out/exit.json", 4096)):
                members = [m for m in archive.getmembers() if m.name == name]
                if len(members) != 1 or not members[0].isfile() or members[0].size > maximum:
                    return False
                with archive.extractfile(members[0]) as f:
                    data[name] = json.load(f)
        e = data["out/exit.json"]
        return (e.get("run") == run and e.get("role") == role and type(e.get("rc")) is int
                and judge(data["out/report.json"], role, run, e["rc"]))
    except (OSError, ValueError, TypeError, tarfile.TarError, KeyError, AttributeError):
        return False


def main():
    # Environment values are shell-quoted by the runner's exec action. Archives
    # are read in place on the controller; no member is extracted to disk.
    role, run = os.environ["KZ_ROLE"], os.environ["KZ_ACCEPTANCE_ID"]
    good = judge_archive(os.environ["KZ_ARCHIVE"], role, run)
    print(json.dumps(dict(role=role, run=run, verdict="PASS" if good else "FAIL")))
    return 0 if good else 1


if __name__ == "__main__": raise SystemExit(main())
