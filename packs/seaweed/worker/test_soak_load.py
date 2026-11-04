import io
import json
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import soak_load


class VolumeIds(unittest.TestCase):
    def test_ranges_lists_and_nesting(self):
        topo = dict(DataCenters=[dict(Racks=[dict(DataNodes=[dict(VolumeIds=' 1-4 7,9-10'), dict(VolumeIds='12')])])])
        self.assertEqual(soak_load.volume_ids(topo), [1, 2, 3, 4, 7, 9, 10, 12])

    def test_malformed_or_unbounded_refused(self):
        for text in ('4-1', '0', '1-999999', '1/foo', 7):
            with self.subTest(text=text), self.assertRaises(ValueError):
                soak_load.volume_ids(dict(VolumeIds=text))


class TopologyPoller(unittest.TestCase):
    LAYOUT = dict(collection='lmcache', replication='000', ttl='2h')

    def serve(self, answers):
        calls = []

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                status, body = answers[min(len(calls), len(answers) - 1)]
                calls.append(self.path)
                raw = body if isinstance(body, bytes) else json.dumps(body).encode()
                self.send_response(status)
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return 'http://127.0.0.1:%d' % server.server_address[1], calls

    def poll(self, answers, seconds):
        url, calls = self.serve(answers)
        out, stop = io.StringIO(), threading.Event()
        thread = threading.Thread(target=soak_load.poll_topology, args=(dict(master_http=url, layout=self.LAYOUT), stop, out))
        thread.start()
        time.sleep(seconds)
        stop.set()
        thread.join(5)
        return [json.loads(line) for line in out.getvalue().splitlines()], calls

    def test_complete_poll_records_layout_writables_and_presence(self):
        good = dict(Topology=dict(Layouts=[dict(collection='lmcache', replication='000', ttl='2h', writables=[5, 6]),
                                           dict(collection='other', replication='000', ttl='', writables=[9])],
                                  DataCenters=[dict(Racks=[dict(DataNodes=[dict(VolumeIds='5-7 9')])])]))
        rows, calls = self.poll([(200, good)], 0.5)
        self.assertEqual(calls, ['/dir/status'])
        self.assertTrue(rows[0]['ok'])
        self.assertEqual((rows[0]['writables'], rows[0]['present']), ([5, 6], [5, 6, 7, 9]))
        self.assertLessEqual(rows[0]['req'], rows[0]['done'])

    def test_error_partial_or_empty_poll_is_a_gap_not_absence(self):
        for answer in ((503, dict(error='warming')), (200, b'{"Topology": {'), (200, dict(Topology=dict())), (200, b'')):
            with self.subTest(answer=answer):
                rows, _ = self.poll([answer], 0.3)
                self.assertFalse(rows[0]['ok'])
                self.assertNotIn('present', rows[0])
                self.assertNotIn('writables', rows[0])

    def test_polls_every_two_seconds_until_stopped(self):
        good = dict(Topology=dict(Layouts=[], DataCenters=[]))
        rows, _ = self.poll([(200, good)], 4.5)
        self.assertEqual(len(rows), 3)
        gaps = [b['req'] - a['req'] for a, b in zip(rows, rows[1:])]
        self.assertTrue(all(1.9 < g < 2.3 for g in gaps), gaps)


class Publish(unittest.TestCase):
    def test_atomic_exclusive_and_no_temp_left(self):
        import tempfile
        from pathlib import Path
        root = Path(tempfile.mkdtemp())
        soak_load.publish(root / 'a.json', dict(x=1))
        self.assertEqual(json.loads((root / 'a.json').read_text()), dict(x=1))
        with self.assertRaises(FileExistsError):
            soak_load.publish(root / 'a.json', dict(x=2))
        self.assertEqual(sorted(p.name for p in root.iterdir()), ['a.json'])
        self.assertEqual(json.loads((root / 'a.json').read_text()), dict(x=1))


class ReadBound(unittest.TestCase):  # codex01 R4
    def test_oversized_file_is_refused_not_truncated(self):
        import tempfile
        from pathlib import Path
        root = Path(tempfile.mkdtemp())
        valid = json.dumps(dict(run_id='r', pad='x' * 4000))
        (root / 'f.json').write_text(valid + ' ' * (4096 - len(valid)) + 'garbage beyond the bound')
        with self.assertRaisesRegex(ValueError, 'over 4096'):
            soak_load.read_bound(root / 'f.json', dict(run_id='r'))
        (root / 'g.json').write_text(valid)
        self.assertEqual(soak_load.read_bound(root / 'g.json', dict(run_id='r'))['run_id'], 'r')


class FaultWindowWorker(unittest.TestCase):
    """The real run() with a fake native child (spawned processes) and a test
    thread acting as the controller of the fault-window handshake (r4)."""

    def setUp(self):
        import shutil
        import tempfile
        from pathlib import Path
        import soak_stats
        import test_soak_fake_child
        self.root = Path(tempfile.mkdtemp())
        self.root.chmod(0o700)
        self.addCleanup(shutil.rmtree, self.root)
        saved = soak_load.child
        soak_load.child = test_soak_fake_child.child
        self.addCleanup(setattr, soak_load, 'child', saved)
        url, _ = TopologyPoller.serve(self, [(200, dict(Topology=dict(Layouts=[], DataCenters=[])))])
        here = str(Path(__file__).resolve().parent)
        self.boot = soak_stats.boot_id()
        self.config = dict(root=str(self.root), master_http=url, layout=TopologyPoller.LAYOUT, duration_s=4,
                           pacing_s=0.25, quiescence_s=2, write_stop_s=1, client_kill_at_s=10 ** 6, key_prefix='k@',
                           smoke=dict(run_id='run-1', transport='http', keys=1, length=1, helper_dir=here, native_dir=here),
                           native=dict(fake_delay_get=1.5, fake_delay_set=0.05))

    def write(self, name, row):  # the controller publishes atomically, like the Go adapter must
        soak_load.publish(self.root / name, dict(dict(run_id='run-1', boot_id=self.boot), **row))

    def controller(self, at, close=True, fail_after_ack=False, fail_after_close=False, close_row=None, error=''):
        def run():
            # Like the real controller: the fault clock starts at the load-start
            # witness, never at enrollment (spawning 4 workers can take seconds).
            while not (self.root / 'soak-load-start.json').exists():
                time.sleep(0.01)
            time.sleep(at)
            self.write('fault-pause-E1.json', dict(episode='E1', role='volume', pause_mono=time.monotonic()))
            ack = self.root / 'fault-ack-E1.json'
            while not ack.exists():
                time.sleep(0.01)
            if fail_after_ack:  # every acknowledged pending GET (1.5 s each) now completes with an error
                (self.root / 'fake-fail').write_text(error)
                time.sleep(1.7)
                (self.root / 'fake-fail').unlink()
            if close:
                self.write('fault-close-E1.json', close_row or dict(episode='E1', role='volume', end_mono=time.monotonic()))
            if fail_after_close:
                time.sleep(1.7)  # every old pending GET is done; the next ones are post-close
                (self.root / 'fake-fail').touch()
        t = threading.Thread(target=run, daemon=True)
        t.start()
        return t

    def events(self):  # operation events only (body_end/client_kill markers carry no op)
        return [e for e in (json.loads(l) for l in (self.root / 'op-events.jsonl').read_text().splitlines()) if 'op' in e]

    def test_pause_stops_dispatch_acks_pending_gets_and_credits_their_errors(self):
        self.controller(1.5, fail_after_ack=True)
        summary = soak_load.run(self.config)
        ack = json.loads((self.root / 'fault-ack-E1.json').read_text())
        self.assertEqual((ack['episode'], ack['inflight_sets'], ack['run_id']), ('E1', 0, 'run-1'))
        self.assertTrue(ack['pause_mono'] <= ack['observed_mono'] <= ack['ack_mono'])
        self.assertTrue(all(g['submitted'] <= ack['observed_mono'] for g in ack['pending_gets']))
        pending = {g['op'] for g in ack['pending_gets']}
        credited = [e for e in self.events() if e.get('fault_error')]
        self.assertTrue(pending and credited, (pending, credited))
        self.assertEqual({e['op'] for e in credited}, pending)  # every pending GET, and only those
        self.assertEqual(summary['status'], 'LOAD_DONE')
        # No new dispatch between the pause and the close.
        ops = [int(e['op']) for e in self.events()]
        self.assertEqual(sorted(ops), list(range(1, len(ops) + 1)))

    def toggle(self, name, at, length):
        def run():  # timed from the load-start witness, like the controller (spawning workers takes seconds)
            while not (self.root / 'soak-load-start.json').exists():
                time.sleep(0.01)
            time.sleep(at)
            (self.root / name).touch()
            time.sleep(length)
            (self.root / name).unlink()
        threading.Thread(target=run, daemon=True).start()

    def completions(self):
        return [json.loads(l) for l in (self.root / 'completions.jsonl').read_text().splitlines()]

    def test_an_ok_get_without_the_key_is_a_miss_candidate_as_in_r9(self):
        # r9 soak_workload.py (sha 6a4bbb6b) l.74-99: ok=True with results != [True] is a miss judged by retirement.
        self.toggle('fake-notfound', 1.0, 2.0)
        summary = soak_load.run(self.config)
        misses = [e for e in self.events() if e.get('miss')]
        self.assertEqual(summary['status'], 'LOAD_DONE')
        self.assertTrue(misses and all(e['ok'] is True and e['results'] == [False] and e['fids'] for e in misses), misses)

    def test_a_failed_get_with_an_empty_error_is_still_fatal_and_its_row_is_kept(self):
        self.toggle('fake-fail-empty', 1.5, 10)
        with self.assertRaisesRegex(soak_load.OracleError, 'unexpected GET error'):
            soak_load.run(self.config)
        last = self.completions()[-1]  # the stop is attributable: its raw row was written first
        self.assertEqual((last['action'], last['row']['ok'], last['row']['error']), ('get', False, ''))

    def test_every_completion_is_written_raw(self):
        soak_load.run(self.config)
        self.assertEqual(sorted(c['op'] for c in self.completions()), sorted(e['op'] for e in self.events()))

    def test_a_get_error_without_a_fault_window_is_fatal(self):
        def fail():
            time.sleep(1.5)
            (self.root / 'fake-fail').touch()
        threading.Thread(target=fail, daemon=True).start()
        with self.assertRaisesRegex(soak_load.OracleError, 'unexpected GET error'):
            soak_load.run(self.config)

    def test_a_new_get_after_the_close_is_fatal(self):
        self.controller(1.0, fail_after_close=True)
        self.config['duration_s'] = 6  # room for post-close GETs to complete
        with self.assertRaisesRegex(soak_load.OracleError, 'unexpected GET error'):
            soak_load.run(self.config)

    def test_a_foreign_pause_is_refused(self):
        def foreign():
            time.sleep(1.0)
            soak_load.publish(self.root / 'fault-pause-E1.json',
                              dict(run_id='other-run', boot_id=self.boot, episode='E1', role='volume', pause_mono=1.0))
        threading.Thread(target=foreign, daemon=True).start()
        with self.assertRaisesRegex(ValueError, 'binding differs'):
            soak_load.run(self.config)

    def malformed_close(self, row):  # codex01 R1: a malformed close fails, it never resumes
        self.controller(1.0, close_row=row)
        with self.assertRaises(ValueError):
            soak_load.run(self.config)

    def test_a_close_with_the_wrong_role_fails(self):
        self.malformed_close(dict(episode='E1', role='master', end_mono=10 ** 6))

    def test_a_close_without_a_time_fails(self):
        self.malformed_close(dict(episode='E1', role='volume'))

    def test_a_close_with_a_nan_time_fails(self):
        self.malformed_close(dict(episode='E1', role='volume', end_mono=float('nan')))

    def test_a_close_before_the_ack_fails(self):
        self.malformed_close(dict(episode='E1', role='volume', end_mono=1.0))

    def test_a_non_transient_error_on_an_acked_get_ends_the_load(self):  # codex01 R2
        self.controller(1.0, fail_after_ack=True, error='permission denied')
        with self.assertRaisesRegex(soak_load.OracleError, 'unexpected GET error'):
            soak_load.run(self.config)

    def test_a_future_pause_is_refused(self):  # codex01 R1 remainder: pause <= observed
        def future():
            while not (self.root / 'soak-load-start.json').exists():
                time.sleep(0.01)
            time.sleep(1.0)
            self.write('fault-pause-E1.json', dict(episode='E1', role='volume', pause_mono=time.monotonic() + 60))
        threading.Thread(target=future, daemon=True).start()
        with self.assertRaisesRegex(ValueError, 'dated after'):
            soak_load.run(self.config)
        self.assertFalse((self.root / 'fault-ack-E1.json').exists())

    def test_without_a_close_the_worker_never_resumes(self):
        self.controller(1.0, close=False)
        soak_load.run(self.config)
        ack = json.loads((self.root / 'fault-ack-E1.json').read_text())
        after = [e for e in self.events() if float(e['submitted']) > ack['observed_mono']]
        self.assertEqual(after, [], 'operations dispatched after the pause without a close')



class PatternMode(unittest.TestCase):
    """kv_load's client pattern: what a free client issues next."""
    BASE = dict(clients=4, transport='dc', keys=100, distribution='zipf', zipf_s=1.1, object_bytes=64 << 10,
                rate_per_client=10, phases=[dict(kind='write_once'), dict(kind='read', duration_s=60)])

    def pattern(self, **change):
        import random
        return soak_load.Pattern(dict(self.BASE, **change), random.Random(7), 'p/')

    def test_write_once_writes_every_key_once_in_order_then_waits_for_the_acks(self):
        p = self.pattern()
        sets = [p.next(0, True, {}, {}, {}) for _ in range(100)]
        self.assertEqual([s[1] for s in sets], [p.key(i) for i in range(100)])
        self.assertEqual({s[0] for s in sets}, {'set'})
        self.assertEqual(len({s[2] for s in sets}), 100)
        latest = {p.key(i): 1 for i in range(100)}
        self.assertIsNone(p.next(1, True, {'pending': 1}, latest, {}))  # reads wait for every ACK
        op = p.next(2, True, {}, latest, {})
        self.assertEqual(op[0], 'get')
        self.assertTrue(all(p.next(3, True, {}, latest, {})[0] == 'get' for _ in range(200)))  # never written again

    def test_the_read_phase_ends_after_its_duration(self):
        p = self.pattern()
        for _ in range(100):
            p.next(0, True, {}, {}, {})
        latest = {p.key(i): 1 for i in range(100)}
        self.assertIsNotNone(p.next(10, True, {}, latest, {}))  # the read phase starts at 10
        self.assertIsNotNone(p.next(69.9, True, {}, latest, {}))
        self.assertIsNone(p.next(70, True, {}, latest, {}))

    def test_zipf_concentrates_reads_on_the_hot_keys_and_uniform_does_not(self):
        for dist, s, low, high in (('zipf', 1.1, .55, .64), ('uniform', 0, .005, .02)):  # zipf(1.1) over 4096: 0.593
            p = self.pattern(keys=4096, distribution=dist, zipf_s=s)
            draws = [p.draw() for _ in range(20000)]
            top = sum(d < 41 for d in draws) / len(draws)  # the top 1% of keys
            self.assertTrue(low < top < high, (dist, top))

    def test_mixed_writes_about_its_share_and_only_while_writing_is_allowed(self):
        p = self.pattern(phases=[dict(kind='write_once'), dict(kind='mixed', duration_s=60, read_ratio=.75)])
        for _ in range(100):
            p.next(0, True, {}, {}, {})
        latest = {p.key(i): 1 for i in range(100)}
        ops = [p.next(1, True, {}, latest, {})[0] for _ in range(4000)]
        self.assertAlmostEqual(ops.count('set') / len(ops), .25, delta=.03)
        self.assertNotIn('set', [p.next(1, False, {}, latest, {})[0] for _ in range(400)])

    def test_write_once_stops_at_the_write_stop(self):
        p = self.pattern()
        self.assertIsNone(p.next(0, False, {}, {}, {}))
        self.assertEqual(p.cursor, 0)
        self.assertFalse(p.finished(10 ** 6))

    def test_a_busy_key_is_skipped_not_doubled(self):
        p = self.pattern(keys=1, distribution='uniform', zipf_s=0)
        p.next(0, True, {}, {}, {})
        self.assertIsNone(p.next(1, True, {}, {p.key(0): 1}, {p.key(0): 2}))


class KVLoadRun(unittest.TestCase):
    """The real run() in pattern mode, with the fake native child."""
    setUp, events = FaultWindowWorker.setUp, FaultWindowWorker.events
    PATTERN = dict(clients=2, transport='dc', keys=6, distribution='uniform', object_bytes=64, rate_per_client=50,
                   phases=[dict(kind='write_once'), dict(kind='read', duration_s=1)])

    def test_every_write_is_acked_before_any_read_and_the_reads_run(self):
        self.config['pattern'] = self.PATTERN
        soak_load.run(self.config)
        ops = self.events()
        sets, gets = [e for e in ops if e['action'] == 'set'], [e for e in ops if e['action'] == 'get']
        self.assertEqual(len({e['key'] for e in sets}), 6)
        self.assertEqual(len(sets), 6)
        self.assertTrue(gets)
        self.assertLessEqual(max(e['completed'] for e in sets), min(e['dispatched'] for e in gets))

    def test_phases_that_do_not_complete_fail_the_load(self):
        self.config['pattern'] = dict(self.PATTERN, phases=[dict(kind='write_once'), dict(kind='read', duration_s=30)])
        with self.assertRaisesRegex(soak_load.OracleError, 'did not complete'):
            soak_load.run(self.config)


if __name__ == '__main__':
    unittest.main()
