import importlib.util
import pathlib
import os
import shlex
import subprocess
import sys
import tempfile
import unittest


spec = importlib.util.spec_from_file_location(
    'checker_commands', pathlib.Path(__file__).with_name('checker_commands.py'))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


class CheckerCommands(unittest.TestCase):
    def setUp(self):
        self.config = {'python': '/tools/python 3', 'packet': '/sealed/packet',
                       'node': 'controller', 'runner_receipt_dir': '/runner-only/commands'}

    def argv(self, action):
        self.assertEqual(action['action'], 'exec')
        self.assertNotIn('ignore_error', action)
        self.assertNotIn('save_as', action)
        return shlex.split(action['cmd'])

    def test_grant_is_direct_checker_with_literal_paths(self):
        a = m.grant(self.config, '/bundle/grant.txt', '/bundle/range.txt', 'move; echo bad')
        self.assertEqual(self.argv(a), ['exec', '/tools/python 3', '-I',
                         '/sealed/packet/lsncheck.py', 'grant', '/bundle/grant.txt',
                         '/bundle/range.txt', '--request', 'move; echo bad'])
        self.assertEqual(a['receipt_path'], str(pathlib.Path('/runner-only/commands/grant.json')))
        self.assertNotIn('runner-only', a['cmd'])

    def test_runner_receipt_is_local_even_when_checker_files_are_remote(self):
        with tempfile.TemporaryDirectory() as root:
            self.config['runner_receipt_dir'] = root
            self.config['node'] = 'm02'
            a = m.grant(self.config, '/remote-only/grant', '/remote-only/range', 'move')
            receipt = pathlib.Path(a['receipt_path'])
            self.assertEqual(receipt.parent, pathlib.Path(root))
            self.assertNotIn(root, a['cmd'])
            self.assertIn('/remote-only/grant', a['cmd'])
            # This path is consumed by beginReceipt before any node call.
            with receipt.open('x') as f:
                f.write('runner-local-intent')
            self.assertEqual(receipt.read_text(), 'runner-local-intent')

    def test_image_carries_checker_boundary_and_raced_row_without_deriving_them(self):
        a = m.image(self.config, 'restore-s1', '/b/ledger', '/b/image', '42',
                    blocks=100, seed='s', volume='g', raced='none')
        argv = self.argv(a)
        self.assertEqual(argv[4:8], ['image', '/b/ledger', '/b/image', '42'])
        self.assertEqual(argv[-2:], ['--raced', 'none'])
        self.assertNotIn('allow_not_counted', a)

    def test_summary_preserves_exact_host_statuses_and_order(self):
        a = m.summary(self.config, '/bundle', 'FAILED(d-load)', 'READY',
                      'UNKNOWN(rc=9)', 'CLEAN', 'pilot-r01')
        self.assertEqual(self.argv(a)[3:], ['/sealed/packet/summarize-go2.py',
                         '/bundle', 'FAILED(d-load)', 'READY', 'UNKNOWN(rc=9)',
                         'CLEAN', 'pilot-r01'])

    def test_stopbound_calls_only_python_checker(self):
        self.assertEqual(self.argv(m.stopbound(self.config, '/b/l', '/b/j'))[3:],
                         ['/sealed/packet/lsncheck.py', 'stopbound', '/b/l', '/b/j'])

    def test_refuses_receipt_path_escape(self):
        with self.assertRaises(ValueError):
            m.image(self.config, '../outside', '/l', '/i', '1', 1, 's', 'g', 'none')

    def test_gate_calls_the_sealed_build_gate(self):
        self.assertEqual(self.argv(m.gate(self.config))[3:],
                         ['/sealed/packet/build/gate.py', 'check', '/sealed/packet/GATE.json'])

    def test_hashes_explicit_output_files_without_interpreting_receipts(self):
        a = m.evidence_hashes(self.config, ['/bundle/commands/grant.json', '/bundle/m01-checks.txt'])
        self.assertEqual(self.argv(a), ['exec', 'sha256sum', '--',
                         '/bundle/commands/grant.json', '/bundle/m01-checks.txt'])
        with self.assertRaises(ValueError):
            m.evidence_hashes(self.config, [])

    def test_shell_preserves_failure_and_not_counted_without_injection(self):
        bash = os.environ.get('BASH_FOR_TESTS', 'bash')
        with tempfile.TemporaryDirectory() as root:
            path = pathlib.Path(root)
            c = {**self.config, 'python': pathlib.Path(sys.executable).as_posix(),
                 'packet': path.as_posix()}
            script = path / 'lsncheck.py'
            marker = path / 'unexpected'
            request = 'move; touch ' + marker.as_posix()
            for rc in (0, 1, 4):
                script.write_text('import sys\nassert sys.argv[-1] == ' +
                                  repr(request) + '\nraise SystemExit(' + str(rc) + ')\n')
                action = m.grant(c, '/grant', '/range', request)
                result = subprocess.run([bash, '-c', action['cmd']],
                                        capture_output=True, timeout=5)
                self.assertEqual(result.returncode, rc, result.stderr)
                self.assertFalse(marker.exists())


if __name__ == '__main__':
    unittest.main()
