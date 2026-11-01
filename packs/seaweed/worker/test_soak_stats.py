import json
import os
import tempfile
import threading
import time
import unittest
from pathlib import Path

import soak_stats


class FakeConn:
    def __init__(self, fail=False):
        self.fail = fail

    def stats_json(self):
        if self.fail:
            raise RuntimeError('native stats unavailable')
        return json.dumps(dict(write_pool_stale_dropped=0))


class SoakStats(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: [p.unlink() for p in self.root.iterdir()] and None or self.root.rmdir())
        self.tick = soak_stats.TICK_S
        soak_stats.TICK_S = 0.05
        self.addCleanup(setattr, soak_stats, 'TICK_S', self.tick)

    def witness(self, **override):
        row = dict(boot_id=soak_stats.boot_id(), mono=time.monotonic())
        row.update(override)
        (self.root / soak_stats.WITNESS).write_text(json.dumps(row))

    def rows(self):
        [path] = list(self.root.glob('native-stats-*.jsonl'))
        return [json.loads(line) for line in path.read_text().splitlines()]

    def test_identity_of_self_and_gone(self):
        me = soak_stats.identity(os.getpid())
        self.assertEqual(me['pid'], os.getpid())
        self.assertGreater(me['starttime'], 0)
        self.assertIsNone(soak_stats.identity(2 ** 22 + 12345))

    def test_records_one_row_per_tick_until_the_horizon(self):
        self.witness()
        soak_stats.native_stats(FakeConn(), self.root, threading.Event(), horizon_s=0.2)
        rows = self.rows()
        ticks = [round(r['body_elapsed'] / soak_stats.TICK_S) for r in rows]
        # Consecutive ticks, none before the start (a tick already past is skipped), none after the horizon.
        self.assertEqual(ticks, list(range(ticks[0], ticks[0] + len(ticks))))
        self.assertLessEqual(ticks[0], 1)
        self.assertLessEqual(rows[-1]['body_elapsed'], 0.2 + 1e-9)
        self.assertGreaterEqual(len(rows), 3)
        self.assertTrue(all(r['status'] == 'OBSERVED' and r['identity']['pid'] == os.getpid() for r in rows))

    def test_a_failing_stats_call_is_recorded_not_raised(self):
        self.witness()
        soak_stats.native_stats(FakeConn(fail=True), self.root, threading.Event(), horizon_s=0.05)
        self.assertTrue(all(r['status'] == 'NOT_OBSERVED' and 'unavailable' in r['error'] for r in self.rows()))

    def test_no_witness_ends_bounded_with_not_started(self):
        start = time.monotonic()
        soak_stats.native_stats(FakeConn(), self.root, threading.Event(), horizon_s=10, wait_s=0.3)
        self.assertLess(time.monotonic() - start, 2)
        self.assertEqual(self.rows(), [dict(status='NOT_STARTED', error='no load-start witness')])

    def test_foreign_boot_or_bad_clock_witness_is_refused(self):
        for override in (dict(boot_id='other-boot'), dict(mono=float('nan')), dict(mono=-1), dict(mono=True)):
            with self.subTest(override=override):
                self.witness(**override)
                with self.assertRaises(ValueError):
                    soak_stats.read_witness(self.root)

    def test_stop_ends_the_recorder_early(self):
        self.witness()
        stop = threading.Event()
        thread = threading.Thread(target=soak_stats.native_stats, args=(FakeConn(), self.root, stop, 60))
        thread.start()
        time.sleep(0.2)
        stop.set()
        thread.join(2)
        self.assertFalse(thread.is_alive())
        self.assertLess(len(self.rows()), 10)


if __name__ == '__main__':
    unittest.main()
