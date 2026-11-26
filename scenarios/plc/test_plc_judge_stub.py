"""plc_judge.py controls with a stub packet (packet-free, so they run in this repository's CI): whatever the
packet's judge_run does other than return PASS or FAIL, the wrapper reports UNKNOWN and exits 2; it never exits 0
for anything but PASS; and it writes nothing into the packet."""
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent


class Stub(unittest.TestCase):
    def judge(self, body):
        with tempfile.TemporaryDirectory() as t:
            t = Path(t)
            pk = t/'packet'
            (pk/'drill').mkdir(parents=True)
            (pk/'drill'/'powerloss-fs.json').write_text('{}')
            (pk/'helper.py').write_text('X = 1\n')
            (pk/'powerloss.py').write_text('import sys\nsys.path.insert(0, %r)\nimport helper\n%s\n' % (str(pk), body))
            out = t/'judge.json'
            p = subprocess.run([sys.executable, '-I', str(HERE/'plc_judge.py'), str(pk), str(t/'run'), 'R1', str(out)],
                               capture_output=True, text=True, timeout=60)
            left = sorted(x.name for x in pk.rglob('*'))
            return json.loads(out.read_text())['verdict'], p.returncode, left

    def test_pass_and_fail_pass_through(self):
        self.assertEqual(self.judge('def judge_run(*a): return {"verdict": "PASS"}')[:2], ('PASS', 0))
        self.assertEqual(self.judge('def judge_run(*a): return {"verdict": "FAIL"}')[:2], ('FAIL', 1))

    def test_unknown_raise_and_malformed_are_unknown_exit_2(self):
        for name, body in (('unknown', 'def judge_run(*a): return {"verdict": "UNKNOWN"}'),
                           ('raises', 'def judge_run(*a): raise RuntimeError("boom")'),
                           ('non-dict', 'def judge_run(*a): return "PASS"'),
                           ('odd verdict', 'def judge_run(*a): return {"verdict": "pass"}'),
                           ('no judge_run', 'judge = None')):
            with self.subTest(case=name):
                self.assertEqual(self.judge(body)[:2], ('UNKNOWN', 2), name)

    def test_judging_writes_nothing_into_the_packet(self):
        _, _, left = self.judge('def judge_run(*a): return {"verdict": "PASS"}')
        self.assertEqual(left, ['drill', 'helper.py', 'powerloss-fs.json', 'powerloss.py'])


if __name__ == '__main__':
    unittest.main()
