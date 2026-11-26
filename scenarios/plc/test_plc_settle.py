"""plc_settle.sh controls with stub seaweed-cloud and az (packet-free, no cloud): the teardown is proven in the
scope the pinned config names, and every way to look clean without being clean is refused."""
import hashlib
import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
BASH = shutil.which('bash')


@unittest.skipIf(BASH is None, 'bash is required')
class Settle(unittest.TestCase):
    def setUp(self):
        t = tempfile.TemporaryDirectory()
        self.addCleanup(t.cleanup)
        self.t = Path(t.name)
        self.e = self.t/'evidence'
        self.e.mkdir()
        (self.e/'started.txt').write_text('2026-11-26T00:00:00Z\n')
        self.variables = self.t/'run.tfvars.json'
        self.variables.write_text(json.dumps({'subscription_id': 'sub-1', 'resource_group_name': 'rg-run'}))
        self.config = self.t/'config.json'
        self.write_config()
        self.verify = {'code': 'EMPTY', 'remaining_resource_ids': [], 'create_unresolved': []}
        self.verify_rc, self.exists, self.listed = 0, 'true', []
        self.calls = self.t/'az-calls.txt'

    def write_config(self, pin=None):
        pin = pin or hashlib.sha256(self.variables.read_bytes()).hexdigest()
        self.config.write_text(json.dumps({'variables': str(self.variables).replace('\\', '/'), 'variables_sha256': pin}))

    def stubs(self):
        cloud, az = self.t/'cloud', self.t/'az'
        (self.t/'verify.json').write_text(json.dumps(self.verify))
        (self.t/'listed.json').write_text(json.dumps(self.listed))
        cloud.write_text('#!/bin/bash\ncat "%s"\nexit %d\n' % ((self.t/'verify.json').as_posix(), self.verify_rc))
        az.write_text('#!/bin/bash\necho "$*" >> "%s"\ncase "$1 $2" in\n'
                      '  "group exists") echo %s ;;\n  "resource list") cat "%s" ;;\n  *) exit 9 ;;\nesac\n'
                      % (self.calls.as_posix(), self.exists, (self.t/'listed.json').as_posix()))
        return cloud, az

    def settle(self, cls='counted'):
        cloud, az = self.stubs()
        p = subprocess.run([BASH, str(HERE/'plc_settle.sh'), str(self.e), cls, str(cloud), str(self.config),
                            sys.executable, str(az)], capture_output=True, text=True, timeout=60)
        return p.returncode, (self.e/'settle.txt').read_text() if (self.e/'settle.txt').exists() else ''

    def test_clean_teardown_in_the_pinned_scope(self):
        rc, note = self.settle()
        self.assertEqual(rc, 0, note)
        calls = self.calls.read_text()
        self.assertIn('--subscription sub-1 --name rg-run', calls)
        self.assertIn('--subscription sub-1 --resource-group rg-run', calls)

    def test_no_started_marker_or_rehearsal_settles_nothing(self):
        self.assertEqual(self.settle('rehearsal')[0], 0)
        (self.e/'started.txt').unlink()
        rc, note = self.settle('counted')
        self.assertEqual(rc, 0)
        self.assertIn('no cloud run', note)

    def test_refusals(self):
        cases = {
            'remaining ids': lambda: self.verify.update(remaining_resource_ids=['/sub/rg/vm']),
            'unresolved create': lambda: self.verify.update(create_unresolved=['vm']),
            'verify without its keys': lambda: self.verify.pop('remaining_resource_ids'),
            'verify keys not lists': lambda: self.verify.update(create_unresolved=None),
            'verify failed': lambda: setattr(self, 'verify_rc', 3),
            'group absent': lambda: setattr(self, 'exists', 'false'),
            'a listed resource': lambda: self.listed.append({'id': '/sub/rg/disk'}),
            'variables off their pin': lambda: self.write_config(pin='0' * 64),
        }
        for name, change in cases.items():
            with self.subTest(case=name):
                self.setUp()
                change()
                self.assertEqual(self.settle()[0], 1, name)


if __name__ == '__main__':
    unittest.main()
