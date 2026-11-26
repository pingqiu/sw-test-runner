"""plc_settle.sh controls with stub seaweed-cloud and az (packet-free, no cloud). The verify output is the real
Result contract: fixtures/verify-*.json were encoded by provision/smoke's own verify path (a fake Azure listing,
json.Encoder as cmd/seaweed-cloud uses), not written by hand. Covered: the terminal barrier on a cloud child
that is still running (launched through plc_launch.sh, as the scenario does), admission by this invocation's
launch receipt, and every way to look clean without being clean."""
import hashlib
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
BASH = shutil.which('bash')
TOKEN = 'a1b2c3d4e5f60718293a4b5c'
EMPTY = json.loads((HERE/'fixtures'/'verify-empty.json').read_text(encoding='utf-8'))
REMAIN = json.loads((HERE/'fixtures'/'verify-remain.json').read_text(encoding='utf-8'))


def posix(p):
    return str(p).replace('\\', '/')


@unittest.skipIf(BASH is None, 'bash is required')
class Settle(unittest.TestCase):
    def setUp(self):
        t = tempfile.TemporaryDirectory()
        self.addCleanup(t.cleanup)
        self.t = Path(t.name)
        self.e = self.t/'evidence'
        self.e.mkdir()
        self.output = self.t/'output'
        self.variables = self.t/'run.tfvars.json'
        self.variables.write_bytes(json.dumps({'subscription_id': 'sub-1', 'resource_group_name': 'rg-run'}).encode())
        self.config = self.t/'config.json'
        self.write_config()
        self.verify = json.loads(json.dumps(EMPTY))
        self.verify_rc, self.exists, self.listed = 0, 'true', []
        self.calls = self.t/'calls.txt'
        self.children = []
        self.addCleanup(self.reap)
        self.launched()      # a cloud child that ran and exited
        self.admitted()      # ... after seaweed-cloud admitted it

    def reap(self):
        for p in self.children:
            if p.poll() is None:
                p.kill()
                p.wait()

    def write_config(self, pin=None):
        pin = pin or hashlib.sha256(self.variables.read_bytes()).hexdigest()
        self.config.write_bytes(json.dumps({'output': posix(self.output), 'variables': posix(self.variables),
                                            'variables_sha256': pin}).encode())

    def launch(self, *cmd):
        """Start cmd through plc_launch.sh exactly as cloud_run does (a fresh launch: no earlier record or
        receipt); return once it recorded its identity."""
        for name in ('cloud.pid', 'cloud.pid.child', 'cloud.pid.exit'):
            (self.e/name).unlink(missing_ok=True)
        (self.e/'launching').write_bytes(b'')
        p = subprocess.Popen([BASH, str(HERE/'plc_launch.sh'), posix(self.e/'cloud.pid')] + list(cmd))
        self.children.append(p)
        deadline = time.time() + 20
        while not (self.e/'cloud.pid').exists() and time.time() < deadline:
            time.sleep(0.1)
        self.assertTrue((self.e/'cloud.pid').exists(), 'plc_launch.sh recorded no identity')
        return p

    def launched(self):
        self.launch('true').wait(timeout=20)

    def admitted(self, token=TOKEN):
        self.output.mkdir(exist_ok=True)
        (self.output/'launch.json').write_bytes(json.dumps({'schema': 'seaweed.cloud-launch/v1', 'launch_token': token,
                                                           'admitted_at': '2026-11-26T00:00:00Z'}).encode())

    def stubs(self):
        cloud, az = self.t/'cloud', self.t/'az'
        (self.t/'verify.json').write_bytes((json.dumps(self.verify) + '\n').encode())
        (self.t/'listed.json').write_bytes(json.dumps(self.listed).encode())
        cloud.write_bytes(('#!/bin/bash\necho "cloud $*" >> "%s"\ncat "%s"\nexit %d\n'
                           % (posix(self.calls), posix(self.t/'verify.json'), self.verify_rc)).encode())
        az.write_bytes(('#!/bin/bash\necho "az $*" >> "%s"\ncase "$1 $2" in\n'
                        '  "group exists") echo %s ;;\n  "resource list") cat "%s" ;;\n  *) exit 9 ;;\nesac\n'
                        % (posix(self.calls), self.exists, posix(self.t/'listed.json'))).encode())
        for stub in (cloud, az):
            stub.chmod(0o755)
        return cloud, az

    def settle(self, cls='counted', wait='4'):
        cloud, az = self.stubs()
        env = dict(os.environ, PLC_SETTLE_WAIT=wait)
        p = subprocess.run([BASH, str(HERE/'plc_settle.sh'), str(self.e), str(self.e), cls, str(cloud), str(self.config),
                            sys.executable, TOKEN, str(az)], capture_output=True, text=True, timeout=120, env=env)
        note = (self.e/'settle.txt').read_text() if (self.e/'settle.txt').exists() else ''
        return p.returncode, note + p.stderr

    def calls_made(self):
        return self.calls.read_text() if self.calls.exists() else ''

    def test_clean_teardown_in_the_pinned_scope(self):
        rc, note = self.settle()
        self.assertEqual(rc, 0, note)
        self.assertIn('teardown verified in rg-run', note)
        calls = self.calls_made()
        self.assertIn('az group exists --subscription sub-1 --name rg-run', calls)
        self.assertIn('az resource list --subscription sub-1 --resource-group rg-run', calls)

    def test_nothing_to_settle_without_a_launch(self):
        self.assertEqual(self.settle('rehearsal')[0], 0)
        (self.e/'launching').unlink()
        rc, note = self.settle()
        self.assertEqual(rc, 0, note)
        self.assertIn('no cloud run was launched', note)
        self.assertEqual(self.calls_made(), '')

    def test_not_admitted_touches_nothing(self):
        shutil.rmtree(self.output)
        rc, note = self.settle()
        self.assertEqual(rc, 0, note)
        self.assertIn('not admitted', note)
        self.assertEqual(self.calls_made(), '', 'verify ran for an invocation that was never admitted')
        self.assertFalse(self.output.exists())

    def test_output_without_our_receipt_is_unknown_and_untouched(self):
        for name, prepare in {'another run': lambda: self.admitted('f' * 24),
                              'no receipt': lambda: (self.output/'launch.json').unlink()}.items():
            with self.subTest(case=name):
                self.setUp()
                prepare()
                before = sorted(p.name for p in self.output.iterdir())
                rc, note = self.settle()
                self.assertEqual(rc, 1, note)
                self.assertIn('UNKNOWN', note)
                self.assertEqual(self.calls_made(), '')
                self.assertEqual(sorted(p.name for p in self.output.iterdir()), before)

    def test_a_cloud_child_still_running_is_unknown_never_observed(self):
        (self.e/'cloud.pid').unlink()
        self.launch('sleep', '60')
        rc, note = self.settle(wait='3')
        self.assertEqual(rc, 1, note)
        self.assertIn('still running', note)
        self.assertTrue((self.e/'recovery-handle.txt').exists())
        self.assertEqual(self.calls_made(), '', 'inventory observed while the cloud child was running')

    def test_a_cloud_child_that_finishes_within_the_wait_is_settled(self):
        (self.e/'cloud.pid').unlink()
        self.launch('sleep', '3')
        rc, note = self.settle(wait='30')
        self.assertEqual(rc, 0, note)

    def test_cancel_stand_in_shell_killed_child_still_active(self):
        """As the runner cancels cloud_run: only the step's shell is killed. The launcher and its cloud child keep
        running; settle must not observe inventory until the child has exited, and with a short wait it is
        UNKNOWN."""
        for name in ('cloud.pid', 'cloud.pid.child', 'cloud.pid.exit'):
            (self.e/name).unlink(missing_ok=True)
        (self.e/'launching').write_bytes(b'')
        shell = subprocess.Popen([BASH, '-c', 'bash "$1" "$2" sleep 6 < /dev/null > /dev/null 2>&1 & wait "$!"', '_',
                                  posix(HERE/'plc_launch.sh'), posix(self.e/'cloud.pid')])
        self.children.append(shell)
        deadline = time.time() + 20
        while not (self.e/'cloud.pid').exists() and time.time() < deadline:
            time.sleep(0.1)
        shell.kill()
        shell.wait(timeout=20)
        rc, note = self.settle(wait='1')
        self.assertEqual(rc, 1, note)
        self.assertIn('still running', note)
        self.assertEqual(self.calls_made(), '')
        started = time.time()
        rc, note = self.settle(wait='30')
        self.assertEqual(rc, 0, note)
        self.assertGreater(time.time() - started, 1.0, 'settled without waiting for the active child')

    def test_a_launcher_killed_without_a_receipt_is_unknown(self):
        """The launcher dies abnormally (SIGKILL, as an OOM kill would) while the cloud child keeps running: no
        terminal receipt, so the child's termination is unproven and nothing is observed."""
        for name in ('cloud.pid', 'cloud.pid.exit', 'cloud.pid.child'):
            (self.e/name).unlink(missing_ok=True)
        self.launch('sleep', '30')
        deadline = time.time() + 20
        while not (self.e/'cloud.pid.child').exists() and time.time() < deadline:
            time.sleep(0.1)
        launcher = (self.e/'cloud.pid').read_text().split()[0]
        child = (self.e/'cloud.pid.child').read_text().split()[0]
        try:
            subprocess.run([BASH, '-c', 'kill -9 $0', launcher], timeout=30)
            rc, note = self.settle(wait='6')
            self.assertEqual(rc, 1, note)
            self.assertIn('without a terminal receipt', note)
            self.assertEqual(self.calls_made(), '', 'inventory observed with the cloud child unaccounted for')
            self.assertFalse((self.e/'cloud.pid.exit').exists())
        finally:
            subprocess.run([BASH, '-c', 'kill -9 $0 2>/dev/null; true', child], timeout=30)

    def test_the_launcher_reports_the_childs_status(self):
        for name in ('cloud.pid', 'cloud.pid.exit'):
            (self.e/name).unlink(missing_ok=True)
        p = self.launch('bash', '-c', 'exit 7')
        self.assertEqual(p.wait(timeout=20), 7)
        self.assertEqual((self.e/'cloud.pid.exit').read_text().strip(), '7')

    def test_a_launch_without_a_recorded_identity_is_unknown(self):
        (self.e/'cloud.pid').unlink()
        rc, note = self.settle()
        self.assertEqual(rc, 1, note)
        self.assertIn('recorded no identity', note)
        self.assertEqual(self.calls_made(), '')

    def test_refusals(self):
        # Each refusal must be for its own reason: an exit 1 for another cause (a stub that cannot run, say)
        # would pass a bare rc check vacuously.
        cases = {
            'remaining ids (real Result)': (lambda: setattr(self, 'verify', json.loads(json.dumps(REMAIN))), 'remaining_resource_ids'),
            'create unresolved': (lambda: self.verify.update(create_unresolved=True), 'create_unresolved'),
            'create_unresolved absent': (lambda: self.verify.pop('create_unresolved'), 'create_unresolved'),
            'create_unresolved an empty list': (lambda: self.verify.update(create_unresolved=[]), 'create_unresolved'),
            'remaining not observed': (lambda: self.verify.update(remaining_observed=False), 'remaining_observed'),
            'remaining_observed absent': (lambda: self.verify.pop('remaining_observed'), 'remaining_observed'),
            'remaining ids null': (lambda: self.verify.update(remaining_resource_ids=None), 'remaining_resource_ids'),
            'code not EMPTY': (lambda: self.verify.update(code='CREATE_UNRESOLVED'), 'code'),
            'another schema': (lambda: self.verify.update(schema='seaweed.cloud-smoke/v0'), 'schema'),
            'verify failed': (lambda: setattr(self, 'verify_rc', 1), 'verify exited 1'),
            'group absent': (lambda: setattr(self, 'exists', 'false'), 'does not exist'),
            'a listed resource': (lambda: self.listed.append({'id': '/sub/rg/disk'}), 'REMAINING in the independent listing'),
            'variables off their pin': (lambda: self.write_config(pin='0' * 64), 'cannot derive the run scope'),
        }
        for name, (change, reason) in cases.items():
            with self.subTest(case=name):
                self.setUp()
                change()
                rc, note = self.settle()
                self.assertEqual(rc, 1, '%s: %s' % (name, note))
                self.assertIn(reason, note, name)

    def test_the_stubs_run(self):
        """Guard for the controls above: the stub seaweed-cloud and az execute (exit 0 on the clean case)."""
        cloud, az = self.stubs()
        for stub, args in ((cloud, ''), (az, ' group exists')):
            r = subprocess.run([BASH, '-c', 'exec $0' + args, posix(stub)], capture_output=True, timeout=30)
            self.assertEqual(r.returncode, 0, '%s does not run: %r' % (stub.name, r.stderr))


if __name__ == '__main__':
    unittest.main()
