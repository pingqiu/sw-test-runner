"""r12 (b), F3 only: a native completion of a future the worker abandoned at its deadline, through the worker's real
work()/finish() with a scripted connector. Each abandoned fid is consumed once, reported and skipped; any other
foreign fid, or the same fid again, stays 'unowned completion'; without the flag (every other row) nothing changes."""
import os
import unittest

import soak_native_worker as w


class Conn:
    """A scripted native connector: submit_* hands out the next fid; script[n] are the batches drain_completions()
    returns, in order, while the n-th submitted op waits (then empty). Its event fd is always readable."""

    def __init__(self, fids, script):
        self.fids, self.script, self.n = list(fids), {k: list(v) for k, v in script.items()}, 0
        self.r, wfd = os.pipe()
        os.write(wfd, b'x')
        os.close(wfd)

    def event_fd(self):
        return self.r

    def drain_completions(self):
        batches = self.script.get(self.n, [])
        return batches.pop(0) if batches else []

    def submit_batch_set(self, keys, bufs):
        self.n += 1
        return self.fids.pop(0)

    submit_batch_get = submit_batch_set

    def stats_json(self):
        return '{}'


def cmd(op, action='set'):
    return dict(operation_id=op, action=action, key='k@' + op, seed='s' + op, length=8)


class LateCompletion(unittest.TestCase):
    def setUp(self):
        saved = w.finish.__defaults__
        w.finish.__defaults__ = (0.2,) + saved[1:]  # a 0.2 s deadline in place of 30 s (the only seam)
        self.addCleanup(setattr, w.finish, '__defaults__', saved)
        self.late = []

    def deadline(self, conn, abandoned, op='1'):
        with self.assertRaisesRegex(TimeoutError, 'native completion deadline'):
            w.work(conn, cmd(op), None, abandoned, self.late.append)

    def test_a_late_completion_of_the_abandoned_fid_is_recorded_and_skipped(self):
        abandoned = {}
        conn = Conn([11, 12], {2: [[(11, True, '', [True])], [(12, True, '', None)]]})
        self.deadline(conn, abandoned)
        self.assertEqual(abandoned, {11: '1'})
        row = w.work(conn, cmd('2'), None, abandoned, self.late.append)
        self.assertEqual((row['operation_id'], row['ok']), ('2', True))  # its own completion, not fid 11's
        self.assertEqual([(x['operation_id'], x['fid'], x['ok'], x['error'], x['results']) for x in self.late],
                         [('1', 11, True, '', [True])])
        self.assertIsInstance(self.late[0]['mono'], float)
        self.assertEqual(abandoned, {})  # consumed

    def test_a_foreign_fid_is_still_unowned(self):
        abandoned = {}
        conn = Conn([11, 12], {2: [[(99, True, '', None)]]})
        self.deadline(conn, abandoned)
        with self.assertRaisesRegex(RuntimeError, 'unowned completion'):
            w.work(conn, cmd('2'), None, abandoned, self.late.append)
        self.assertEqual(self.late, [])

    def test_the_same_abandoned_fid_twice_is_unowned_the_second_time(self):
        abandoned = {}
        conn = Conn([11, 12], {2: [[(11, False, 'reset', None)], [(11, False, 'reset', None)]]})
        self.deadline(conn, abandoned)
        with self.assertRaisesRegex(RuntimeError, 'unowned completion'):
            w.work(conn, cmd('2'), None, abandoned, self.late.append)
        self.assertEqual([x['fid'] for x in self.late], [11])

    def test_without_the_flag_nothing_is_tracked_and_a_late_completion_is_unowned(self):
        conn = Conn([11, 12], {2: [[(11, True, '', None)]]})
        with self.assertRaisesRegex(TimeoutError, 'native completion deadline'):
            w.work(conn, cmd('1'))  # every other row's call: no abandoned map, no late callback
        with self.assertRaisesRegex(RuntimeError, 'unowned completion'):
            w.work(conn, cmd('2'))

    def test_only_a_deadline_abandons_a_future(self):
        abandoned = {}

        class Boom(Conn):
            def drain_completions(self):
                raise OSError('connector closed')
        with self.assertRaisesRegex(OSError, 'connector closed'):
            w.work(Boom([11], {}), cmd('1'), None, abandoned, self.late.append)
        self.assertEqual(abandoned, {})


class BatchOrder(unittest.TestCase):
    """r13 (codex01 on r12): with the F3 flag, finish() processes each drained batch WHOLE, in any order. Fid 11 is
    abandoned by op 1's deadline; op 2 waits on fid 12; 99 is foreign."""
    CUR, ABN, FOR = (12, True, '', [True]), (11, True, '', None), (99, True, '', None)

    def setUp(self):
        saved = w.finish.__defaults__
        w.finish.__defaults__ = (0.2,) + saved[1:]
        self.addCleanup(setattr, w.finish, '__defaults__', saved)
        self.late = []

    def op2(self, *drains):
        abandoned = {}
        conn = Conn([11, 12], {2: [list(d) for d in drains]})
        with self.assertRaisesRegex(TimeoutError, 'native completion deadline'):
            w.work(conn, cmd('1'), None, abandoned, self.late.append)
        self.assertEqual(abandoned, {11: '1'})
        return w.work(conn, cmd('2'), None, abandoned, self.late.append), abandoned

    def fatal(self, why, *drains):
        with self.assertRaisesRegex(RuntimeError, why):
            self.op2(*drains)

    def returned_and_recorded(self, *drains):
        row, abandoned = self.op2(*drains)
        self.assertEqual((row['operation_id'], row['ok'], row['results']), ('2', True, [True]))
        self.assertEqual([(x['operation_id'], x['fid']) for x in self.late], [('1', 11)])  # recorded exactly once
        self.assertEqual(abandoned, {})

    def test_current_then_foreign_is_fatal(self):
        self.fatal('unowned completion', [self.CUR, self.FOR])

    def test_foreign_then_current_is_fatal(self):
        self.fatal('unowned completion', [self.FOR, self.CUR])

    def test_current_then_abandoned_returns_current_and_records_the_late_one(self):
        self.returned_and_recorded([self.CUR, self.ABN])

    def test_abandoned_then_current_returns_current_and_records_the_late_one(self):
        self.returned_and_recorded([self.ABN, self.CUR])

    def test_current_twice_is_fatal(self):
        self.fatal('duplicate completion', [self.CUR, self.CUR])

    def test_the_same_abandoned_fid_twice_in_one_batch_is_fatal(self):
        self.fatal('unowned completion', [self.ABN, self.ABN, self.CUR])

    def test_a_batch_split_across_two_drains(self):
        self.returned_and_recorded([self.ABN], [self.CUR])

    def test_a_late_one_arriving_with_the_next_ops_completion(self):
        # op 2 returns on its own fid; fid 11 arrives in op 3's batch, after op 3's own completion
        abandoned = {}
        conn = Conn([11, 12, 13], {2: [[self.CUR]], 3: [[(13, True, '', [True]), self.ABN]]})
        with self.assertRaisesRegex(TimeoutError, 'native completion deadline'):
            w.work(conn, cmd('1'), None, abandoned, self.late.append)
        w.work(conn, cmd('2'), None, abandoned, self.late.append)
        self.assertEqual(self.late, [])
        row = w.work(conn, cmd('3'), None, abandoned, self.late.append)
        self.assertEqual(row['operation_id'], '3')
        self.assertEqual([x['fid'] for x in self.late], [11])

    def test_a_current_fid_completing_again_in_a_later_op_is_fatal(self):
        abandoned = {}
        conn = Conn([12, 13], {1: [[self.CUR]], 2: [[(13, True, '', None), self.CUR]]})
        w.work(conn, cmd('1'), None, abandoned, self.late.append)
        with self.assertRaisesRegex(RuntimeError, 'unowned completion'):
            w.work(conn, cmd('2'), None, abandoned, self.late.append)


NOFLAG = ('no-flag path (R0/F1/F2/F4 and the soak) finding for claude01/codex01, deliberately not changed in r13: '
          'finish() returns on the current fid and silently drops a foreign completion later in the same batch; '
          'run with SOAK_NOFLAG_FINDING=1 to see it fail')


@unittest.skipUnless(os.environ.get('SOAK_NOFLAG_FINDING') == '1', NOFLAG)
class NoFlagFinding(unittest.TestCase):
    def test_no_flag_current_then_foreign_should_be_fatal(self):
        conn = Conn([12], {1: [[(12, True, '', [True]), (99, True, '', None)]]})
        saved = w.finish.__defaults__
        w.finish.__defaults__ = (0.2,) + saved[1:]
        self.addCleanup(setattr, w.finish, '__defaults__', saved)
        with self.assertRaisesRegex(RuntimeError, 'unowned completion'):
            w.work(conn, cmd('1'))


STUB = '''import os
class SeaweedKVConnector:
    script = None  # set by the test: dict(fids=[...], batches={n: [batch, ...]})
    def __init__(self, *args):
        self.n = 0
        self.r, w = os.pipe()
        os.write(w, b'x')
        os.close(w)
    def event_fd(self):
        return self.r
    def drain_completions(self):
        b = type(self).script['batches'].get(self.n, [])
        return b.pop(0) if b else []
    def submit_batch_get(self, keys, bufs):
        self.n += 1
        return type(self).script['fids'].pop(0)
    submit_batch_set = submit_batch_get
    def stats_json(self):
        return '{}'
    def close(self):
        pass
'''


class ChildLoop(unittest.TestCase):
    """The worker process's real loop, child(), over a stub connector module: the flag the load passes only to F3
    workers is what turns tracking on; each child() call (one per worker process) starts with an empty map."""

    def setUp(self):
        import shutil
        import sys
        import tempfile
        from pathlib import Path
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
        saved = w.finish.__defaults__
        w.finish.__defaults__ = (0.2,) + saved[1:]
        self.addCleanup(setattr, w.finish, '__defaults__', saved)

    def run_child(self, flag):
        import multiprocessing as mp
        import threading
        from seaweedkv_native._native import SeaweedKVConnector
        SeaweedKVConnector.script = dict(fids=[21, 22], batches={2: [[(21, True, '', [True])], [(22, True, '', [True])]]})
        parent, end = mp.Pipe()
        import tempfile
        cfg = dict(transport='http', filer_grpc='127.0.0.1:1', filer_http='http://127.0.0.1:1', root=tempfile.mkdtemp(dir=self.tmp),
                   native_options=dict(root='/lmcache'), stats_horizon_s=1)
        if flag is not None:
            cfg['late_completions'] = flag
        t = threading.Thread(target=w.child, args=(end, cfg), daemon=True)
        t.start()
        self.assertEqual(parent.recv()['event'], 'ready')
        events = []
        for op in ('1', '2'):
            parent.send(dict(cmd(op, 'get'), length=8))
            while True:
                e = parent.recv()
                events.append(e)
                if e['event'] in ('complete', 'error'):
                    break
        parent.send(dict(action='stop'))
        t.join(10)
        return [(e['event'], e.get('operation_id'), e.get('fid'), e.get('ok'), e.get('error')) for e in events if e['event'] != 'submitted']

    def test_with_the_f3_flag_the_late_completion_is_reported_and_the_next_op_gets_its_own(self):
        ev = self.run_child(True)
        self.assertEqual(ev[0][:2], ('error', '1'))
        self.assertIn('native completion deadline', ev[0][4])
        self.assertEqual(ev[1:], [('late', '1', 21, True, ''), ('complete', None, None, None, None)])

    def test_without_the_flag_the_late_completion_is_unowned_as_today(self):
        for flag in (None, False):  # absent (every other row) or not exactly True
            with self.subTest(flag=flag):
                ev = self.run_child(flag)
                self.assertEqual([e[:2] for e in ev], [('error', '1'), ('error', '2')])
                self.assertIn('unowned completion', ev[1][4])


if __name__ == '__main__':
    unittest.main()
