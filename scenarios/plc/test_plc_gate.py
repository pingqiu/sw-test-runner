"""plc_gate.sh controls: gate_pass is 1 only for a counted run whose COLLECTED evidence says sealed=yes and
PASS; a PASS rehearsal or dry run is recorded with 0; a non-PASS or missing verdict gives no value (mark_pass
fails, gate_pass stays 0). Packet-free: runs in this repository's CI."""
import io
import json
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
BASH = shutil.which('bash')


@unittest.skipIf(BASH is None, 'bash is required')
class Gate(unittest.TestCase):
    def setUp(self):
        t = tempfile.TemporaryDirectory()
        self.addCleanup(t.cleanup)
        self.t = Path(t.name)

    def archive(self, verdict='PASS', seal='sealed=yes', judge=True):
        path = self.t/'evidence.tgz'
        with tarfile.open(path, 'w:gz') as tar:
            def add(name, text):
                data = text.encode()
                info = tarfile.TarInfo('evidence/' + name)
                info.size = len(data)
                tar.addfile(info, io.BytesIO(data))
            if judge:
                add('judge.json', json.dumps({'verdict': verdict}))
            if seal is not None:
                add('seal.txt', seal + '\n')
        return path

    def gate(self, archive, cls):
        p = subprocess.run([BASH, str(HERE/'plc_gate.sh'), str(archive), cls, sys.executable],
                           capture_output=True, text=True, timeout=60)
        return p.returncode, p.stdout.strip()

    def test_counted_sealed_pass_is_one(self):
        self.assertEqual(self.gate(self.archive(), 'counted'), (0, '1'))

    def test_rehearsal_over_an_unsealed_packet_that_passes_is_zero(self):
        self.assertEqual(self.gate(self.archive(seal='sealed=no'), 'rehearsal'), (0, '0'))

    def test_rehearsal_and_dry_with_a_sealed_packet_are_zero(self):
        for cls in ('rehearsal', 'dry'):
            with self.subTest(cls=cls):
                self.assertEqual(self.gate(self.archive(), cls), (0, '0'))

    def test_counted_unsealed_is_zero(self):
        self.assertEqual(self.gate(self.archive(seal='sealed=no'), 'counted'), (0, '0'))

    def test_non_pass_or_missing_verdict_gives_no_value(self):
        for name, archive in (('fail', self.archive(verdict='FAIL')), ('unknown', self.archive(verdict='UNKNOWN')),
                              ('no judge', self.archive(judge=False)), ('no seal', self.archive(seal=None))):
            with self.subTest(case=name):
                rc, out = self.gate(archive, 'counted')
                self.assertNotEqual(rc, 0)
                self.assertEqual(out, '')


if __name__ == '__main__':
    unittest.main()
