"""CPU controls for chaos_load.py; fake native connector and fake S3, no hosts."""
import http.server, json, pathlib, re, sys, tempfile, threading, time, unittest
from unittest import mock
from urllib.error import URLError
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import chaos_load
from test_kv_smoke import FakeConnector

# The client name carries the stage-2 case name, uppercase included (run 7c).
C = dict(run_id='cpu', client='client1-C1-single-kill', transport='http', keys=16, length=64, root='/testops/cpu/client1-C1-single-kill', filer_http='http://filer:8888')
BUCKET = re.compile(r'[a-z0-9][a-z0-9-]{1,61}[a-z0-9]')


class Rig:
    def __init__(self, s3_mode='good', kv_miss=()):
        self.now, self.store, self.s3_mode, self.kv_miss = [0.0], {}, s3_mode, set(kv_miss)
        self.helper = chaos_load.kv_smoke().exact_helper('soak_native_worker')
        self.conn = FakeConnector('explicit-http')
        rig = self
        get = self.conn.submit_batch_get
        def submit_get(keys, buffers):
            if keys[0] in rig.kv_miss:
                rig.conn.values[keys[0]] = bytes(len(buffers[0]))  # zeros: not the expected bytes
                rig.conn.mode = 'miss'
                try:
                    return get(keys, buffers)
                finally:
                    rig.conn.mode = 'explicit-http'
            return get(keys, buffers)
        self.conn.submit_batch_get = submit_get

    def s3(self, url, data=None, method='GET'):
        if not BUCKET.fullmatch(url.split('/')[3]):  # S3 bucket naming rules, as the gateway applies them
            return False, 'http 400 InvalidBucketName'
        if self.s3_mode == 'hang':
            raise TimeoutError('S3 op exceeded 30 s')
        if method == 'PUT':
            if url.endswith('-w2') and self.s3_mode == 'put-fails':
                return False, 'http 503'
            self.store[url] = data
            return True, b''
        body = self.store.get(url)
        if self.s3_mode == 'wrong' and body:
            return True, b'x' * len(body)
        return (True, body) if body is not None else (False, 'http 404')

    def run(self, stage, seconds=40):
        journal = pathlib.Path(tempfile.mkdtemp()) / 'journal.jsonl'
        if stage == 'readback':
            self.run('load', seconds)
            journal = self.journal
        with mock.patch.object(chaos_load, 's3', self.s3), mock.patch.object(chaos_load, 'fids_of', lambda c, p: ['3,01ab']), \
                mock.patch.object(chaos_load.time, 'monotonic', lambda: self.now[0]), \
                mock.patch.object(chaos_load.time, 'sleep', lambda d: self.now.__setitem__(0, self.now[0] + 1)):
            try:
                if stage == 'load':
                    return chaos_load.load(self.conn, C, 'http://s3', seconds, journal, self.helper)
                return chaos_load.readback(self.conn, C, 'http://s3', journal, self.helper)
            finally:
                self.journal = journal


class ChaosLoadControls(unittest.TestCase):
    def test_mixed_write_once_load_journals_every_ack(self):
        r = Rig()
        out = r.run('load')
        kinds = [o['kind'] for o in out['outcomes']]
        self.assertEqual(kinds[:4], ['kv_set', 'kv_get', 's3_put', 's3_get'])
        sets = [o['key'] for o in out['outcomes'] if o['kind'] in ('kv_set', 's3_put')]
        self.assertEqual(len(sets), len(set(sets)), 'writes must be write-once')
        self.assertTrue(all(o['exact'] for o in out['outcomes']))
        rows = [json.loads(x) for x in r.journal.read_text().splitlines()]
        self.assertEqual((out['acked_kv'], out['acked_s3']), (sum(x['kind'] == 'kv' for x in rows), sum(x['kind'] == 's3' for x in rows)))
        self.assertTrue(all(x['event'] == 'ACK' and x['fids'] for x in rows))

    def test_every_client_and_case_shares_one_valid_bucket(self):
        # A bucket is a collection with its own volumes; one per client per case
        # ran out of free slots in the M02 rehearsal (run 7d would have 9 at 001).
        names = {chaos_load.bucket_name(dict(C, run_id='azure10-m01-20261101t135000z', client=cl))
                 for cl in ('client1-C1-single-kill', 'client2-C2-rolling', 'client3-C6-netem')}
        self.assertEqual(names, {'testops-azure10-m01-20261101t135000z-chaos'})
        self.assertTrue(BUCKET.fullmatch(names.pop()))

    def test_kv_miss_is_a_recorded_failure_not_fatal(self):
        r = Rig(kv_miss={'client1-C1-single-kill-w0'})
        out = r.run('load')
        failed = [o for o in out['outcomes'] if not o['exact']]
        self.assertTrue(failed and all(o['kind'] == 'kv_get' and o['key'] == 'client1-C1-single-kill-w0' for o in failed))

    def test_failed_put_is_unknown_and_never_read_back(self):
        r = Rig(s3_mode='put-fails')
        out = r.run('load')
        self.assertIn('client1-C1-single-kill-w2', out['unknown'])
        keys = [json.loads(x)['key'] for x in r.journal.read_text().splitlines()]
        self.assertNotIn('client1-C1-single-kill-w2', keys)
        self.assertNotIn('client1-C1-single-kill-w2', [o['key'] for o in out['outcomes'] if o['kind'] == 's3_get'])

    def test_wrong_bytes_and_hangs_are_fatal(self):
        for mode, error in (('wrong', AssertionError), ('hang', TimeoutError)):
            with self.subTest(mode=mode), self.assertRaises(error):
                Rig(s3_mode=mode).run('load')

    def test_readback_requires_every_ack_exact(self):
        self.assertEqual(Rig().run('readback')['status'], 'PASS')
        r = Rig()
        r.run('load')
        r.kv_miss = {'client1-C1-single-kill-w0'}
        with self.assertRaises(AssertionError):
            with mock.patch.object(chaos_load, 's3', r.s3):
                chaos_load.readback(r.conn, C, 'http://s3', r.journal, r.helper)

    def test_readback_refuses_an_inexact_s3_object(self):
        r = Rig()
        r.run('load')
        acked = [json.loads(x)['key'] for x in r.journal.read_text().splitlines() if json.loads(x)['kind'] == 's3']
        r.store['http://s3/testops-cpu-chaos/' + acked[0]] = b'x' * chaos_load.S3_LENGTH
        with self.assertRaises(AssertionError), mock.patch.object(chaos_load, 's3', r.s3):
            chaos_load.readback(r.conn, C, 'http://s3', r.journal, r.helper)

    def test_any_op_over_30s_is_fatal(self):
        for kind in ('kv', 's3'):
            with self.subTest(kind=kind):
                r = Rig()
                slow = lambda: r.now.__setitem__(0, r.now[0] + 31)
                work, s3 = r.helper.work, r.s3
                if kind == 'kv':
                    r.helper.work = lambda conn, cmd: (slow(), work(conn, cmd))[1]
                else:
                    r.s3 = lambda *a, **k: (slow(), s3(*a, **k))[1]
                with self.assertRaises(TimeoutError):
                    r.run('load')

    def test_ack_stands_when_its_fid_lookup_fails(self):
        # From claude03's #48 review: C4 restarts the filer under load.
        r = Rig()
        calls = [0]
        def fids(c, p):
            calls[0] += 1
            if calls[0] == 2:
                raise URLError(ConnectionRefusedError(111, 'Connection refused'))
            return ['3,01ab']
        journal = pathlib.Path(tempfile.mkdtemp()) / 'journal.jsonl'
        with mock.patch.object(chaos_load, 's3', r.s3), mock.patch.object(chaos_load, 'fids_of', fids), \
                mock.patch.object(chaos_load.time, 'monotonic', lambda: r.now[0]), \
                mock.patch.object(chaos_load.time, 'sleep', lambda d: r.now.__setitem__(0, r.now[0] + 1)):
            out = chaos_load.load(r.conn, C, 'http://s3', 40, journal, r.helper)
        rows = [json.loads(x) for x in journal.read_text().splitlines()]
        self.assertEqual(len(rows), out['acked_kv'] + out['acked_s3'])
        self.assertEqual(sum(x['fids'] is None for x in rows), 1)

    def test_trickling_s3_response_over_30s_is_fatal(self):
        # From claude03's #48 review: a real server sends 35 bytes, one per
        # second; no single socket read waits 30 s, but the GET does.
        class Trickle(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200); self.send_header('Content-Length', '35'); self.end_headers()
                for _ in range(35):
                    self.wfile.write(b'z'); self.wfile.flush(); time.sleep(1)
            def log_message(self, *a): pass
        srv = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Trickle)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        try:
            with self.assertRaises(TimeoutError):
                chaos_load.s3('http://127.0.0.1:%d/obj' % srv.server_port)
        finally:
            srv.shutdown()


if __name__ == '__main__':
    unittest.main()
