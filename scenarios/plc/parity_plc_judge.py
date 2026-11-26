"""Parity: the scenario's controller judge (plc_judge.py, run as the scenario runs it) gives the verdict the
packet's own judge_run gives, on the packet's own per-filesystem fixtures, for the passing run and for each
refusal the packet's controls assert. Nothing here re-implements a verdict.

PLC_PACKET=/path/to/packet python3 -m unittest parity_plc_judge   (the packet's controls/ build the fixtures)
"""
import hashlib
import json
import os
import runpy
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
PK = Path(os.environ.get('PLC_PACKET', ''))
if not (PK/'powerloss.py').is_file():
    raise SystemExit('PLC_PACKET must name a power-loss packet (powerloss.py, controls/, drill/powerloss-fs.json)')
sys.dont_write_bytecode = True  # importing the packet's controls must not write into the packet
sys.path.insert(0, str(PK/'controls'))
from test_nonprefix_attempts import FIX, RUN, read, write  # noqa: E402  the packet's own fixture builders
import test_nonprefix_attempts as attempt_fixture  # noqa: E402

CASES = ['pl-flushed-exact', 'pl-nonprefix', 'pl-quiet-certified', 'pl-oracle-control']
GOOD_INFO = "params FSLIST='ext4 xfs' NC=3\n"


def volumes(x, value):
    if isinstance(x, dict):
        for k, v in x.items():
            if k == 'volume_id':
                x[k] = value
            else:
                volumes(v, value)
    elif isinstance(x, list):
        for v in x:
            volumes(v, value)


class Parity(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.run_root = Path(self.temp.name)/'run'
        lout = self.run_root/'lout'
        for fs in ('ext4', 'xfs'):
            plc = lout/fs/'plc'
            plc.mkdir(parents=True)
            helper = attempt_fixture.Attempts()
            helper.root = plc/'pl-nonprefix'
            helper.root.mkdir()
            helper.build(3, fs)
            for name in CASES:
                if name != 'pl-nonprefix':
                    FIX(plc/name, RUN)
            for p in plc.rglob('*.json'):
                x = read(p)
                volumes(x, fs + '-v1')
                write(p, x)
        self.info(GOOD_INFO)

    def info(self, text):
        out = self.run_root/'out'
        out.mkdir(exist_ok=True)
        metadata, manifest = out/'info.txt', out/'SHA256SUMS'
        if text is None:
            metadata.unlink(missing_ok=True)
            manifest.unlink(missing_ok=True)
            return
        metadata.write_text(text)
        manifest.write_text(hashlib.sha256(metadata.read_bytes()).hexdigest() + '  info.txt\n')

    def both(self):
        """(verdict and exit of the scenario's judge, verdict of the packet's judge_run)."""
        out = Path(self.temp.name)/'judge.json'
        p = subprocess.run([sys.executable, '-I', str(HERE/'plc_judge.py'), str(PK), str(self.run_root), RUN, str(out)],
                           capture_output=True, text=True, timeout=120)
        scenario = json.loads(out.read_text())['verdict']
        direct = runpy.run_path(str(PK/'powerloss.py'))['judge_run'](
            self.run_root, RUN, list(CASES), PK/'drill'/'powerloss-fs.json', self.run_root/'out'/'info.txt')['verdict']
        return scenario, p.returncode, direct

    def expect(self, want):
        scenario, rc, direct = self.both()
        self.assertEqual(direct, want, 'the packet judge itself')
        self.assertEqual(scenario, direct, 'the scenario judge differs from the packet judge')
        self.assertEqual(rc, {'PASS': 0, 'FAIL': 1}.get(want, 2), 'exit code')

    def test_both_filesystems_pass(self):
        self.expect('PASS')

    def test_missing_metadata_is_unknown(self):
        self.info(None)
        self.expect('UNKNOWN')

    def test_mismatched_fslist_is_unknown(self):
        for text in ("params FSLIST='xfs ext4'\n", "params FSLIST='ext4'\n"):
            with self.subTest(text=text):
                self.info(text)
                self.expect('UNKNOWN')

    def test_metadata_off_the_manifest_is_unknown(self):
        (self.run_root/'out'/'info.txt').write_text("params FSLIST='ext4 xfs' NC=4\n")
        self.expect('UNKNOWN')

    def test_missing_filesystem_tree_is_unknown(self):
        import shutil
        shutil.rmtree(self.run_root/'lout'/'xfs')
        self.expect('UNKNOWN')

    def test_extra_filesystem_tree_is_unknown(self):
        (self.run_root/'lout'/'extra').mkdir()
        self.expect('UNKNOWN')

    def test_a_proven_product_fail_is_fail(self):
        p = self.run_root/'lout'/'ext4'/'plc'/'pl-flushed-exact'/'published.observation.json'
        r = read(p)
        r['chunks'][0]['sha256'] = 'f'*64
        write(p, r)
        self.expect('FAIL')

    def expect_not_pass(self):
        scenario, rc, direct = self.both()
        self.assertNotEqual(direct, 'PASS', 'the packet judge itself passed an incomplete run')
        self.assertEqual(scenario, direct, 'the scenario judge differs from the packet judge')
        self.assertNotEqual(rc, 0, 'a non-PASS exited 0')

    def test_one_missing_case_on_either_filesystem_is_not_pass(self):
        import shutil
        for fs in ('ext4', 'xfs'):
            for case in CASES:
                with self.subTest(fs=fs, case=case):
                    self.setUp()
                    shutil.rmtree(self.run_root/'lout'/fs/'plc'/case)
                    self.expect_not_pass()

    def test_truncated_case_data_is_not_pass(self):
        for fs in ('ext4', 'xfs'):
            with self.subTest(fs=fs):
                self.setUp()
                p = self.run_root/'lout'/fs/'plc'/'pl-flushed-exact'/'published.observation.json'
                raw = p.read_bytes()
                p.write_bytes(raw[:len(raw)//2])
                self.expect_not_pass()

    def test_invalid_nonprefix_obligation_is_not_pass(self):
        for fs in ('ext4', 'xfs'):
            with self.subTest(fs=fs):
                self.setUp()
                root = self.run_root/'lout'/fs/'plc'/'pl-nonprefix'
                containers = sorted(p for p in root.glob('*.json'))
                self.assertTrue(containers, 'the fixture has no pl-nonprefix container json')
                containers[0].write_text('{"schema": "not-the-nonprefix-attempts-schema"}')
                self.expect_not_pass()

    def test_unreadable_packet_is_unknown_never_pass(self):
        out = Path(self.temp.name)/'judge.json'
        p = subprocess.run([sys.executable, '-I', str(HERE/'plc_judge.py'), str(Path(self.temp.name)/'no-packet'),
                            str(self.run_root), RUN, str(out)], capture_output=True, text=True, timeout=120)
        self.assertEqual((json.loads(out.read_text())['verdict'], p.returncode), ('UNKNOWN', 2))


if __name__ == '__main__':
    unittest.main()
