"""soak_fault.py injectors and sampler through their real CLI, with FAKE host tools on PATH (systemctl,
systemd-run, ip, ss, rdma) and a fake sysfs tree. prlimit is the real one, aimed at a dummy child this test
owns. Nothing here touches a real device, link, socket or mount; none of it has run on lab hardware."""
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import soak_fault

HERE = Path(__file__).resolve().parent
FAKE = r'''#!PYTHON
import json, os, sys
from pathlib import Path
S = Path(os.environ['FAKE_STATE']); tool = Path(sys.argv[0]).name; a = sys.argv[1:]
with (S / 'calls.jsonl').open('a') as f: f.write(json.dumps([tool] + a) + '\n')
read = lambda n: (S / n).read_text() if (S / n).exists() else ''
port = S / 'sys/class/infiniband/mlx5_0/ports/1'
if tool == 'systemctl':
    if a[:2] == ['show', '-p']: print(read('mainpid').strip() or '0')
    elif a[0] == 'stop':
        rc = int(read('stop-rc') or 0)
        if rc == 0 and read('after-stop'): (S / 'is-active').write_text(read('after-stop'))  # a pending timer, disarmed
        sys.exit(rc)
    elif a[0] == 'is-active':  # scripted "rc|stdout"; default is systemd's answer for a gone timer
        rc, _, out = (read('is-active') or '3|inactive').partition('|')
        if out: print(out)
        sys.exit(int(rc))
elif tool == 'ip':
    if a[:2] == ['-o', 'addr']: print(read('addr'), end='')
    elif a[:2] == ['route', 'show']: print(read('default'), end='')
    elif a[:2] == ['link', 'set'] and a[-1] == 'up' and (S / 'ip-up-noop').exists(): pass  # "succeeds", port stays down
    elif a[:2] == ['link', 'set']:
        down = a[-1] == 'down'
        (port / 'state').write_text('1: DOWN\n' if down else '4: ACTIVE\n')
        c = port / 'counters/link_downed'; c.write_text(str(int(c.read_text()) + down))
elif tool == 'ss':
    if '-K' in a: (S / 'ss').write_text(read('ss_after'))
    elif any('sport = :22' in x for x in a): print(read('ssh'), end='')
    else: print(read('ss'), end='')
elif tool == 'rdma': print(read('rdma_' + a[-1]) or '[]')
'''


class Host(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        self.state, self.bin, self.root = self.tmp / 'state', self.tmp / 'bin', self.tmp / 'run'
        for d in (self.state, self.bin, self.root):
            d.mkdir()
        body = FAKE.replace('PYTHON', sys.executable)
        for tool in ('systemctl', 'systemd-run', 'ip', 'ss', 'rdma'):
            (self.bin / tool).write_text(body)
            (self.bin / tool).chmod(0o755)
        port = self.state / 'sys/class/infiniband/mlx5_0/ports/1'
        (port / 'counters').mkdir(parents=True)
        (port / 'state').write_text('4: ACTIVE\n')
        (port / 'counters/link_downed').write_text('0')
        (self.state / 'sys/class/net/ens1f0/device/infiniband/mlx5_0').mkdir(parents=True)
        (self.state / 'addr').write_text('5: ens1f0    inet 198.51.100.2/24 brd 198.51.100.255 scope global ens1f0\n')
        (self.state / 'default').write_text('default via 192.0.2.1 dev eno1 proto static\n')
        (self.state / 'ssh').write_text('0 0 192.0.2.20:22 192.0.2.5:50000\n')
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ['PATH'], FAKE_STATE=str(self.state))

    def dummy(self, lock_bytes=0):
        """A child this test owns: optionally mlock()s lock_bytes, then sleeps. Its pid is the fake MainPID."""
        code = ('import ctypes, mmap, time\n'
                'n=%d\n'
                'if n:\n    b=mmap.mmap(-1,n); a=ctypes.addressof(ctypes.c_char.from_buffer(b))\n'
                '    ctypes.CDLL(None).mlock(ctypes.c_void_p(a), ctypes.c_size_t(n)) == 0 or exit(9)\n'
                'print("ok", flush=True); time.sleep(300)\n' % lock_bytes)
        p = subprocess.Popen([sys.executable, '-c', code], stdout=subprocess.PIPE, text=True)
        self.addCleanup(p.stdout.close)
        self.addCleanup(p.wait)
        self.addCleanup(p.kill)
        self.assertEqual(p.stdout.readline().strip(), 'ok')
        (self.state / 'mainpid').write_text(str(p.pid))
        return p

    def config(self, kind, **kw):
        return dict(dict(root=str(self.root), run_id='run-1', episode='E1', kind=kind, units=['sw-cloud-run-1-deploy-m02-volume'],
                         python=sys.executable, script=str(HERE / 'soak_fault.py'), deadman_s=40, sys_root=str(self.state / 'sys'),
                         netdev='ens1f0', rdma_device='mlx5_0', rdma_port=1, management_addrs=['192.0.2.20'],
                         setup_ports=[18888], memlock_bytes=1 << 20, nofile_extra=4, free_below_bytes=1 << 20,
                         max_ballast_bytes=1 << 30, sample_s=0.2, quiescence_s=0.6, max_s=8), **kw)

    def cli(self, c, action, expect=0):
        out = subprocess.run([sys.executable, '-I', str(HERE / 'soak_fault.py'), '--config', json.dumps(c), '--action', action],
                             capture_output=True, text=True, timeout=60, env=self.env)
        self.assertEqual(out.returncode, expect, out.stderr + out.stdout)
        return json.loads(out.stdout) if out.stdout.strip() else None

    def calls(self, tool=None):
        p = self.state / 'calls.jsonl'
        rows = [json.loads(line) for line in p.read_text().splitlines()] if p.exists() else []
        return [r for r in rows if tool is None or r[0] == tool]

    def limits(self, pid):
        return soak_fault.limits(pid)


class LinkFlap(Host):
    def test_deadman_is_armed_before_the_flap_and_its_own_command_restores(self):
        self.dummy()
        c = self.config('link_flap')
        row = self.cli(c, 'apply')
        calls = [r[0] + ' ' + ' '.join(r[1:3]) for r in self.calls() if r[0] in ('systemd-run', 'ip') and r[1] in ('--unit=sw-fault-run-1-E1', 'link')]
        self.assertEqual(calls[0].split()[0], 'systemd-run')  # armed first
        self.assertEqual(row['after']['port'], ['1: DOWN', 1])
        self.assertEqual(row['deadman'], dict(unit='sw-fault-run-1-E1', on_active_s=40))
        timer = self.calls('systemd-run')[0]
        self.assertIn('--on-active=40s', timer)
        self.assertEqual(timer[-2:], ['--action', 'restore'])
        # what the timer would run, as it would run it: the port comes back, the restore is proven
        cmd = timer[timer.index('--timer-property=AccuracySec=1s') + 1:]
        done = subprocess.run(cmd, capture_output=True, text=True, env=self.env, timeout=60)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertTrue(json.loads(done.stdout)['restored'])
        self.assertEqual((self.state / 'sys/class/infiniband/mlx5_0/ports/1/state').read_text().strip(), '4: ACTIVE')
        self.assertIn(['systemctl', 'stop', 'sw-fault-run-1-E1.timer'], self.calls('systemctl'))
        self.assertTrue(self.cli(c, 'restore')['restored'])  # idempotent

    def test_a_dead_man_that_cannot_be_cancelled_is_not_a_proven_restore(self):
        self.dummy()
        c = self.config('link_flap')
        self.cli(c, 'apply')
        (self.state / 'stop-rc').write_text('1')  # the timer stays armed: a later firing could flap the link again
        (self.state / 'is-active').write_text('0|active')
        row = self.cli(c, 'restore', expect=4)
        self.assertEqual((row['restored'], row['proof']['deadman_timer'], row['proof']['deadman_stop_rc']), (False, [0, 'active'], 1))

    def stops(self):
        return [r for r in self.calls('systemctl') if r[1] == 'stop']

    def pending_timer(self):
        (self.state / 'is-active').write_text('0|active')
        (self.state / 'after-stop').write_text('3|inactive')

    def test_an_unproven_restore_leaves_the_dead_man_armed_then_a_proven_one_stops_it(self):
        p = self.dummy()
        before = self.limits(p.pid)['nofile']
        c = self.config('fd_exhaust')
        self.cli(c, 'apply')
        self.pending_timer()
        real = shutil.which('prlimit')
        (self.bin / 'prlimit').write_text('#!/bin/sh\nexit 0\n')  # reports success, changes nothing: the proof read fails
        (self.bin / 'prlimit').chmod(0o755)
        row = self.cli(c, 'restore', expect=4)
        self.assertEqual((row['restored'], row['proof']['deadman'], row['proof']['deadman_before']), (False, 'left_armed', 'pending'))
        self.assertEqual(self.stops(), [])  # the backstop was not cancelled
        (self.bin / 'prlimit').unlink()
        self.assertTrue(real)
        row = self.cli(c, 'restore')  # now the limits read back restored: only then is the dead-man stopped
        self.assertEqual((row['restored'], row['proof']['deadman_before'], row['proof']['deadman_timer']), (True, 'pending', [3, 'inactive']))
        self.assertEqual(len(self.stops()), 1)
        self.assertEqual(self.limits(p.pid)['nofile'], before)

    def test_an_unproven_link_restore_leaves_the_dead_man_armed(self):
        self.dummy()
        c = self.config('link_flap', restore_wait_s=1)
        self.cli(c, 'apply')
        self.pending_timer()
        (self.state / 'ip-up-noop').touch()
        row = self.cli(c, 'restore', expect=4)
        self.assertEqual((row['restored'], row['proof']['deadman']), (False, 'left_armed'))
        self.assertEqual(self.stops(), [])

    def test_a_timer_that_already_fired_or_is_unavailable_is_recorded(self):
        for answer, state, proven in (('3|inactive', 'fired_or_gone', True), ('1|', 'unavailable', False)):
            with self.subTest(answer=answer):
                self.setUp()
                self.dummy()
                c = self.config('fd_exhaust')
                self.cli(c, 'apply')
                (self.state / 'is-active').write_text(answer)
                row = self.cli(c, 'restore', expect=0 if proven else 4)
                self.assertEqual((row['proof']['deadman_before'], row['restored']), (state, proven))

    def test_only_the_documented_timer_answers_prove_the_restore(self):
        """systemctl is-active (rc, stdout) and the stop's rc, through the CLI: only rc 3 with inactive or failed,
        after a stop that succeeded or found the unit not loaded, proves the dead-man is gone."""
        cases = [('3|inactive', '0', True), ('3|failed', '0', True), ('3|inactive', '5', True),
                 ('4|inactive', '0', True), ('1|inactive', '0', False), ('0|inactive', '0', False), ('4|', '0', False), ('4|unknown', '0', False), ('3|', '0', False),
                 ('1|', '0', False), ('0|active', '0', False), ('3|activating', '0', False), ('3|inactive', '1', False)]
        for answer, stop_rc, proven in cases:
            with self.subTest(answer=answer, stop_rc=stop_rc):
                self.setUp()
                p = self.dummy()
                before = self.limits(p.pid)['nofile']
                c = self.config('fd_exhaust')
                self.cli(c, 'apply')
                (self.state / 'is-active').write_text(answer)
                (self.state / 'stop-rc').write_text(stop_rc)
                row = self.cli(c, 'restore', expect=0 if proven else 4)
                rc, _, out = answer.partition('|')
                self.assertEqual((row['restored'], row['proof']['deadman_timer'], row['proof']['deadman_stop_rc']),
                                 (proven, [int(rc), out], int(stop_rc)))
                self.assertEqual(self.limits(p.pid)['nofile'], before)  # the limit itself was put back either way

    def test_preflight_refuses_and_changes_nothing(self):
        self.dummy()
        cases = {
            'management address': lambda: (self.state / 'addr').write_text('5: ens1f0  inet 192.0.2.20/24 scope global\n'),
            'default route': lambda: (self.state / 'default').write_text('default via 198.51.100.1 dev ens1f0\n'),
            'SSH route': lambda: (self.state / 'ssh').write_text('0 0 198.51.100.2:22 198.51.100.9:40000\n'),
            'not the row RDMA device': lambda: shutil.rmtree(self.state / 'sys/class/net/ens1f0/device/infiniband/mlx5_0'),
        }
        for why, break_it in cases.items():
            with self.subTest(why=why):
                self.setUp()
                self.dummy()
                break_it()
                row = self.cli(self.config('link_flap'), 'apply', expect=3)
                self.assertIn(why.split()[0], row['refused'])
                self.assertFalse(self.calls('systemd-run'))
                self.assertFalse([r for r in self.calls('ip') if r[1] == 'link'])


class Limits(Host):
    def test_mr_pressure_lowers_memlock_below_use_and_restores_it(self):
        p = self.dummy(lock_bytes=4 << 20)
        before = self.limits(p.pid)['memlock']
        c = self.config('mr_pressure')
        row = self.cli(c, 'apply')
        self.assertEqual(self.limits(p.pid)['memlock'], [1 << 20, before[1]])
        self.assertEqual(row['after']['limits']['memlock'][0], 1 << 20)
        self.assertTrue(self.calls('systemd-run'))
        self.assertTrue(self.cli(c, 'restore')['restored'])
        self.assertEqual(self.limits(p.pid)['memlock'], before)

    def test_mr_pressure_refuses_a_target_not_below_current_use(self):
        self.dummy(lock_bytes=0)
        row = self.cli(self.config('mr_pressure'), 'apply', expect=3)
        self.assertIn('not below current use', row['refused'])
        self.assertFalse(self.calls('systemd-run'))

    def test_fd_exhaust_sets_current_plus_small_and_restores(self):
        p = self.dummy()
        before = self.limits(p.pid)['nofile']
        c = self.config('fd_exhaust')
        row = self.cli(c, 'apply')
        self.assertEqual(self.limits(p.pid)['nofile'][0], row['before']['fds'] + 4)
        self.assertTrue(self.cli(c, 'restore')['restored'])
        self.assertEqual(self.limits(p.pid)['nofile'], before)

    def test_a_restore_command_that_did_nothing_is_not_proven(self):
        p = self.dummy()
        c = self.config('fd_exhaust')
        self.cli(c, 'apply')
        (self.bin / 'prlimit').write_text('#!/bin/sh\nexit 0\n')  # reports success, changes nothing
        (self.bin / 'prlimit').chmod(0o755)
        row = self.cli(c, 'restore', expect=4)
        self.assertFalse(row['restored'])
        self.assertNotEqual(self.limits(p.pid)['nofile'][0], row['after']['limits']['nofile'][1] + 1)

    def test_restore_on_a_changed_target_is_not_proven(self):
        p = self.dummy()
        c = self.config('fd_exhaust')
        self.cli(c, 'apply')
        p.kill()
        p.wait()
        self.dummy()  # the unit's MainPID is now another process
        row = self.cli(c, 'restore', expect=4)
        self.assertFalse(row['restored'])


class ConnKill(Host):
    def test_kills_the_setup_connections_and_witnesses_gone_and_new(self):
        p = self.dummy()
        line = '0 0 198.51.100.2:18888 198.51.100.1:%d users:(("weed-volume",pid=%d,fd=12))\n'
        (self.state / 'ss').write_text(line % (5000, p.pid) + '0 0 198.51.100.2:8080 198.51.100.1:1 users:(("x",pid=%d,fd=3))\n' % p.pid)
        (self.state / 'ss_after').write_text('')
        c = self.config('conn_kill')
        row = self.cli(c, 'apply')
        self.assertEqual(row['before']['conns'], ['198.51.100.2:18888>198.51.100.1:5000'])  # only the setup port
        self.assertEqual(row['after']['conns'], [])
        self.assertIn('sport = :18888', ' '.join(next(r for r in self.calls('ss') if '-K' in r)))
        (self.state / 'ss').write_text(line % (5001, p.pid))
        self.assertEqual(self.cli(c, 'restore')['after']['conns'], ['198.51.100.2:18888>198.51.100.1:5001'])
        self.assertFalse(self.calls('systemd-run'))  # nothing persists, so no dead-man

    def test_refuses_with_no_setup_connection(self):
        self.dummy()
        self.assertIn('no setup connection', self.cli(self.config('conn_kill'), 'apply', expect=3)['refused'])


class DiskFull(Host):
    def test_refuses_outside_its_own_loop_mount(self):
        self.dummy()
        mount = self.root / 'disk'
        mount.mkdir()
        info = self.tmp / 'mountinfo'
        for source, where, why in (('/dev/sda1', mount, 'not a loop'), ('/dev/loop3', self.tmp, 'outside the run root')):
            with self.subTest(why=why):
                info.write_text('99 1 7:3 / %s rw - ext4 %s rw\n' % (where, source))
                row = self.cli(self.config('disk_full', mount=str(where), mountinfo=str(info)), 'apply', expect=3)
                self.assertIn(why, row['refused'])

    def test_fills_to_below_the_threshold_and_restores(self):
        """In-process with a simulated statfs: a real fallocate of the host's free space is never done here."""
        self.dummy()
        mount = self.root / 'disk'
        mount.mkdir()
        info = self.tmp / 'mountinfo'
        info.write_text('99 1 7:3 / %s rw - ext4 /dev/loop3 rw\n' % mount.resolve())
        c = self.config('disk_full', mount=str(mount), mountinfo=str(info))
        free = {'v': 64 << 20}
        sized = []
        saved_env, saved = dict(os.environ), (soak_fault.statfs_free, os.posix_fallocate)
        os.environ.update(self.env)
        def fallocate(fd, offset, length):
            sized.append(length)
            free['v'] -= length
        soak_fault.statfs_free, os.posix_fallocate = (lambda m: free['v']), fallocate
        try:
            row = soak_fault.apply(c)
            self.assertLess(row['after']['free'], c['free_below_bytes'])
            self.assertEqual(sized, [(64 << 20) - (1 << 19)])
            self.assertTrue(soak_fault.ballast(c).exists())
            # statfs still reads below the threshold after the unlink: unproven, so the dead-man is not stopped
            (self.state / 'is-active').write_text('0|active')
            row = soak_fault.restore(c)
            self.assertEqual((row['restored'], row['proof'].get('deadman')), (False, 'left_armed'))
            self.assertFalse([r for r in self.calls('systemctl') if r[1] == 'stop'])
            (self.state / 'is-active').write_text('3|inactive')
            free['v'] = 64 << 20
            self.assertTrue(soak_fault.restore(c)['restored'])
            self.assertFalse(soak_fault.ballast(c).exists())
        finally:
            soak_fault.statfs_free, os.posix_fallocate = saved
            os.environ.clear()
            os.environ.update(saved_env)


class Control(Host):
    def test_r0_changes_nothing_and_arms_nothing(self):
        p = self.dummy()
        before = self.limits(p.pid)
        c = self.config('control')
        row = self.cli(c, 'apply')
        self.assertEqual(row['before']['limits'], row['after']['limits'])
        self.assertTrue(self.cli(c, 'restore')['restored'])
        self.assertEqual(self.limits(p.pid), before)
        self.assertFalse(self.calls('systemd-run') + [r for r in self.calls('ip') if r[1] == 'link'])


class Sampler(Host):
    def test_samples_by_pid_through_the_phases(self):
        p = self.dummy()
        rows = [dict(pid=p.pid, lqpn=1), dict(pid=p.pid, lqpn=2), dict(pid=1, lqpn=3)]
        (self.state / 'rdma_qp').write_text(json.dumps(rows))
        (self.state / 'rdma_mr').write_text(json.dumps([dict(pid=p.pid, mrlen=4096), dict(pid=7, mrlen=1)]))

        class Metrics(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                body = b'# HELP x\nSeaweedFS_rdma_connections_released_total 3\nSeaweedFS_pinned{a="1"} 5\nSeaweedFS_pinned{a="2"} 7\n'
                self.send_response(200)
                self.end_headers()
                self.wfile.write(body)
        server = ThreadingHTTPServer(('127.0.0.1', 0), Metrics)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        c = self.config('control', metrics_url='http://127.0.0.1:%d/metrics' % server.server_address[1],
                        metrics=dict(released='SeaweedFS_rdma_connections_released_total', pinned='SeaweedFS_pinned',
                                     missing='SeaweedFS_absent'), watch=dict(master='sw-cloud-run-1-deploy-m02-master'))
        def marks():
            time.sleep(0.5)
            self.cli(c, 'mark-load')
            time.sleep(0.5)
            self.cli(c, 'mark-quiescence')
        t = threading.Thread(target=marks)
        t.start()
        subprocess.run([sys.executable, '-I', str(HERE / 'soak_fault.py'), '--config', json.dumps(c), '--action', 'sample'],
                       env=self.env, timeout=30, check=True)
        t.join()
        out = [json.loads(line) for line in (self.root / 'resource-samples.jsonl').read_text().splitlines()]
        self.assertEqual([x for i, x in enumerate([s['phase'] for s in out]) if i == 0 or out[i - 1]['phase'] != x],
                         ['pre_load', 'load', 'quiescence'])
        s = out[-1]
        self.assertEqual(s['identity']['pid'], p.pid)
        self.assertEqual(s['rdma'], dict(qp=2, mr=1, mr_lens=[4096]))
        self.assertEqual(s['metrics'], dict(released=3.0, pinned=12.0))  # an absent series is absent, not 0
        self.assertEqual(s['port'], ['4: ACTIVE', 0])
        self.assertGreater(s['fds'], 0)
        self.assertEqual(s['data_fds'], 0)  # the dummy holds no volume file
        self.assertGreaterEqual(out[-1]['mono'] - next(x['mono'] for x in out if x['phase'] == 'quiescence'), 0.6 - 0.25)

    def test_rows_without_a_pid_make_the_view_unread_not_zero(self):
        """Through the real sampler CLI: a QP or MR row with no pid cannot be joined, so rdma is null."""
        p = self.dummy()
        for qp, mr in (([dict(lqpn=4)], []), ([dict(pid=p.pid, lqpn=1)], [dict(mrlen=4096)]), ([dict(pid='7', lqpn=2)], [])):
            with self.subTest(qp=qp, mr=mr):
                (self.state / 'rdma_qp').write_text(json.dumps(qp))
                (self.state / 'rdma_mr').write_text(json.dumps(mr))
                out = self.root / 'resource-samples.jsonl'
                if out.exists():
                    out.unlink()
                c = self.config('control', max_s=0.5)
                subprocess.run([sys.executable, '-I', str(HERE / 'soak_fault.py'), '--config', json.dumps(c), '--action', 'sample'],
                               env=self.env, timeout=30, check=True)
                rows = [json.loads(line) for line in out.read_text().splitlines()]
                self.assertTrue(rows and all(r['rdma'] is None for r in rows), rows[:1])

    def test_f1_and_f2_rows_sample_and_mark_but_never_inject(self):
        """The runner pins the sampler and markers with the row's own kind; F1/F2 must sample, never apply."""
        self.dummy()
        for kind in ('volume_kill_io', 'client_kill_put'):
            with self.subTest(kind=kind):
                c = self.config(kind, root=str(self.tmp / kind), max_s=0.5)
                Path(c['root']).mkdir()
                self.cli(c, 'mark-load')
                subprocess.run([sys.executable, '-I', str(HERE / 'soak_fault.py'), '--config', json.dumps(c), '--action', 'sample'],
                               env=self.env, timeout=30, check=True)
                self.assertTrue((Path(c['root']) / 'resource-samples.jsonl').read_text())
                for action in ('apply', 'restore'):
                    out = subprocess.run([sys.executable, '-I', str(HERE / 'soak_fault.py'), '--config', json.dumps(c), '--action', action],
                                         capture_output=True, text=True, env=self.env, timeout=30)
                    self.assertNotEqual(out.returncode, 0)
                    self.assertIn('has no ' + action, out.stderr)
        self.assertFalse(self.calls('systemd-run'))

    def test_fds_held_by_volume_files_are_counted_apart(self):
        """The rehearsal's finding: volumes created during a load hold .dat/.idx/.vif fds; they are counted apart
        (data_fds) so a reader can tell them from any other fd. The verdict rule itself is unchanged."""
        files = []
        for name, body in (('7.dat', b'x' * 5000), ('7.idx', b''), ('7.vif', b''), ('notes.txt', b'')):
            (self.tmp / name).write_bytes(body)
            files.append((self.tmp / name).open('rb'))
        self.addCleanup(lambda: [f.close() for f in files])
        total, dats, data, sockets = soak_fault.fd_view(os.getpid())
        self.assertEqual((data, 5000 in dats), (['7.dat', '7.idx', '7.vif'], True))
        self.assertGreaterEqual(total, 4)

    def test_socket_view_joins_each_peer_to_its_local_owner(self):
        p = self.dummy()
        me = os.getpid()
        (self.state / 'ss').write_text(
            'LISTEN 0 128 127.0.0.1:8080 0.0.0.0:* users:(("weed-volume",pid=%d,fd=5))\n' % p.pid
            + 'ESTAB 0 0 127.0.0.1:8080 127.0.0.1:41000 users:(("weed-volume",pid=%d,fd=9))\n' % p.pid
            + 'ESTAB 0 0 127.0.0.1:41000 127.0.0.1:8080 users:(("weed",pid=%d,fd=3))\n' % me
            + 'ESTAB 0 0 127.0.0.1:8080 198.51.100.9:40000 users:(("weed-volume",pid=%d,fd=10))\n' % p.pid
            + 'TIME-WAIT 0 0 127.0.0.1:8080 127.0.0.1:41999\n')
        saved = dict(os.environ)
        os.environ.update(self.env)
        try:
            rows, owners = soak_fault.socket_view(p.pid)
        finally:
            os.environ.clear()
            os.environ.update(saved)
        self.assertEqual([r['state'] for r in rows], ['ESTAB', 'ESTAB', 'LISTEN'])
        ident = soak_fault.identity(me)
        self.assertEqual(owners, {'127.0.0.1:41000': dict(pid=me, starttime=ident['starttime'])})  # the remote peer has no local owner
        self.assertIsNone(soak_fault.socket_view(p.pid, run=lambda *a, **k: subprocess.CompletedProcess(a, 1, '', '')))

    def test_the_volume_list_comes_from_the_servers_status(self):
        answers = {'/good': dict(Volumes=[dict(Id=4, Collection='lmcache'), dict(Id=3, Collection='lmcache'), dict(Id=7, Collection='')]),
                   '/bad-id': dict(Volumes=[dict(Id='4', Collection='lmcache')]), '/bool-id': dict(Volumes=[dict(Id=True, Collection='lmcache')]),
                   '/float-id': dict(Volumes=[dict(Id=4.0, Collection='lmcache')]), '/no-list': dict(Disks=[])}

        class Status(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                body = json.dumps(answers.get(self.path, {})).encode()
                self.send_response(200 if self.path in answers else 500)
                self.end_headers()
                self.wfile.write(body)
        server = ThreadingHTTPServer(('127.0.0.1', 0), Status)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        url = 'http://127.0.0.1:%d' % server.server_address[1]
        self.assertEqual(soak_fault.volume_list(url + '/good'), ['7', 'lmcache_3', 'lmcache_4'])
        for path in ('/bad-id', '/bool-id', '/float-id', '/no-list', '/missing'):
            self.assertIsNone(soak_fault.volume_list(url + path), path)
        self.assertIsNone(soak_fault.volume_list('http://127.0.0.1:1/status'))

    def test_an_unreadable_view_is_null_never_zero(self):
        self.dummy()
        (self.state / 'rdma_qp').write_text('not json')
        self.assertIsNone(soak_fault.rdma_view(os.getpid(), run=lambda *a, **k: subprocess.CompletedProcess(a, 1, '', '')))
        self.assertIsNone(soak_fault.limits(2 ** 22 + 7))
        self.assertIsNone(soak_fault.fd_view(2 ** 22 + 7))


if __name__ == '__main__':
    unittest.main()
