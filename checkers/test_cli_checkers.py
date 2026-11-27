"""--checkers: which checkers decide the exit code. Every checker still runs and
is written to --json; a scenario names the ones its run exercises."""
import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path

import history_check as hc

FIXTURE = Path(__file__).resolve().parent / 'testdata' / 'm0-history.jsonl'


def run(*args):
    out = io.StringIO()
    with contextlib.redirect_stdout(out):
        code = hc.main([str(FIXTURE), *args])
    return code, out.getvalue()


class Checkers(unittest.TestCase):
    def test_the_m0_set_passes_on_the_m0_run(self):
        code, out = run('--checkers', 'C0,C1,C2,C3,C7')
        self.assertEqual(code, 0, out)
        self.assertIn('C6 FAIL (not required)', out)

    def test_all_by_default(self):
        # The M0 run exercises no fault: C6, DUR and MO answer FAIL (not exercised).
        code, out = run()
        self.assertEqual(code, 1, out)
        self.assertNotIn('(not required)', out)

    def test_red_a_required_checker_that_fails_fails_the_run(self):
        code, out = run('--checkers', 'C0,C6')
        self.assertEqual(code, 1, out)

    def test_every_checker_is_still_written(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / 'checks.json'
            run('--checkers', 'C0', '--json', str(p))
            self.assertEqual(sorted(json.loads(p.read_text(encoding='utf-8'))), sorted(hc.CHECKERS))

    def test_an_unknown_checker_is_an_error(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as e:
            run('--checkers', 'C0,C9')
        self.assertEqual(e.exception.code, 2)
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            run('--checkers', ',')

    def test_the_scenarios_name_known_checkers(self):
        root = Path(__file__).resolve().parent.parent / 'scenarios' / 'v3block'
        want = {'m0-sequence': 'C0,C1,C2,C3,C7', 'm1-random': 'C0,C1,C2,C3,C5,C7',
                'm2-faults': 'C0,C1,C2,C3,C5,C6,C7,DUR,MO'}
        for name, cs in want.items():
            text = (root / (name + '.yaml')).read_text(encoding='utf-8')
            self.assertIn('--checkers ' + cs, text, name)
            self.assertTrue(set(cs.split(',')) <= set(hc.CHECKERS), name)


if __name__ == '__main__':
    unittest.main()
