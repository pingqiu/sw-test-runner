"""r26 F5 (plan v4 C, v5 C1/C2, v6): the ownership mode, through the worker's real work()/child() over a scripted
connector, and the load's real reap()/reap_schedule() over fake Process objects and a fake clock. Nothing here touches a
native connector or a host."""
import json
import os
import shutil
import sys
import tempfile
import threading
import unittest
from pathlib import Path

import soak_load
import soak_native_worker as w


def cmd(op, action='set'):
    return dict(operation_id=op, action=action, key='k@' + op, seed='s' + op, length=8)


def stats(set_=None, get=None):
    out = {}
    if set_ is not None or get is not None:
        out['connector_failed_ops'] = {k: v for k, v in (('set', set_), ('get', get)) if v is not None}
    return out


class Conn:
    """A scripted native connector: submit_* hands out the next fid (or raises when `raise_submit`); script[n] are the
    batches drain_completions() returns, in order, while the n-th submitted op waits; stats_json() returns the next
    object of `stats` (an Exception raises; exhausted: {}). Its event fd is always readable."""

    def __init__(self, fids, script, stats_seq=(), raise_submit=False):
        self.fids, self.script, self.n = list(fids), {k: list(v) for k, v in script.items()}, 0
        self.stats_seq, self.raise_submit = list(stats_seq), raise_submit
        self.r, wfd = os.pipe()
        os.write(wfd, b'x')
        os.close(wfd)

    def event_fd(self):
        return self.r

    def drain_completions(self):
        batches = self.script.get(self.n, [])
        return batches.pop(0) if batches else []

    def submit_batch_set(self, keys, bufs):
        if self.raise_submit:
            raise RuntimeError('submit failed after enqueue')
        self.n += 1
        return self.fids.pop(0)

    submit_batch_get = submit_batch_set

    def stats_json(self):
        s = self.stats_seq.pop(0) if self.stats_seq else {}
        if isinstance(s, Exception):
            raise s
        return json.dumps(s)


class Base(unittest.TestCase):
    def setUp(self):
        saved = w.finish.__defaults__
        w.finish.__defaults__ = (0.2,) + saved[1:]  # a 0.2 s deadline in place of 30 s (the only seam)
        self.addCleanup(setattr, w.finish, '__defaults__', saved)
        del w.UNSETTLED_BUFFERS[:]
        self.addCleanup(w.UNSETTLED_BUFFERS.clear)


class StrictCounters(Base):
    """v4 C(2): the per-op connector_failed_ops delta on the same connector, before and after one op; a reset (a
    decrease), a missing or a malformed reading leaves it ABSENT; a failed stats read is never an op failure."""

    def run_op(self, before, after, action='get', ok=False):
        conn = Conn([11], {1: [[(11, ok, 'x' if not ok else '', [False] if not ok else None)]]}, [before, after])
        return w.work(conn, cmd('1', action), None, {}, lambda r: None, own=True)

    def test_a_failed_op_counted_plus_one(self):
        for action in ('get', 'set'):
            with self.subTest(action=action):
                row = self.run_op(stats(**{('set_' if action == 'set' else 'get'): 3}), stats(**{('set_' if action == 'set' else 'get'): 4}), action)
                self.assertEqual((row['ok'], row['failed_delta'], row['pid']), (False, 1, os.getpid()))

    def test_equal_counts_zero(self):
        self.assertEqual(self.run_op(stats(get=3), stats(get=3))['failed_delta'], 0)

    def test_a_decrease_missing_or_malformed_reading_leaves_it_absent(self):
        for name, before, after in (('decrease (a reset)', stats(get=5), stats(get=1)),
                                    ('missing after', stats(get=3), {}),
                                    ('missing before', {}, stats(get=3)),
                                    ('a bool', stats(get=0), dict(connector_failed_ops=dict(get=True))),
                                    ('negative', stats(get=-1), stats(get=0)),
                                    ('not an object', stats(get=3), dict(connector_failed_ops=[4])),
                                    ('stats read raised', RuntimeError('stats down'), stats(get=4))):
            with self.subTest(case=name):
                self.assertNotIn('failed_delta', self.run_op(before, after))

    def test_the_other_operation_counter_never_counts(self):
        self.assertNotIn('failed_delta', self.run_op(stats(set_=3), stats(set_=4), action='get'))

    def test_without_ownership_a_set_reads_no_stats_and_carries_no_delta(self):
        conn = Conn([11], {1: [[(11, True, '', None)]]}, [stats(set_=1), stats(set_=2)])
        row = w.work(conn, cmd('1'), None)
        self.assertNotIn('failed_delta', row)
        self.assertEqual(len(conn.stats_seq), 2)  # untouched: every other row runs exactly as before


class Phases(Base):
    """v4 C phases: submit (no fid: enqueue cannot be excluded), wait (abandoned at the deadline), protocol."""

    def test_a_raised_submit_is_phase_submit_and_keeps_the_buffer(self):
        with self.assertRaises(w.PhaseError) as e:
            w.work(Conn([], {}, raise_submit=True), cmd('1'), None, {}, None, own=True)
        self.assertEqual(e.exception.phase, 'submit')
        self.assertEqual(len(w.UNSETTLED_BUFFERS), 1)

    def test_a_missed_deadline_is_phase_wait_abandoned_with_its_buffer(self):
        abandoned = {}
        with self.assertRaises(w.PhaseError) as e:
            w.work(Conn([11], {}), cmd('1'), None, abandoned, None, own=True)
        self.assertEqual((e.exception.phase, abandoned, len(w.UNSETTLED_BUFFERS)), ('wait', {11: '1'}, 1))

    def test_an_unowned_completion_is_phase_protocol(self):
        with self.assertRaises(w.PhaseError) as e:
            w.work(Conn([11], {1: [[(99, True, '', None)]]}), cmd('1'), None, {}, None, own=True)
        self.assertEqual(e.exception.phase, 'protocol')

    def test_without_ownership_the_old_exceptions_stand(self):
        with self.assertRaises(RuntimeError):
            w.work(Conn([], {}, raise_submit=True), cmd('1'), None)
        with self.assertRaises(TimeoutError):
            w.work(Conn([11], {}), cmd('1'), None)


STUB = '''import json, os, time
class SeaweedKVConnector:
    script = None  # set by the test: fids, batches {n: [batch, ...]}, delays {n: s after submit}, raise_submit (op counts)
    def __init__(self, *args):
        self.n, self.calls, self.failed, self.at = 0, 0, 0, time.monotonic()
        self.r, w = os.pipe()
        os.write(w, b'x')
        os.close(w)
    def event_fd(self):
        return self.r
    def drain_completions(self):
        if time.monotonic() < self.at + type(self).script.get('delays', {}).get(self.n, 0):
            return []  # not delivered yet: the op's own wait may miss it (a late completion)
        b = type(self).script['batches'].get(self.n, [])
        out = b.pop(0) if b else []
        self.failed += sum(1 for c in out if not c[1])  # the connector counts a failure before it is delivered
        return out
    def submit_batch_get(self, keys, bufs):
        self.calls += 1
        if self.calls in type(self).script.get('raise_submit', ()):
            raise RuntimeError('submit failed after enqueue')
        self.n += 1
        self.at = time.monotonic()
        return type(self).script['fids'].pop(0)
    submit_batch_set = submit_batch_get
    def stats_json(self):
        return json.dumps(dict(connector_failed_ops=dict(get=self.failed, set=self.failed)))
    def close(self):
        pass
'''


class ChildOwnership(Base):
    """The worker process's real loop, child(), in ownership mode over a stub connector module."""

    def setUp(self):
        super().setUp()
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        (self.tmp / 'seaweedkv_native').mkdir()
        (self.tmp / 'seaweedkv_native' / '__init__.py').write_text('')
        (self.tmp / 'seaweedkv_native' / '_native.py').write_text(STUB)
        sys.path.insert(0, str(self.tmp))
        self.addCleanup(sys.path.remove, str(self.tmp))
        for m in ('seaweedkv_native', 'seaweedkv_native._native'):
            self.addCleanup(sys.modules.pop, m, None)
        saved_env = dict(os.environ)
        self.addCleanup(lambda: (os.environ.clear(), os.environ.update(saved_env)))
        saved = w.STOP_DRAIN_S
        w.STOP_DRAIN_S = 0.4
        self.addCleanup(setattr, w, 'STOP_DRAIN_S', saved)

    def start(self, script, own=True):
        import multiprocessing as mp
        from seaweedkv_native._native import SeaweedKVConnector
        SeaweedKVConnector.script = script
        self.parent, end = mp.Pipe()
        cfg = dict(transport='http', filer_grpc='127.0.0.1:1', filer_http='http://127.0.0.1:1',
                   root=tempfile.mkdtemp(dir=self.tmp), native_options=dict(root='/lmcache'), stats_horizon_s=1)
        if own:
            cfg['ownership'] = True
        self.t = threading.Thread(target=w.child, args=(end, cfg), daemon=True)
        self.t.start()
        self.assertEqual(self.parent.recv()['event'], 'ready')

    def until(self, *events, timeout=5):
        got = []
        while True:
            self.assertTrue(self.parent.poll(timeout), 'no event; got %s' % got)
            e = self.parent.recv()
            got.append(e)
            if e['event'] in events:
                return got

    def stop(self):
        self.parent.send(dict(action='stop'))
        got = self.until('final')
        self.t.join(10)
        self.assertFalse(self.t.is_alive())
        return got[-1]

    def test_a_blocked_worker_refuses_ops_until_it_drains(self):
        self.start(dict(fids=[21, 22], batches={}))
        self.parent.send(dict(cmd('1'), length=8))
        err = self.until('error')[-1]
        self.assertEqual((err['phase'], err['operation_id']), ('wait', '1'))
        self.parent.send(dict(cmd('2'), length=8))
        self.assertEqual(self.until('refused')[-1]['reason'], 'blocked')  # the reuse barrier

    def test_after_the_late_drain_the_worker_unblocks_and_the_next_delta_is_its_own(self):
        self.start(dict(fids=[21, 22], batches={1: [[(21, False, 'late failure', None)]], 2: [[(22, True, '', None)]]}, delays={1: 0.5}))
        self.parent.send(dict(cmd('1'), length=8))
        self.assertEqual(self.until('error')[-1]['phase'], 'wait')
        got = self.until('unblocked')
        self.assertEqual([(e['event'], e.get('operation_id'), e.get('ok')) for e in got if e['event'] == 'late'], [('late', '1', False)])
        self.parent.send(dict(cmd('2', 'get'), length=8))
        row = self.until('complete')[-1]['row']
        self.assertEqual((row['ok'], row['failed_delta']), (True, 0))  # the late failure was counted before the barrier lifted
        self.assertEqual(self.stop()['abandoned_outstanding'], [])

    def test_a_raised_submit_poisons_the_worker_for_good(self):
        self.start(dict(fids=[21], batches={0: [[(77, False, 'hidden op', None)]]}, raise_submit={1}))
        self.parent.send(dict(cmd('1'), length=8))
        got = self.until('error')
        self.assertEqual(got[-1]['phase'], 'submit')
        self.assertNotIn('submitted', [e['event'] for e in got])  # no fid, no submission witness
        orphan = self.until('orphan')[-1]
        self.assertEqual(orphan['fid'], 77)  # a completion on a poisoned connector is evidence only
        self.parent.send(dict(cmd('2'), length=8))
        self.assertEqual(self.until('refused')[-1]['reason'], 'poisoned')
        final = self.stop()
        self.assertEqual((final['poisoned'], final['unsettled_buffers']), (True, 1))

    def test_stop_drains_then_reports_what_is_still_outstanding(self):
        self.start(dict(fids=[21], batches={}))
        self.parent.send(dict(cmd('1'), length=8))
        self.until('error')
        final = self.stop()  # no late completion within STOP_DRAIN_S
        self.assertEqual((final['abandoned_outstanding'], final['unsettled_buffers']), (['1'], 1))

    def test_a_late_completion_during_the_stop_drain_is_consumed(self):
        self.start(dict(fids=[21], batches={1: [[(21, True, '', None)]]}, delays={1: 0.3}))
        self.parent.send(dict(cmd('1'), length=8))
        self.until('error')
        self.parent.send(dict(action='stop'))
        got = self.until('final')
        self.assertIn('late', [e['event'] for e in got])
        self.assertEqual(got[-1]['abandoned_outstanding'], [])
        self.t.join(10)

    def test_an_unverified_ack_is_phase_ack_with_its_settled_row(self):
        self.start(dict(fids=[21], batches={1: [[(21, True, '', None)]]}))
        self.parent.send(dict(cmd('1'), length=8))
        err = self.until('error')[-1]  # the metadata read to 127.0.0.1:1 fails
        self.assertEqual((err['phase'], err['row']['ok'], err['row']['operation_id']), ('ack', True, '1'))

    def test_without_ownership_no_final_row_and_no_phase(self):
        self.start(dict(fids=[21], batches={}), own=False)
        self.parent.send(dict(cmd('1'), length=8))
        err = self.until('error')[-1]
        self.assertNotIn('phase', err)
        self.parent.send(dict(action='stop'))
        self.t.join(10)
        try:
            more = [self.parent.recv() for _ in range(3) if self.parent.poll(0.2)]
        except EOFError:
            more = []
        self.assertNotIn('final', [m.get('event') for m in more])  # no final row


class FakeClock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now

    def sleep(self, s):
        self.now += s


class FakeProcess:
    """exitcode stays None until `exit_at` (fake time); kill() ends it at once (exitcode -9) unless `survives_kill`."""

    def __init__(self, clock, exit_at=None, survives_kill=False):
        self.clock, self.exit_at, self.survives_kill, self.kills, self.code = clock, exit_at, survives_kill, 0, 0

    @property
    def exitcode(self):
        return self.code if self.exit_at is not None and self.clock() >= self.exit_at else None

    def kill(self):
        self.kills += 1
        if not self.survives_kill:
            self.exit_at, self.code = min(self.exit_at if self.exit_at is not None else self.clock(), self.clock()), -9


class FakePipe:
    def __init__(self):
        self.sent = []

    def send(self, m):
        self.sent.append(m)


class Reap(unittest.TestCase):
    """v6: ONE absolute deadline; an exit counts only when observed; a kill sent is no proof."""

    def reap(self, procs, kill_at, until, finals=None):
        clock = self.clock
        children = {k: dict(process=p, pipe=FakePipe()) for k, p in procs.items()}
        exits, kills, stopped = {}, {}, set()
        drained = []
        live = soak_load.reap(children, list(procs), kill_at, until, exits, kills, stopped, drained.append,
                              clock=clock, sleep=clock.sleep)
        return live, exits, kills, children

    def setUp(self):
        self.clock = FakeClock()

    def test_the_schedule_is_one_absolute_budget(self):
        self.assertEqual(soak_load.reap_schedule(0.0, 20), dict(K1=20, G=23, A=43, K2=45, D=47, E=50))
        for audit_s in (1, 20, 120):
            k = soak_load.reap_schedule(100.0, audit_s)
            self.assertEqual(k['E'] - 100.0, audit_s + 30)  # audit_s + LoadExitGrace 30, whatever audit_s
            self.assertTrue(100.0 < k['K1'] < k['G'] < k['A'] < k['K2'] < k['D'] < k['E'])

    def test_four_children_that_ignore_stop_and_kill_are_bounded_by_the_deadline_not_by_four_joins(self):
        procs = {i: FakeProcess(self.clock, survives_kill=True) for i in range(4)}
        live, exits, kills, children = self.reap(procs, 20, 47)
        self.assertEqual((sorted(live), exits), ([0, 1, 2, 3], {}))  # UNPROVEN: never assumed exited
        self.assertLess(self.clock.now, 47.1)  # one deadline, not 4 x (5 + 3)
        self.assertEqual({k: p.kills for k, p in procs.items()}, {i: 1 for i in range(4)})
        self.assertTrue(all(20 <= t for t in kills.values()))
        self.assertTrue(all(c['pipe'].sent == [dict(action='stop')] for c in children.values()))

    def test_a_final_row_is_no_exit_and_a_killed_child_counts_only_once_seen_gone(self):
        p = FakeProcess(self.clock)  # sent its final, then the destructor hangs: no exit until the kill
        live, exits, kills, _ = self.reap({0: p}, 20, 47)
        self.assertEqual((live, exits[0]['exitcode'], 0 in kills), ([], -9, True))
        self.assertGreaterEqual(exits[0]['mono'], 20)

    def test_a_kill_sent_to_a_child_that_does_not_go_is_unproven(self):
        live, exits, kills, _ = self.reap({0: FakeProcess(self.clock, survives_kill=True)}, 20, 47)
        self.assertEqual((live, exits, list(kills)), ([0], {}, [0]))

    def test_a_child_exiting_after_six_seconds_is_not_killed(self):
        p = FakeProcess(self.clock, exit_at=6.0)  # late: the legacy join(5) would have killed it
        live, exits, kills, _ = self.reap({0: p}, 20, 47)
        self.assertEqual((live, exits[0]['exitcode'], kills, p.kills), ([], 0, {}, 0))

    # ---- r26 correction (source review of c0a826ae): the exit proof is stamped at the observation
    def test_an_exit_first_seen_after_a_drain_that_crossed_the_deadline_is_no_proof(self):
        p = FakeProcess(self.clock)
        children = {0: dict(process=p, pipe=FakePipe())}

        def drain(k):  # the drain runs past `until`, and only then does the exit become observable
            self.clock.now = 2.0
            p.exit_at, p.code = 2.0, 0
        exits = {}
        live = soak_load.reap(children, [0], 20, 1.0, exits, {}, set(), drain, clock=self.clock, sleep=self.clock.sleep)
        self.assertEqual((live, exits), ([0], {}))  # UNPROVEN: never backdated to the loop's stale clock

    def test_an_exit_seen_inside_the_deadline_is_stamped_at_its_observation(self):
        p = FakeProcess(self.clock)
        children = {0: dict(process=p, pipe=FakePipe())}

        def drain(k):
            self.clock.now = 0.5
            p.exit_at, p.code = 0.5, 0
        exits = {}
        live = soak_load.reap(children, [0], 20, 1.0, exits, {}, set(), drain, clock=self.clock, sleep=self.clock.sleep)
        self.assertEqual((live, exits[0]['exitcode']), ([], 0))
        self.assertGreaterEqual(exits[0]['mono'], 0.5)  # the clock read at the observation, not before the drain

    def test_no_pipe_work_once_the_deadline_has_passed(self):
        drained = []
        p = FakeProcess(self.clock, exit_at=0.0)
        self.clock.now = 5.0  # already past `until`
        live = soak_load.reap({0: dict(process=p, pipe=FakePipe())}, [0], 20, 1.0, {}, {}, set(), drained.append,
                              clock=self.clock, sleep=self.clock.sleep)
        self.assertEqual((live, drained), ([0], []))  # no drain and no late proof

    def test_a_dead_pipe_does_not_stop_the_observation(self):
        p = FakeProcess(self.clock, exit_at=1.0)
        children = {0: dict(process=p, pipe=FakePipe())}
        def drain(k):
            raise EOFError
        exits = {}
        live = soak_load.reap(children, [0], 20, 47, exits, {}, set(), drain, clock=self.clock, sleep=self.clock.sleep)
        self.assertEqual((live, exits[0]['exitcode']), ([], 0))


if __name__ == '__main__':
    unittest.main()
