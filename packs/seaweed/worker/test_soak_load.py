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


if __name__ == '__main__':
    unittest.main()
