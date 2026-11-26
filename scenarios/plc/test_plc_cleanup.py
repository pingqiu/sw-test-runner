"""plc_owned.sh and plc_cleanup.sh controls (packet-free): an always-phase step touches only this invocation's
run directory, and cleanup removes it only when every existing evidence/capture/lout directory is collected
exactly; a failed or partial collection keeps it."""
import shutil
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
BASH = shutil.which('bash')
TOKEN = '0123456789abcdef0123456789abcdef'


@unittest.skipIf(BASH is None, 'bash is required')
class Cleanup(unittest.TestCase):
    def setUp(self):
        t = tempfile.TemporaryDirectory()
        self.addCleanup(t.cleanup)
        self.t = Path(t.name)
        self.d = self.t/'run'
        for rel in ('evidence/judge.json', 'evidence/final/settle.txt', 'capture/out.tgz', 'lout/ext4/plc/a'):
            (self.d/rel).parent.mkdir(parents=True, exist_ok=True)
            (self.d/rel).write_bytes(b'x')
        (self.d/'.plc-owned').write_bytes((TOKEN + '\n').encode())
        self.art = self.t/'artifacts'
        self.art.mkdir()

    def collect(self, *subs):
        """As collect_path does: tar -czf <name>.tgz -C <parent> <base>."""
        for sub in subs:
            with tarfile.open(self.art/('plc-%s.tgz' % sub), 'w:gz') as tf:
                tf.add(self.d/sub, arcname=sub)

    def run_cleanup(self, token=TOKEN):
        return subprocess.run([BASH, str(HERE/'plc_cleanup.sh'), str(self.d), token, str(self.art)],
                              capture_output=True, text=True, timeout=60)

    def test_removed_once_everything_is_collected(self):
        self.collect('evidence', 'capture', 'lout')
        r = self.run_cleanup()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(self.d.exists())

    def test_a_directory_that_never_existed_needs_no_archive(self):
        shutil.rmtree(self.d/'lout')
        self.collect('evidence', 'capture')
        self.assertEqual(self.run_cleanup().returncode, 0)

    def test_kept_when_collection_failed_or_is_partial(self):
        cases = {
            'lout not collected': lambda: self.collect('evidence', 'capture'),
            'archive unreadable': lambda: (self.collect('evidence', 'capture'), (self.art/'plc-lout.tgz').write_bytes(b'not a tar')),
            'archive missing a file': lambda: (self.collect('evidence', 'capture', 'lout'), (self.d/'lout'/'late').write_bytes(b'y')),
        }
        for name, prepare in cases.items():
            with self.subTest(case=name):
                self.setUp()
                prepare()
                r = self.run_cleanup()
                self.assertEqual(r.returncode, 1, name)
                self.assertIn('keeping', r.stderr)
                self.assertTrue((self.d/'evidence'/'judge.json').exists())

    def test_never_touches_a_directory_this_invocation_does_not_own(self):
        self.collect('evidence', 'capture', 'lout')
        for name, token in {'prepare acquired nothing': '{{ plc_token }}', 'another token': 'f' * 32}.items():
            with self.subTest(case=name):
                r = self.run_cleanup(token)
                self.assertEqual(r.returncode, 1, name)
                self.assertTrue(self.d.exists())

    def test_owned_check(self):
        ok = subprocess.run([BASH, str(HERE/'plc_owned.sh'), str(self.d), TOKEN], capture_output=True, timeout=30)
        self.assertEqual(ok.returncode, 0)
        for token in ('', '{{ plc_token }}', 'f' * 32, TOKEN.upper()):
            r = subprocess.run([BASH, str(HERE/'plc_owned.sh'), str(self.d), token], capture_output=True, timeout=30)
            self.assertEqual(r.returncode, 1, repr(token))


if __name__ == '__main__':
    unittest.main()
