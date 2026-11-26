import copy
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from judge import judge, judge_archive

RUN = "kz-20261126t050000z"
def report(role):
    names = ([(n, v) for v in ("kzext4", "kzxfs") for n in (1, 2)] + [(5, "kzc1")]
             if role == "source" else [(n, v) for v in ("kzext4r", "kzxfsr") for n in (3, 4)])
    return dict(run=RUN, role=role, bytes=1 << 40, device=True,
                lines=[f"KZ STEP {n} PASS {v} measured evidence" for n,v in names])

class JudgeTest(unittest.TestCase):
    def test_four_restore_rows_only(self):
        good = report("target")
        self.assertEqual(len(good["lines"]), 4)
        self.assertEqual(len(report("source")["lines"]) + len(good["lines"]), 9)
        for volume in ("kzext4f", "kzxfsf"):
            extra = copy.deepcopy(good)
            extra["lines"] += [f"KZ STEP {n} PASS {volume} legacy fork" for n in (3, 4)]
            self.assertFalse(judge(extra, "target", RUN, 0))
    def test_complete(self):
        for role in ("source", "target"):
            self.assertTrue(judge(report(role), role, RUN, 0))
    def test_collected_archive_is_judged_on_the_controller(self):
        for mutant in ("", "missing", "duplicate", "symlink", "foreign-exit", "nonzero-exit"):
            with self.subTest(mutant=mutant), tempfile.TemporaryDirectory() as base:
                archive=Path(base)/"collected.tgz"
                with tarfile.open(archive,"w:gz") as t:
                    values={"out/report.json":report("source"),"out/exit.json":dict(run=RUN,role="source",rc=0)}
                    if mutant=="foreign-exit":values["out/exit.json"]["run"]="old"
                    if mutant=="nonzero-exit":values["out/exit.json"]["rc"]=1
                    for name,value in values.items():
                        if mutant=="missing" and name=="out/exit.json":continue
                        raw=json.dumps(value).encode();m=tarfile.TarInfo(name);m.size=len(raw)
                        if mutant=="symlink":m.type=tarfile.SYMTYPE;m.linkname="/foreign"
                        t.addfile(m,io.BytesIO(raw))
                        if mutant=="duplicate":t.addfile(m,io.BytesIO(raw))
                self.assertEqual(judge_archive(archive,"source",RUN),mutant=="")
                self.assertEqual(list(Path(base).iterdir()),[archive]) # no tar extraction

    def test_negative_controls(self):
        for role in ("source", "target"):
            good = report(role)
            mutants = []
            for key, value in (("run", "old"), ("role", "other"), ("bytes", 1 << 36), ("bytes", float(1 << 40)), ("device", False)):
                m = copy.deepcopy(good); m[key] = value; mutants.append((key, m, 0))
            m = copy.deepcopy(good); m["lines"].pop(); mutants.append(("missing", m, 0))
            m = copy.deepcopy(good); m["lines"] += [m["lines"][0]]; mutants.append(("duplicate", m, 0))
            m = copy.deepcopy(good); m["lines"][0] = m["lines"][0].replace("PASS", "FAIL"); mutants.append(("failed", m, 0))
            mutants.append(("exit", good, 1))
            mutants.append(("bool-exit", good, False))
            for name, value, rc in mutants:
                with self.subTest(role=role, mutant=name):
                    self.assertFalse(judge(value, role, RUN, rc))

if __name__ == "__main__": unittest.main()
