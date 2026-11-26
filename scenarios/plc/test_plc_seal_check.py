"""plc_seal_check.sh controls (packet-free): a packet sealed by plc_seal.py and approved passes the dry/counted
seal check; every executed input off its pin refuses before any cloud call; a rehearsal over an unsealed packet
passes the check but records sealed=no (the gate then never counts it)."""
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
HEAD = 'b' * 40
TOOLS = ['plc_cleanup.sh', 'plc_gate.sh', 'plc_judge.py', 'plc_launch.sh', 'plc_owned.sh', 'plc_seal_check.sh', 'plc_settle.sh']
# Fixtures are LF bytes on every platform (write_text would translate newlines on Windows, and the seal tool
# rightly refuses a CR in a text file).
Path.write_text = lambda self, data, *a, **k: self.write_bytes(data.encode('utf-8'))


def sha(p):
    return hashlib.sha256(Path(p).read_bytes()).hexdigest()


def sums(d, manifest='SHA256SUMS', names=None):
    d = Path(d)
    names = names or sorted(p.relative_to(d).as_posix() for p in d.rglob('*') if p.is_file() and p.name != manifest)
    (d/manifest).write_text(''.join('%s  %s\n' % (sha(d/n), n) for n in names))


@unittest.skipIf(BASH is None, 'bash is required')
class SealCheck(unittest.TestCase):
    def setUp(self):
        t = tempfile.TemporaryDirectory()
        self.addCleanup(t.cleanup)
        self.t = Path(t.name)
        cand = self.t/'cand'
        for rel, text in {'powerloss.py': 'def judge_run(*a): return {}\n',
                          'drill/powerloss-fs.json': '{"fs_list": ["ext4", "xfs"]}\n', 'UNSEALED': 'x\n'}.items():
            (cand/rel).parent.mkdir(parents=True, exist_ok=True)
            (cand/rel).write_text(text)
        lst = self.t/'INPUT.sha256'
        lst.write_text(''.join('%s  c/%s\n' % (sha(p), p.relative_to(cand).as_posix()) for p in sorted(cand.rglob('*')) if p.is_file()))
        self.producer = self.t/'producer'
        self.producer.mkdir()
        (self.producer/'plc_select.py').write_text('print("select")\n')
        sums(self.producer)
        self.tools = self.t/'tools'
        self.tools.mkdir()
        for name in TOOLS:
            (self.tools/name).write_text(name + '\n')
        sums(self.tools, 'plc-tools.sha256', TOOLS)
        self.config = self.t/'config.json'
        self.config.write_text(json.dumps({'output': '/root/out/R1'}))
        self.packet = self.t/'sealed'
        r = subprocess.run([sys.executable, '-I', str(HERE/'plc_seal.py'), '--candidate', str(cand), '--input-list', str(lst),
                            '--input-pin', sha(lst), '--prefix', 'c/', '--config', str(self.config), '--producer',
                            str(self.producer), '--tools', str(self.tools), '--product-head', HEAD, '--out', str(self.packet)],
                           capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.seals = self.t/'approved-seals.txt'
        self.seals.write_text('s %s approver=test 2026-11-26T00:00:00Z %s\n' % (sha(self.packet/'SHA256SUMS'), self.packet))
        self.e = self.t/'evidence'
        self.e.mkdir()

    def check(self, cls='counted', head=HEAD, packet=None):
        p = subprocess.run([BASH, str(HERE/'plc_seal_check.sh'), str(self.e), cls, str(packet or self.packet), str(self.seals),
                            str(self.config), str(self.producer), str(self.tools), head, sys.executable],
                           capture_output=True, text=True, timeout=60)
        return p.returncode, p.stderr, (self.e/'seal.txt').read_text().strip() if (self.e/'seal.txt').exists() else ''

    def test_sealed_and_approved_passes(self):
        for cls in ('dry', 'counted'):
            with self.subTest(cls=cls):
                rc, err, seal = self.check(cls)
                self.assertEqual((rc, seal), (0, 'sealed=yes'), err)

    def test_refusals_before_any_cloud_call(self):
        cases = {
            'not approved': lambda: self.seals.write_text(''),
            'UNSEALED marker': lambda: (self.packet/'UNSEALED').write_text('x\n'),
            'packet file changed': lambda: (self.packet/'powerloss.py').write_text('def judge_run(*a): return {"verdict": "PASS"}\n'),
            'config changed': lambda: self.config.write_text(json.dumps({'output': '/elsewhere'})),
            'modified plc_select.py': lambda: (self.producer/'plc_select.py').write_text('print("other")\n'),
            'producer manifest rewritten': lambda: ((self.producer/'plc_select.py').write_text('print("other")\n'), sums(self.producer)),
            'tool changed': lambda: (self.tools/'plc_judge.py').write_text('other judge\n'),
            'unlisted producer file': lambda: (self.producer/'extra_module.py').write_text('import os\n'),
            # Both rewritten consistently and re-pinned would still need a new seal; here the pinned manifests are
            # replaced, so the pin refuses too, but the named-set checks are what the dedicated cases below isolate.
            'tools manifest without a tool': lambda: sums(self.tools, 'plc-tools.sha256', TOOLS[1:]),
            'producer without plc_select.py': lambda: ((self.producer/'plc_select.py').rename(self.producer/'other.py'), sums(self.producer)),
        }
        for name, change in cases.items():
            with self.subTest(case=name):
                self.setUp()
                change()
                rc, err, _ = self.check()
                self.assertEqual(rc, 1, '%s: %s' % (name, err))

    def repin(self, **pins):
        """A packet whose pins were made by something other than plc_seal.py (it refuses these sets): pins
        rewritten, manifest regenerated, seal approved, so only the named-set checks can refuse it."""
        p = self.packet/'drill'/'plc-run-pins.json'
        v = json.loads(p.read_text())
        v.update(pins)
        p.write_text(json.dumps(v))
        sums(self.packet)
        self.seals.write_text('s %s approver=test 2026-11-26T00:00:00Z %s\n' % (sha(self.packet/'SHA256SUMS'), self.packet))

    def test_named_sets_are_checked_even_when_pinned(self):
        with self.subTest(case='tools manifest without a tool, pinned'):
            sums(self.tools, 'plc-tools.sha256', TOOLS[1:])
            self.repin(tools_sha256=sha(self.tools/'plc-tools.sha256'))
            rc, err, _ = self.check()
            self.assertEqual(rc, 1, err)
            self.assertIn('not exactly the executed tools', err)
        with self.subTest(case='producer without plc_select.py, pinned'):
            self.setUp()
            (self.producer/'plc_select.py').rename(self.producer/'other.py')
            sums(self.producer)
            self.repin(producer_sha256=sha(self.producer/'SHA256SUMS'))
            rc, err, _ = self.check()
            self.assertEqual(rc, 1, err)
            self.assertIn('plc_select.py', err)

    def test_product_head_off_its_pin_is_refused(self):
        self.assertEqual(self.check(head='c' * 40)[0], 1)

    def test_rehearsal_over_unsealed_passes_the_check_but_records_sealed_no(self):
        self.seals.write_text('')
        rc, err, seal = self.check('rehearsal')
        self.assertEqual((rc, seal), (0, 'sealed=no'), err)


if __name__ == '__main__':
    unittest.main()
