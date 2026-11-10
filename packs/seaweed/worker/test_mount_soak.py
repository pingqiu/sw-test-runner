"""Caller controls for the mount soak load: teardown owns the rule and the mounts on every path, and absence is only
ever proven by a successful check. No FUSE, iptables or network: the effects are recorded by fakes."""
import hashlib, inspect, json, os, signal, socket, stat, struct, sys, tempfile, threading, unittest
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


class TargetFaults:
    """iptables by target: one rule per (host, port). -I adds, -D removes one copy, -C reports presence, -S/-L list.
    fail_insert names a target whose -I fails; stuck names a target whose rule will not delete."""

    def __init__(self, fail_insert=None, stuck=None, packets=7):
        self.rules, self.calls, self.fail_insert, self.stuck, self.packets = [], [], fail_insert, stuck, packets

    def __call__(self, argv):
        self.calls.append(argv)
        action = argv[1]
        if action in ('-S', '-L'):
            body = '\n'.join('%d 0 DROP tcp -- 0.0.0.0/0 %s tcp dpt:%d /* swmountsoak */' % (self.packets, h, p) for h, p in self.rules)
            return [0, body]
        target = (argv[argv.index('-d') + 1], int(argv[argv.index('--dport') + 1]))
        if action == '-I':
            if target == self.fail_insert:
                return [4, '']
            self.rules.append(target)
            return [0, '']
        if action == '-D':
            if target == self.stuck:
                return [4, '']
            if target in self.rules:
                self.rules.remove(target)
                return [0, '']
            return [1, '']
        if action == '-C':
            return [0 if target in self.rules else 1, '']
        return [0, '']


class ManyVolumes(Base):
    """The partition cuts every volume server's port, and nothing is left behind on any way out."""
    VOLS = [['10.0.0.11', 8080], ['10.0.0.12', 8080], ['10.0.0.13', 8081]]

    def setUp(self):
        super().setUp()
        self.run_.c['volumes'] = self.VOLS

    def test_every_volume_server_is_cut_then_restored(self):
        f = TargetFaults()
        self.run_.fault_cmd = f
        self.assertEqual(self.run_.rules('-I'), [0, 0, 0])
        self.assertEqual(sorted(f.rules), sorted((h, p) for h, p in self.VOLS))
        self.assertEqual(self.run_.counter(), 21)
        self.assertEqual(self.run_.rules('-C'), [0, 0, 0])
        self.run_.rules('-D')
        self.assertEqual(self.run_.rules('-C'), [M.IPTABLES_NO_SUCH_RULE] * 3)

    def test_a_partial_install_fails_the_episode_and_nothing_stays(self):
        f = TargetFaults(fail_insert=('10.0.0.12', 8080))
        self.run_.fault_cmd = f
        self.run_.mounts = {m: dict(dir=self.root, proc=mock.Mock(), argv=['-volumeServerAccess=direct']) for m in ('wb', 'sync')}
        with self.assertRaisesRegex(RuntimeError, 'fault rule not applied'):
            self.run_.episode('E1', 1)
        self.assertEqual(f.rules, [])  # the rule that did go in was removed

    def test_heal_removes_every_volume_servers_rule(self):
        f = TargetFaults()
        f.rules = [(h, p) for h, p in self.VOLS] + [(self.VOLS[0][0], self.VOLS[0][1])]  # one target twice
        self.run_.fault_cmd = f
        self.run_.heal_rule()
        self.assertEqual(f.rules, [])

    def test_the_counter_needs_every_rule(self):
        f = TargetFaults()
        self.run_.fault_cmd = f
        self.run_.rules('-I')
        f.rules.pop()
        self.assertIsNone(self.run_.counter())

    def test_a_failed_listing_is_not_a_witness(self):
        f = TargetFaults()
        f.rules = [tuple(v) for v in self.VOLS]
        body = f(['iptables', '-L'])[1]
        self.run_.fault_cmd = lambda argv: [1, body]  # complete-looking rows, failed command
        self.assertIsNone(self.run_.counter())

    def test_a_duplicate_row_cannot_stand_for_a_missing_destination(self):
        f = TargetFaults()
        f.rules = [tuple(self.VOLS[0]), tuple(self.VOLS[0]), tuple(self.VOLS[1])]  # A, A, B for frozen A, B, C
        self.run_.fault_cmd = f
        self.assertIsNone(self.run_.counter())

    def test_a_foreign_or_malformed_row_is_not_a_witness(self):
        f = TargetFaults()
        f.rules = [tuple(v) for v in self.VOLS]
        good = f(['iptables', '-L'])[1]
        for extra in ('5 0 DROP tcp -- 0.0.0.0/0 10.9.9.9 tcp dpt:8080 /* swmountsoak */',  # not owned
                      'x 0 DROP tcp -- 0.0.0.0/0 10.0.0.11 tcp dpt:8080 /* swmountsoak */',  # not a count
                      '5 0 ACCEPT tcp -- 0.0.0.0/0 10.0.0.11 tcp dpt:8080 /* swmountsoak */',  # not a drop
                      'swmountsoak'):
            with self.subTest(extra=extra):
                self.run_.fault_cmd = lambda argv, body=good + '\n' + extra: [0, body]
                self.assertIsNone(self.run_.counter())

    def test_real_iptables_rows_are_read(self):
        body = '\n'.join('%8d %8d DROP       6    --  *      *       0.0.0.0/0            %-20s tcp dpt:%d /* swmountsoak */' % (n, n * 60, h, p)
                         for n, (h, p) in zip((3, 0, 4), self.VOLS))
        self.run_.fault_cmd = lambda argv: [0, 'Chain OUTPUT (policy ACCEPT 0 packets, 0 bytes)\n    pkts      bytes target     prot opt in     out     source               destination\n' + body]
        self.assertEqual(self.run_.counter(), 7)

    def test_teardown_is_unknown_while_any_rule_remains(self):
        f = TargetFaults(stuck=('10.0.0.13', 8081))
        self.run_.fault_cmd = f
        self.run_.rules('-I')
        real_read = Path.read_text
        with mock.patch.object(M.Path, 'read_text', lambda p, *a, **k: '' if str(p) == '/proc/mounts' else real_read(p, *a, **k)):
            M.teardown(self.run_, self.root)
        self.assertEqual(f.rules, [('10.0.0.13', 8081)])
        self.assertEqual(self.cleanup_state(), 'UNKNOWN')

    def test_a_single_volume_config_cuts_exactly_that_port(self):
        del self.run_.c['volumes']
        f = TargetFaults()
        self.run_.fault_cmd = f
        self.run_.rules('-I')
        self.assertEqual(f.calls, [['iptables', '-I', 'OUTPUT', '-p', 'tcp', '-d', 'v', '--dport', '1',
                                    '-m', 'comment', '--comment', 'swmountsoak', '-j', 'DROP']])

    def test_an_empty_volume_list_is_refused(self):
        self.run_.c['volumes'] = []
        with self.assertRaisesRegex(RuntimeError, 'no volume server'):
            self.run_.volumes()


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


def weed_socket(d):
    """weed mount's socket name for mount dir d, computed here independently of the worker (mount_std.go:61-65)."""
    v = struct.unpack('>i', hashlib.md5(str(d).encode()).digest()[:4])[0]
    return Path('/tmp/seaweedfs-mount-%d.sock' % abs(v))


class OwnSockets(Base):
    """mono #1176: weed mount never removes its socket. The worker removes each mount's own socket after that
    mount's process has exited, and cleanup is not PROVEN while one is left."""

    def mount_leaving(self, child, kind='socket'):
        d = self.root / 'mnt' / 'm0'
        path = weed_socket(d)
        self.addCleanup(lambda: path.unlink() if os.path.lexists(path) else None)

        def popen(*a, **k):  # the started mount creates its socket, as weed mount does
            if kind == 'socket':
                s = socket.socket(socket.AF_UNIX)
                s.bind(str(path))
                s.close()
            else:
                path.write_text('not a socket')
            return child
        self.run_.fault_cmd = FakeFaults()
        real_read = Path.read_text
        with mock.patch.object(M.subprocess, 'Popen', side_effect=popen), mock.patch.object(M.os.path, 'ismount', return_value=False), \
                mock.patch.object(M.time, 'sleep'):
            with self.assertRaisesRegex(RuntimeError, 'did not start'):
                self.run_.mount('m0')
            self.assertTrue(os.path.lexists(path))
            with mock.patch.object(M.Path, 'read_text', lambda p, *a, **k: '' if str(p) == '/proc/mounts' else real_read(p, *a, **k)):
                M.teardown(self.run_, self.root)
        return path

    def test_an_exited_mount_leaves_no_socket(self):
        path = self.mount_leaving(StartingMount())
        self.assertFalse(os.path.lexists(path))
        self.assertEqual(self.cleanup_state(), 'PROVEN')

    def test_a_live_mount_keeps_its_socket_and_cleanup_is_unknown(self):
        path = self.mount_leaving(StartingMount(stubborn=True))
        self.assertTrue(stat.S_ISSOCK(os.lstat(path).st_mode))
        self.assertEqual(self.cleanup_state(), 'UNKNOWN')

    def test_only_a_socket_is_removed(self):
        path = self.mount_leaving(StartingMount(), kind='file')
        self.assertEqual(path.read_text(), 'not a socket')
        self.assertEqual(self.cleanup_state(), 'UNKNOWN')

    def test_the_name_is_the_one_weed_mount_uses(self):
        # The 10 sockets the second lab smoke left on M01, as observed and removed by hand.
        root = Path('/opt/rdma-lab-ci/work/claude04-mountsmoke2-20261109/mnt')
        names = ['m0', 'm1', 'm2', 'm3', 'wb', 'sync', 'fresh-E1-45s', 'fresh-E2-60s', 'barrier', 'final']
        observed = {227686937, 235780097, 358778235, 621450614, 1213839488, 1214827200, 1665966658, 1672653377, 1861612155, 1914377927}
        self.assertEqual({int(str(M.mount_socket(root / n))[len('/tmp/seaweedfs-mount-'):-len('.sock')]) for n in names}, observed)


class StopAroundSpawn(unittest.TestCase):
    """A real SIGTERM through the real handler at a real source line, with a real child that only sleeps."""

    def setUp(self):
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        self.root = Path(d.name)
        weed = self.root / 'weed'
        weed.write_text('#!/bin/sh\nexec sleep 120\n')
        weed.chmod(0o755)
        with mock.patch.object(M.subprocess, 'Popen'):
            self.run_ = M.Run(dict(config(self.root), weed=str(weed)))
        self.addCleanup(self.run_.journal.close)
        self.run_.fault_cmd = FakeFaults()
        self.addCleanup(signal.signal, signal.SIGTERM, signal.signal(signal.SIGTERM, M.stop_on_term))
        self.children = []
        real = M.subprocess.Popen

        def recording(*a, **k):
            self.children.append(real(*a, **k))
            return self.children[-1]
        patch = mock.patch.object(M.subprocess, 'Popen', recording)
        patch.start()
        self.addCleanup(patch.stop)
        self.addCleanup(self.kill_children)

    def kill_children(self):
        for c in self.children:
            if c.poll() is None:
                c.kill()
                c.wait()

    def term_at(self, source):
        """Mount with a SIGTERM delivered just before the line of Run.mount holding source runs; teardown as main() would."""
        code = M.Run.mount.__code__
        target = code.co_firstlineno + next(i for i, ln in enumerate(inspect.getsource(M.Run.mount).splitlines()) if source in ln)
        sent = []

        def trace(frame, event, arg):
            if frame.f_code is not code:
                return None
            if event == 'line' and frame.f_lineno == target and not sent:
                sent.append(1)
                os.kill(os.getpid(), signal.SIGTERM)
            return trace
        sys.settrace(trace)
        try:
            with self.assertRaises(SystemExit) as stopped:
                self.run_.mount('m0')
        finally:
            sys.settrace(None)
        self.assertEqual(sent, [1])
        self.assertEqual(stopped.exception.code, 143)
        self.assertIn('m0', self.run_.spawned)  # owned when the stop lands
        M.teardown(self.run_, self.root)
        self.assertEqual(len(self.children), 1)
        self.assertEqual(self.children[0].poll(), -signal.SIGTERM)
        self.assertEqual(self.run_.spawned, {})
        self.assertEqual(json.loads((self.root / 'mount-cleanup.json').read_text()), 'PROVEN')

    def test_a_stop_between_spawn_and_registration_cannot_lose_the_child(self):
        self.term_at('self.spawned[name] = dict(dir=d, proc=p)')

    def test_a_stop_after_registration_is_honoured_at_once(self):
        self.term_at('for _ in range(300):')

    def test_a_held_stop_is_not_left_pending(self):
        self.term_at('self.spawned[name] = dict(dir=d, proc=p)')
        self.assertFalse(M.TermHold.held or M.TermHold.pending)


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
