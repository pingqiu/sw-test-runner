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

    def controller(self, at, close=True, fail_after_ack=False, fail_after_close=False):
        def run():
            time.sleep(at)
            self.write('fault-pause-E1.json', dict(episode='E1', role='volume', pause_mono=time.monotonic()))
            ack = self.root / 'fault-ack-E1.json'
            while not ack.exists():
                time.sleep(0.01)
            if fail_after_ack:  # every acknowledged pending GET (1.5 s each) now completes with a timeout
                (self.root / 'fake-fail').touch()
                time.sleep(1.7)
                (self.root / 'fake-fail').unlink()
            if close:
                self.write('fault-close-E1.json', dict(episode='E1', end_mono=time.monotonic()))
            if fail_after_close:
                time.sleep(1.7)  # every old pending GET is done; the next ones are post-close
                (self.root / 'fake-fail').touch()
        t = threading.Thread(target=run, daemon=True)
        t.start()
        return t

    def events(self):
        return [json.loads(l) for l in (self.root / 'op-events.jsonl').read_text().splitlines()]

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

    def test_without_a_close_the_worker_never_resumes(self):
        self.controller(1.0, close=False)
        soak_load.run(self.config)
        ack = json.loads((self.root / 'fault-ack-E1.json').read_text())
        after = [e for e in self.events() if float(e['submitted']) > ack['observed_mono']]
        self.assertEqual(after, [], 'operations dispatched after the pause without a close')


if __name__ == '__main__':
    unittest.main()
