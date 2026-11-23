"""Synthetic controls for shared resource-absence decisions and observation."""
from pathlib import Path
from contextlib import redirect_stdout
import io
import os
import subprocess
import tempfile
from unittest.mock import patch
import resource_inventory
import types
import unittest
import facts


class AbsenceDecision(unittest.TestCase):
    def test_unknown_takes_precedence_over_leftovers(self):
        cases = [([], [], 'PASS', []), (['PORT 9'], [], 'FAIL', ['PORT 9']),
                 ([], ['read failed'], 'UNKNOWN', ['read failed']),
                 (['PORT 9'], ['read failed'], 'UNKNOWN', ['read failed', 'PORT 9'])]
        for leftovers, errors, verdict, details in cases:
            with self.subTest(leftovers=leftovers, errors=errors):
                result = facts.resources_absent(leftovers, errors)
                self.assertEqual(result, {'verdict': verdict, 'details': details})


class AbsenceInventory(unittest.TestCase):
    def observe(self, change=None, unit_code=0, unit_output='', args=('host',)):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            packet = root / 'case' / 'pk'
            packet.mkdir(parents=True)
            fake = root / 'inventories'
            files = {
                'proc/net/tcp': 'sl local_address rem_address st\n',
                'proc/net/tcp6': 'sl local_address rem_address st\n',
                'proc/self/mountinfo': '1 0 0:0 / / rw - tmpfs tmpfs rw\n',
                'proc/12345/stat': '12345 (synthetic) S 1 0\n',
            }
            for name, text in files.items():
                path = fake / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(text)
            (fake / 'dev').mkdir()
            (packet / 'run.env').write_text(
                'RUN_ID=case\nRUN_ID_RE=case\nHOSTS=host\nPORTS=9\n'
                'NQN_PREFIX=nqn.synthetic\nUNIT_GLOB=case-*\nOWNED_ROOTS={run}-owned\n'
                'EXTRA_DEVICES=/dev/extra*\nCHECK_LOOP=1\nCHECK_FIRECRACKER=1\nTEST_SEAM_PREFIX=CASE_\n')
            if change:
                change(fake, root)
            output = io.StringIO()
            completed = subprocess.CompletedProcess([], unit_code, unit_output, '')
            with patch.dict(os.environ, {'CASE_ABS_ROOT': str(fake)}), \
                 patch.object(resource_inventory.os, 'getpid', return_value=12345), \
                 patch.object(resource_inventory.subprocess, 'run', return_value=completed), \
                 redirect_stdout(output):
                code = resource_inventory.main(str(packet / 'absence.py'), list(args), facts.resources_absent)
            return code, output.getvalue()

    def test_complete_empty_inventory_passes(self):
        code, output = self.observe()
        self.assertEqual(code, 0)
        self.assertTrue(output.startswith('ABSENCE-CLEAN host=host '))

    def test_unreadable_inventory_is_unknown(self):
        code, output = self.observe(lambda fake, root: (fake / 'proc/net/tcp').unlink())
        self.assertEqual(code, 3)
        self.assertIn('ABSENCE-UNKNOWN:', output)
        self.assertIn('PORT /proc/net/tcp', output)

    def test_failed_producer_is_unknown_even_with_output(self):
        code, output = self.observe(unit_code=1, unit_output='case-task.service active')
        self.assertEqual(code, 3)
        self.assertIn('UNIT systemctl exit 1', output)

    def test_port_and_unit_leftovers_fail(self):
        def listener(fake, root):
            (fake / 'proc/net/tcp').write_text('sl local_address rem_address st\n0: 00000000:0009 00000000:0000 0A\n')
        for change, units, detail in [(listener, '', 'PORT 9'), (None, 'case-task.service active', 'UNIT case-task.service')]:
            code, output = self.observe(change, unit_output=units)
            self.assertEqual(code, 1)
            self.assertTrue(output.startswith('LEFTOVER:'))
            self.assertIn(detail, output)

    def test_self_unit_is_excluded(self):
        code, output = self.observe(unit_output='case-task.service active', args=('host', '--self-unit', 'case-task'))
        self.assertEqual(code, 0)

    def test_scope_and_usage_stay_explicit(self):
        code, output = self.observe(args=('another-host',))
        self.assertEqual(code, 2)
        self.assertIn('use: absence.py host', output)


class AbsenceMutant(unittest.TestCase):
    def test_ignoring_failed_reads_would_falsely_pass(self):
        source = Path(facts.__file__).read_text()
        self.assertEqual(source.count('if errors:'), 1)
        mutant = types.ModuleType('absence_mutant')
        exec(compile(source.replace('if errors:', 'if False:'), '<synthetic-absence-mutant>', 'exec'), mutant.__dict__)
        self.assertEqual(facts.resources_absent([], ['inventory unreadable'])['verdict'], 'UNKNOWN')
        self.assertEqual(mutant.resources_absent([], ['inventory unreadable'])['verdict'], 'PASS')


if __name__ == '__main__':
    unittest.main()
