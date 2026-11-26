"""plc_seal.py controls: the sealed packet is exactly the reviewed files plus the run pins and a manifest that
verifies; every refusal writes nothing; the candidate is never changed."""
import hashlib
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent


def sha(p):
    return hashlib.sha256(Path(p).read_bytes()).hexdigest()


class Seal(unittest.TestCase):
    def setUp(self):
        t = tempfile.TemporaryDirectory()
        self.addCleanup(t.cleanup)
        self.t = Path(t.name)
        self.cand = self.t/'pk'
        files = {'powerloss.py': 'def judge_run(*a):\n    return {}\n',
                 'drill/powerloss-fs.json': '{"schema": "powerloss-fs/v1", "fs_list": ["ext4", "xfs"]}\n',
                 'controls/test_x.py': 'pass\n',
                 'SHA256SUMS': 'stale manifest of the base seal\n',
                 'UNSEALED': 'candidate\n'}
        for rel, text in files.items():
            p = self.cand/rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(text.encode())
        (self.t/'bins').mkdir()
        (self.t/'bins'/'blockvolume').write_bytes(b'\x7fELF')
        self.write_list()
        self.producer = self.t/'producer'
        self.producer.mkdir()
        (self.producer/'plc_select.py').write_text('print(1)\n')
        self.write_producer_sums()
        self.tools = self.t/'tools'
        self.tools.mkdir()
        (self.tools/'plc_judge.py').write_text('judge\n')
        (self.tools/'test_unlisted.py').write_text('a test the manifest need not name\n')
        (self.tools/'plc-tools.sha256').write_text('%s  plc_judge.py\n' % sha(self.tools/'plc_judge.py'))
        self.head = 'a' * 40
        self.config = self.t/'config.json'
        self.config.write_text(json.dumps({'output': '/root/run/output/R1', 'module_sha256': 'm'}))

    def write_producer_sums(self):
        (self.producer/'SHA256SUMS').write_text(''.join('%s  %s\n' % (sha(p), p.name) for p in sorted(self.producer.iterdir()) if p.name != 'SHA256SUMS'))

    def write_list(self):
        rows = ['%s  pk/%s' % (sha(p), p.relative_to(self.cand).as_posix()) for p in sorted(self.cand.rglob('*')) if p.is_file()]
        rows.append('%s  bins/blockvolume' % sha(self.t/'bins'/'blockvolume'))
        self.list = self.t/'INPUT.sha256'
        self.list.write_text('\n'.join(rows) + '\n')
        self.pin = sha(self.list)

    def seal(self, out=None, pin=None):
        out = out or self.t/'sealed'
        return subprocess.run([sys.executable, '-I', str(HERE/'plc_seal.py'), '--candidate', str(self.cand),
                               '--input-list', str(self.list), '--input-pin', pin or self.pin, '--prefix', 'pk/',
                               '--config', str(self.config), '--producer', str(self.producer), '--tools', str(self.tools),
                               '--product-head', self.head, '--out', str(out)], capture_output=True, text=True, timeout=60)

    def refused(self, what):
        r = self.seal()
        self.assertEqual(r.returncode, 2, r.stdout + r.stderr)
        self.assertIn('REFUSED', r.stderr)
        self.assertIn(what, r.stderr)
        self.assertFalse((self.t/'sealed').exists(), 'a refusal wrote the sealed packet')
        self.assertFalse((self.t/'sealed.partial').exists(), 'a refusal left a partial seal')

    def test_seal_is_the_reviewed_files_plus_pins_and_a_verifying_manifest(self):
        before = {p.relative_to(self.cand).as_posix(): sha(p) for p in self.cand.rglob('*') if p.is_file()}
        r = self.seal()
        self.assertEqual(r.returncode, 0, r.stderr)
        out = self.t/'sealed'
        seal = sha(out/'SHA256SUMS')
        self.assertIn('SEALED %s files=4' % seal, r.stdout)
        self.assertIn('s %s approver=<approver>' % seal, r.stdout)
        self.assertFalse((out/'UNSEALED').exists())
        self.assertFalse((out/'bins').exists(), 'files under another prefix were copied')
        listed = {}
        for line in (out/'SHA256SUMS').read_text().splitlines():
            digest, rel = line.split('  ', 1)
            listed[rel] = digest
        on_disk = {p.relative_to(out).as_posix() for p in out.rglob('*') if p.is_file()} - {'SHA256SUMS'}
        self.assertEqual(set(listed), on_disk, 'the manifest does not cover exactly the sealed files')
        for rel, digest in listed.items():
            self.assertEqual(sha(out/rel), digest, rel)
        pins = json.loads((out/'drill'/'plc-run-pins.json').read_text())
        self.assertEqual(pins['config_sha256'], sha(self.config))
        self.assertEqual(pins['config_output'], '/root/run/output/R1')
        self.assertEqual(pins['candidate_input_pin'], self.pin)
        self.assertEqual(pins['producer_sha256'], sha(self.producer/'SHA256SUMS'))
        self.assertEqual(pins['tools_sha256'], sha(self.tools/'plc-tools.sha256'))
        self.assertEqual(pins['product_head'], self.head)
        after = {p.relative_to(self.cand).as_posix(): sha(p) for p in self.cand.rglob('*') if p.is_file()}
        self.assertEqual(before, after, 'the candidate changed')

    def test_wrong_input_pin_is_refused(self):
        r = self.seal(pin='0'*64)
        self.assertEqual(r.returncode, 2)
        self.assertIn('not the reviewed one', r.stderr)
        self.assertFalse((self.t/'sealed').exists())

    def test_changed_file_is_refused(self):
        (self.cand/'powerloss.py').write_text('def judge_run(*a):\n    return {"verdict": "PASS"}\n')
        self.refused('changed since review')

    def test_unlisted_file_is_refused(self):
        p = self.cand/'controls'/'__pycache__'/'test_x.cpython-312.pyc'
        p.parent.mkdir()
        p.write_bytes(b'\x00')
        self.refused('does not name')

    def test_missing_file_is_refused(self):
        (self.cand/'controls'/'test_x.py').unlink()
        self.refused('missing')

    def test_cr_in_a_text_file_is_refused(self):
        (self.cand/'controls'/'test_x.py').write_bytes(b'pass\r\n')
        self.write_list()
        self.refused('CR byte')

    def test_existing_out_is_refused(self):
        (self.t/'sealed').mkdir()
        r = self.seal()
        self.assertEqual(r.returncode, 2)
        self.assertIn('exists', r.stderr)
        self.assertEqual(list((self.t/'sealed').iterdir()), [])

    def test_changed_producer_is_refused(self):
        (self.producer/'plc_select.py').write_text('print(2)\n')
        self.refused('producer file changed')

    def test_unlisted_producer_file_is_refused(self):
        (self.producer/'extra.py').write_text('x\n')
        self.refused('producer files and its SHA256SUMS differ')

    def test_changed_tool_is_refused(self):
        (self.tools/'plc_judge.py').write_text('a different judge\n')
        self.refused('tool missing or changed')

    def test_short_product_head_is_refused(self):
        self.head = 'abc1234'
        self.refused('full 40-hex commit id')

    def test_config_without_output_is_refused(self):
        self.config.write_text('{"module_sha256": "m"}')
        self.refused('not a seaweed-cloud run config')


if __name__ == '__main__':
    unittest.main()
