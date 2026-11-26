import copy
import unittest
from judge import judge

RUN = "kz-20261126t050000z"
def report(role):
    names = ([(n, v) for v in ("kzext4", "kzxfs") for n in (1, 2)] + [(5, "kzc1")]
             if role == "source" else [(n, v) for v in ("kzext4r", "kzext4f", "kzxfsr", "kzxfsf") for n in (3, 4)])
    return dict(run=RUN, role=role, bytes=1 << 40, device=True,
                lines=[f"KZ STEP {n} PASS {v} measured evidence" for n,v in names])

class JudgeTest(unittest.TestCase):
    def test_complete(self):
        for role in ("source", "target"):
            self.assertTrue(judge(report(role), role, RUN, 0))
    def test_negative_controls(self):
        for role in ("source", "target"):
            good = report(role)
            mutants = []
            for key, value in (("run", "old"), ("role", "other"), ("bytes", 1 << 36), ("device", False)):
                m = copy.deepcopy(good); m[key] = value; mutants.append((key, m, 0))
            m = copy.deepcopy(good); m["lines"].pop(); mutants.append(("missing", m, 0))
            m = copy.deepcopy(good); m["lines"] += [m["lines"][0]]; mutants.append(("duplicate", m, 0))
            m = copy.deepcopy(good); m["lines"][0] = m["lines"][0].replace("PASS", "FAIL"); mutants.append(("failed", m, 0))
            mutants.append(("exit", good, 1))
            for name, value, rc in mutants:
                with self.subTest(role=role, mutant=name):
                    self.assertFalse(judge(value, role, RUN, rc))

if __name__ == "__main__": unittest.main()
