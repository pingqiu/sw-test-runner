"""POSIX controls use only freshly created temporary directory trees."""
import os
from pathlib import Path
import subprocess
import time
import sys
import tempfile
import unittest


@unittest.skipUnless(os.name == 'posix', 'requires POSIX directory-fd operations')
class Removal(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='removal-control-')
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.packet = self.base / 'packet'
        self.packet.mkdir()
        self.root = self.base / 'runs' / 'sample'
        self.root.mkdir(parents=True)
        (self.root / 'kept').write_text('original')
        (self.packet / 'run.env').write_text(
            'RUN_ID=sample\nRUN_ID_RE=sample\nOWNED_ROOTS=runs/{run}\n'
            'TEST_SEAM_PREFIX=REMOVAL_CONTROL_\n')
        self.inventory = self.base / 'inventory' / 'proc' / 'self'
        self.inventory.mkdir(parents=True)
        self.mounts('/')
        self.environment = dict(os.environ, REMOVAL_CONTROL_ABS_ROOT=str(self.base / 'inventory'))
        self.source = Path(__file__).resolve().parent
        self.identity = self.devino(self.root)

    @staticmethod
    def devino(path):
        value = path.stat()
        return f'{value.st_dev}:{value.st_ino}'

    def mounts(self, *paths):
        lines = [f'{i} 0 0:1 / {path} rw - tmpfs tmpfs rw' for i, path in enumerate(paths, 1)]
        (self.inventory / 'mountinfo').write_text('\n'.join(lines) + '\n')

    def command(self, identity=None):
        program = (
            'import sys; from pathlib import Path; '
            'sys.path.insert(0, sys.argv[1]); from loader import load_rmguard; '
            'root=Path(sys.argv[1]); '
            'run=load_rmguard(root,(root/"library.sha256").read_text().strip()); '
            'run(sys.argv[2],sys.argv[3:])')
        return [sys.executable, '-I', '-c', program, str(self.source),
             str(self.packet / 'rmguard.py'), str(self.root), identity or self.identity]

    def invoke(self, identity=None):
        return subprocess.run(self.command(identity), env=self.environment,
                              capture_output=True, text=True, timeout=15)

    def test_removes_contents_and_root(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('RMGUARD DELETED', result.stdout)
        self.assertFalse(self.root.exists())

    def test_mount_at_root_or_below_refuses_and_retains_root(self):
        for mount in [self.root, self.root / 'nested']:
            with self.subTest(mount=mount):
                self.mounts('/', mount)
                result = self.invoke()
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                self.assertIn('REFUSED mounts under R', result.stdout)
                self.assertEqual((self.root / 'kept').read_text(), 'original')

    def test_wrong_identity_refuses_without_deletion(self):
        result = self.invoke('0:0')
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertIn('REFUSED identity', result.stdout)
        self.assertTrue((self.root / 'kept').exists())

    def test_malformed_mount_inventory_is_unknown(self):
        (self.inventory / 'mountinfo').write_text('not mountinfo\n')
        result = self.invoke()
        self.assertEqual(result.returncode, 3, result.stdout + result.stderr)
        self.assertIn('UNKNOWN', result.stdout)
        self.assertTrue((self.root / 'kept').exists())


    def test_missing_mount_inventory_is_unknown(self):
        (self.inventory / 'mountinfo').unlink()
        result = self.invoke()
        self.assertEqual(result.returncode, 3, result.stdout + result.stderr)
        self.assertTrue((self.root / 'kept').exists())

    def test_symlink_ancestor_is_refused(self):
        moved = self.base / 'moved-runs'
        self.root.parent.rename(moved)
        self.root.parent.symlink_to(moved, target_is_directory=True)
        result = self.invoke()
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertEqual((moved / 'sample' / 'kept').read_text(), 'original')

    def paused(self):
        pause = self.base / 'pause'
        self.environment['REMOVAL_CONTROL_RMGUARD_PAUSE'] = str(pause)
        process = subprocess.Popen(self.command(), env=self.environment,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        def cleanup():
            if process.poll() is None:
                process.kill()
            process.communicate(timeout=5)
        self.addCleanup(cleanup)
        deadline = time.monotonic() + 5
        while not pause.with_suffix('.ready').exists():
            self.assertIsNone(process.poll(), 'remover exited before identity check')
            self.assertLess(time.monotonic(), deadline, 'identity-check pause not reached')
            time.sleep(0.01)
        return process, pause

    def test_root_replacement_is_untouched(self):
        process, pause = self.paused()
        moved = self.root.with_name('original')
        self.root.rename(moved)
        self.root.mkdir()
        (self.root / 'impostor').write_text('untouched')
        pause.with_suffix('.go').touch()
        stdout, stderr = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 4, stdout + stderr)
        self.assertIn('INCOMPLETE', stdout)
        self.assertEqual((self.root / 'impostor').read_text(), 'untouched')
        self.assertEqual(list(moved.iterdir()), [])

    def test_parent_replacement_does_not_redirect_removal(self):
        process, pause = self.paused()
        original_parent = self.root.parent
        moved_parent = self.base / 'original-parent'
        original_parent.rename(moved_parent)
        self.root.mkdir(parents=True)
        (self.root / 'impostor').write_text('untouched')
        pause.with_suffix('.go').touch()
        stdout, stderr = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 0, stdout + stderr)
        self.assertFalse((moved_parent / 'sample').exists())
        self.assertEqual((self.root / 'impostor').read_text(), 'untouched')


if __name__ == '__main__':
    unittest.main()
