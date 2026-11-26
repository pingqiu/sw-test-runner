"""The scenario's inline exec steps run under the controller's /bin/sh, which is dash on the Linux controllers (a real
VM run failed at unpack: dash refuses `set -o pipefail`). Controls (packet-free):
- every inline `cmd:` block uses no bash-only syntax (pipefail, [[ ]], <<<, arrays, ${x:...}, function);
- with dash available (always on Linux: a missing dash there is a failure, not a skip), every block parses under
  `dash -n`, and unpack, mark_pass and the collect owner step RUN under dash on fixtures;
- collect records a missing lout/ (ABSENT.txt, so it is collected and compared like the rest) and touches nothing
  in a directory this invocation does not own."""
import hashlib
import io
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
YAML = HERE.parent/'plc-powerloss-counted.yaml'
TOKEN = '0123456789abcdef0123456789abcdef'
DASH = shutil.which('dash') or ('/bin/sh' if os.path.realpath('/bin/sh').endswith('dash') else None)
BASHISMS = [(r'pipefail', 'set -o pipefail'), (r'\[\[', '[[ ]]'), (r'<<<', 'here-string'),
            (r'\$\{[A-Za-z_][A-Za-z0-9_]*:[-=?+]?[0-9]', 'substring expansion'), (r'^\s*function\s', 'function keyword'),
            (r'[A-Za-z_][A-Za-z0-9_]*=\(', 'array'), (r'\$\{[A-Za-z_][A-Za-z0-9_]*\[', 'array subscript')]


def steps():
    """(phase, index, cmd) for every `cmd: |` block, by indentation."""
    out, phase, lines = [], None, YAML.read_text(encoding='utf-8').splitlines()
    i = 0
    while i < len(lines):
        m = re.match(r'^  - name: (\S+)', lines[i])
        if m:
            phase = m[1]
        m = re.match(r'^(\s*)cmd: \|\s*$', lines[i])
        if m:
            ind, body, i = len(m[1]), [], i + 1
            while i < len(lines) and (not lines[i].strip() or len(lines[i]) - len(lines[i].lstrip()) > ind):
                body.append(lines[i][ind + 2:] if lines[i].strip() else '')
                i += 1
            out.append((phase, len(out), '\n'.join(body).rstrip() + '\n'))
            continue
        i += 1
    return out


def step(phase):
    found = [c for p, _, c in steps() if p == phase]
    assert len(found) == 1, '%s: %d inline steps' % (phase, len(found))
    return found[0]


def need_dash(test):
    if DASH is None:
        if sys.platform.startswith('linux'):
            test.fail('dash (the controllers\' /bin/sh) is missing on Linux: these controls must run, not skip')
        test.skipTest('no dash on this platform (CI runs these on Linux)')
    return DASH


def sha(b):
    return hashlib.sha256(b).hexdigest()


def shpath(p):
    """p as the shell's tools see it: on Windows (Git's dash) /d/x, since tar reads 'D:' as a remote host."""
    s = str(p)
    if os.name == 'nt' and re.match(r'^[A-Za-z]:', s):
        s = '/' + s[0].lower() + s[2:].replace('\\', '/')
    return s


class PosixSteps(unittest.TestCase):
    def test_inline_steps_use_no_bash_only_syntax(self):
        ss = steps()
        self.assertGreater(len(ss), 8, 'too few inline steps found: the extractor is broken')
        for phase, n, cmd in ss:
            for pat, what in BASHISMS:
                with self.subTest(phase=phase, step=n, construct=what):
                    self.assertIsNone(re.search(pat, cmd, re.M), '%s step %d uses %s' % (phase, n, what))

    def test_inline_steps_parse_under_dash(self):
        sh = need_dash(self)
        for phase, n, cmd in steps():
            with self.subTest(phase=phase, step=n):
                r = subprocess.run([sh, '-n', '-c', cmd], capture_output=True, text=True, timeout=30)
                self.assertEqual(r.returncode, 0, '%s step %d: %s' % (phase, n, r.stderr))

    def make_capture(self, d, tamper=False):
        files = {'info.txt': b'params FSLIST=\'ext4 xfs\' HOSTLOCK=1\n', 'ext4/x.tsv': b'x\n'}
        sums = ''.join('%s  %s\n' % (sha(b), n) for n, b in files.items())
        if tamper:
            sums = sums.replace(sha(files['ext4/x.tsv']), '0' * 64)
        (d/'capture').mkdir(parents=True)
        (d/'evidence').mkdir()
        with tarfile.open(d/'capture'/'out.tgz', 'w:gz') as tf:
            for n, b in list(files.items()) + [('SHA256SUMS', sums.encode())]:
                ti = tarfile.TarInfo('out/' + n)
                ti.size = len(b)
                tf.addfile(ti, io.BytesIO(b))

    def test_unpack_runs_under_dash(self):
        sh = need_dash(self)
        for name, tamper, want in (('verifies', False, 0), ('tampered', True, 1)):
            with self.subTest(case=name), tempfile.TemporaryDirectory() as t:
                d = Path(t)/'run'
                self.make_capture(d, tamper)
                r = subprocess.run([sh, '-c', step('unpack')], env=dict(os.environ, PLC_DIR=shpath(d)),
                                   capture_output=True, text=True, timeout=60)
                self.assertEqual(r.returncode, want, r.stderr)
                self.assertEqual((d/'evidence'/'info.txt').exists(), want == 0)

    def test_mark_pass_gate_runs_under_dash(self):
        sh = need_dash(self)
        with tempfile.TemporaryDirectory() as t:
            a = Path(t)/'plc-evidence-pass.tgz'
            with tarfile.open(a, 'w:gz') as tf:
                for n, b in (('evidence/settle.txt', b'settle: teardown verified in rg-run\n'),
                             ('evidence/judge.json', json.dumps({'verdict': 'PASS'}).encode()),
                             ('evidence/seal.txt', b'sealed=yes\n')):
                    ti = tarfile.TarInfo(n)
                    ti.size = len(b)
                    tf.addfile(ti, io.BytesIO(b))
            # mark_pass's step is the only inline cmd that reads PLC_ARCHIVE
            cmd = [c for p, _, c in steps() if p == 'mark_pass' and 'PLC_ARCHIVE' in c]
            self.assertEqual(len(cmd), 1)
            r = subprocess.run([sh, '-c', cmd[0]], env=dict(os.environ, PLC_ARCHIVE=shpath(a), PLC_CLASS='counted',
                               PLC_TOOLS=shpath(HERE), PLC_PY=sys.executable), capture_output=True, text=True, timeout=60)
            self.assertEqual((r.returncode, r.stdout.strip()), (0, '1'), r.stderr)

    def test_collect_records_a_missing_lout_only_in_its_own_directory(self):
        sh = need_dash(self)
        cmd = [c for p, _, c in steps() if p == 'collect' and 'plc_owned.sh' in c]
        self.assertEqual(len(cmd), 1)
        for name, token, owned in (('owned, produce never ran', TOKEN, True), ('not this invocation\'s', 'f' * 32, False)):
            with self.subTest(case=name), tempfile.TemporaryDirectory() as t:
                d = Path(t)/'run'
                (d/'evidence').mkdir(parents=True)
                (d/'.plc-owned').write_bytes((TOKEN + '\n').encode())
                r = subprocess.run([sh, '-c', cmd[0]], env=dict(os.environ, PLC_DIR=shpath(d), PLC_OWNER=token,
                                   PLC_TOOLS=shpath(HERE)), capture_output=True, text=True, timeout=60)
                self.assertEqual(r.returncode, 0 if owned else 1, r.stderr)
                self.assertEqual((d/'lout'/'ABSENT.txt').exists(), owned)
                if owned:
                    self.assertIn('produce did not run', (d/'lout'/'ABSENT.txt').read_text())


if __name__ == '__main__':
    unittest.main()
