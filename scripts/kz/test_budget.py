"""Cross-check the two-role runner budget against the post-target scan."""
import re
from pathlib import Path
import unittest

class BudgetTest(unittest.TestCase):
    def test_post_target_scan_fits_source_and_cleanup_deadlines(self):
        root = Path(__file__).resolve().parents[2]
        scenario = (root / "scenarios/public/known-zero-1tib-acceptance.yaml").read_text()
        helper = (root / "scripts/kz/host.py").read_text()
        def phase_minutes(name):
            phase = scenario.split("  - name: " + name + "\n", 1)[1].split("  - name:", 1)[0]
            return max(map(int, re.findall(r'timeout: "(\d+)m"', phase)))
        handoff, target, finish = [phase_minutes(n) for n in ("source-handoff", "target-run", "source-finish")]
        self.assertGreaterEqual(finish, 50, "step 5 follows target completion and scans 1 TiB")
        source_test, target_test = map(int, re.findall(r'"(\d+)m" if c\["role"\] == "source" else "(\d+)m"', helper)[1])
        source_unit, target_unit = map(int, re.search(r'limit = "(\d+)m" if .* else "(\d+)m"', helper).groups())
        wait = int(re.search(r'KZ_TARGET_WAIT="(\d+)m"', helper)[1])
        work = int(re.search(r'^timeout: (\d+)m', scenario, re.M)[1])
        always = int(re.search(r'^always_timeout: (\d+)m', scenario, re.M)[1])
        fallback = int(re.search(r'--on-active=(\d+)m', helper)[1])
        self.assertGreaterEqual(source_test, handoff + target + finish + 10)
        self.assertGreaterEqual(source_unit, source_test + 5)
        self.assertGreaterEqual(target_unit, target_test + 5)
        self.assertGreaterEqual(target, target_unit)
        self.assertGreaterEqual(wait, target + 5)
        self.assertGreaterEqual(work, source_unit + 15) # stage, signal, collect, judge
        self.assertGreaterEqual(fallback, work + always + 5)

if __name__ == "__main__": unittest.main()