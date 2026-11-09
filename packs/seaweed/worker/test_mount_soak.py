"""Caller controls for the mount soak load: teardown owns the rule and the mounts on every path, and absence is only
ever proven by a successful check. No FUSE, iptables or network: the effects are recorded by fakes."""
import json, tempfile, threading, unittest
from pathlib import Path
from unittest import mock

import mount_soak as M


def config(root):
    return dict(root=str(root), weed='/w', filer='f:1', filer_path='/p', volume_host='v', volume_port=1, phases_s=[10, 10, 10],
                episodes_s=[1, 2], iptables=[], mounts=2, dual_append_s=0, write_budget_bytes=1 << 20)


class FakeFaults:
    """iptables -C answers 1 (no rule) once removed; -S lists the rule while installed."""

    def __init__(self, s_rc=0, c_rc=None, d_rc=None):
        self.calls, self.installed, self.s_rc, self.c_rc, self.d_rc = [], False, s_rc, c_rc, d_rc

    def __call__(self, argv):
        action = argv[1]
        self.calls.append(action)
        if action == '-I':
            self.installed = True
            return [0, '']
        if action == '-D':
            if self.d_rc is not None:
                return [self.d_rc, '']
            was, self.installed = self.installed, False
            return [0 if was else 1, '']
        if action == '-C':
            return [self.c_rc if self.c_rc is not None else (0 if self.installed else 1), '']
        if action == '-S':
            return [self.s_rc, '-A OUTPUT swmountsoak' if self.installed else '']
        return [0, '  0 0 DROP swmountsoak' if self.installed else '']


class Base(unittest.TestCase):
    def setUp(self):
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        self.root = Path(d.name)
        patch = mock.patch.object(M.subprocess, 'Popen')
        patch.start()
        self.addCleanup(patch.stop)
        self.run_ = M.Run(config(self.root))
        self.addCleanup(self.run_.journal.close)

    def cleanup_state(self):
        return json.loads((self.root / 'mount-cleanup.json').read_text())


class Teardown(Base):
    def test_an_exception_after_the_rule_is_installed_still_removes_it(self):
        f = FakeFaults()
        self.run_.fault_cmd = f

        def boom(ep, secs):
            f(['iptables', '-I'])
            raise RuntimeError('lookup failed')
        self.run_.run_episode = boom
        with self.assertRaises(RuntimeError):
            self.run_.episode('E1', 1)
        self.assertIn('-D', f.calls)
        self.assertFalse(f.installed)

    def test_every_copy_of_the_rule_is_removed(self):
        n = [3]

        def faults(argv):
            if argv[1] == '-D':
                if n[0]:
                    n[0] -= 1
                    return [0, '']
                return [1, '']
            return [0, '']
        self.run_.fault_cmd = faults
        self.run_.heal_rule()
        self.assertEqual(n[0], 0)

    def test_a_failed_start_unmounts_what_had_started(self):
        f = FakeFaults()
        self.run_.fault_cmd = f
        gone = []
        self.run_.mounts = dict(m0=dict(dir=self.root, proc=mock.Mock(), argv=[]))
        self.run_.unmount = lambda m: (gone.append(m), self.run_.mounts.pop(m))
        with mock.patch.object(M.Path, 'read_text', return_value=''):
            M.teardown(self.run_, self.root)
        self.assertEqual(gone, ['m0'])

    def test_cleanup_is_proven_only_by_successful_checks(self):
        f = FakeFaults()
        self.run_.fault_cmd = f
        real_read = Path.read_text
        with mock.patch.object(M.Path, 'read_text', lambda p, *a, **k: '' if str(p) == '/proc/mounts' else real_read(p, *a, **k)):
            M.teardown(self.run_, self.root)
        self.assertEqual(self.cleanup_state(), 'PROVEN')

    def test_a_failing_rule_listing_leaves_cleanup_unknown(self):
        self.run_.fault_cmd = FakeFaults(s_rc=2)
        real_read = Path.read_text
        with mock.patch.object(M.Path, 'read_text', lambda p, *a, **k: '' if str(p) == '/proc/mounts' else real_read(p, *a, **k)):
            M.teardown(self.run_, self.root)
        self.assertEqual(self.cleanup_state(), 'UNKNOWN')

    def test_a_rule_still_listed_leaves_cleanup_unknown(self):
        f = FakeFaults(d_rc=4)
        f.installed = True
        self.run_.fault_cmd = f
        real_read = Path.read_text
        with mock.patch.object(M.Path, 'read_text', lambda p, *a, **k: '' if str(p) == '/proc/mounts' else real_read(p, *a, **k)):
            M.teardown(self.run_, self.root)
        self.assertEqual(self.cleanup_state(), 'UNKNOWN')

    def test_a_mount_still_listed_leaves_cleanup_unknown(self):
        self.run_.fault_cmd = FakeFaults()
        real_read = Path.read_text
        line = 'weed %s/mnt/m0 fuse rw 0 0' % self.root
        with mock.patch.object(M.Path, 'read_text', lambda p, *a, **k: line if str(p) == '/proc/mounts' else real_read(p, *a, **k)):
            M.teardown(self.run_, self.root)
        self.assertEqual(self.cleanup_state(), 'UNKNOWN')

    def test_a_broken_command_channel_is_an_answer_not_a_crash(self):
        self.run_.helper = mock.Mock()
        self.run_.helper.stdin.write.side_effect = BrokenPipeError()
        self.assertEqual(self.run_.fault_cmd(['iptables', '-S'])[0], -1)


class StartingMount:
    """A mount process that never becomes ready; it dies on terminate unless stubborn."""

    def __init__(self, stubborn=False):
        self.alive, self.stubborn, self.signals = True, stubborn, []

    def poll(self):
        return None if self.alive else -15

    def wait(self, timeout=None):
        if self.alive:
            raise M.subprocess.TimeoutExpired('mount', timeout)
        return -15

    def terminate(self):
        self.signals.append('term')
        self.alive = self.stubborn

    def kill(self):
        self.signals.append('kill')
        self.alive = self.stubborn


class PendingStart(Base):
    def start_and_fail(self, child):
        self.run_.fault_cmd = FakeFaults()
        real_read = Path.read_text
        with mock.patch.object(M.subprocess, 'Popen', return_value=child), mock.patch.object(M.os.path, 'ismount', return_value=False), \
                mock.patch.object(M.time, 'sleep'):
            with self.assertRaisesRegex(RuntimeError, 'did not start'):
                self.run_.mount('m0')
            self.assertIn('m0', self.run_.spawned)  # owned before it is ready
            with mock.patch.object(M.Path, 'read_text', lambda p, *a, **k: '' if str(p) == '/proc/mounts' else real_read(p, *a, **k)):
                M.teardown(self.run_, self.root)

    def test_a_mount_still_starting_is_stopped_before_cleanup_is_proven(self):
        child = StartingMount()
        self.start_and_fail(child)
        self.assertFalse(child.alive)
        self.assertEqual(self.run_.spawned, {})
        self.assertEqual(self.cleanup_state(), 'PROVEN')

    def test_a_mount_that_will_not_die_leaves_cleanup_unknown(self):
        child = StartingMount(stubborn=True)
        self.start_and_fail(child)
        self.assertEqual(child.signals, ['term', 'kill'])
        self.assertIn('m0', self.run_.spawned)
        self.assertEqual(self.cleanup_state(), 'UNKNOWN')


class RuleGone(Base):
    def test_only_the_no_such_rule_answer_proves_absence(self):
        self.assertEqual(M.IPTABLES_NO_SUCH_RULE, 1)
        for rc, want in ((1, True), (2, False), (4, False), (-1, False), (0, False)):
            self.assertEqual(rc == M.IPTABLES_NO_SUCH_RULE, want, rc)


class Pacing(Base):
    def test_each_phase_gets_only_its_share_and_starts_with_no_tokens(self):
        self.run_.w1_tokens = 1e9
        self.run_.start_phase_budget(10)  # 10 of 30 load seconds
        self.assertEqual(self.run_.w1_left, (1 << 20) // 3)
        self.assertEqual(self.run_.w1_tokens, 0.0)

    def test_a_later_phase_is_not_starved_by_an_earlier_one(self):
        self.run_.start_phase_budget(10)
        self.run_.w1_left = 0
        self.run_.start_phase_budget(10)
        self.assertGreater(self.run_.w1_left, 0)

    def test_writes_wait_for_their_share_of_the_budget(self):
        self.run_.w1_rate, self.run_.w1_tokens, self.run_.w1_at = 1.0, 0.0, M.time.monotonic()
        stop = threading.Event()
        threading.Timer(0.2, stop.set).start()
        self.assertFalse(self.run_.spend(1 << 20, stop))  # not enough tokens before the phase ends

    def test_a_write_within_the_tokens_spends_them(self):
        self.run_.w1_rate, self.run_.w1_tokens, self.run_.w1_at, self.run_.w1_left = 0.0, 100.0, M.time.monotonic(), 1000
        self.assertTrue(self.run_.spend(40, threading.Event()))
        self.assertEqual((self.run_.w1_tokens, self.run_.w1_left), (60.0, 960))

    def test_the_budget_is_a_hard_cap(self):
        self.run_.w1_rate, self.run_.w1_tokens, self.run_.w1_left = 0.0, 1e9, 10
        self.assertFalse(self.run_.spend(40, threading.Event()))


if __name__ == '__main__':
    unittest.main()
