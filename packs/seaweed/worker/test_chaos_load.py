"""CPU controls for chaos_load.py; fake native connector and fake S3, no hosts."""
import json, pathlib, sys, tempfile, unittest
from unittest import mock
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import chaos_load
from test_kv_smoke import FakeConnector

C = dict(run_id='cpu', client='client1-c1', transport='http', keys=16, length=64, root='/testops/cpu/client1-c1', filer_http='http://filer:8888')


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

    def test_kv_miss_is_a_recorded_failure_not_fatal(self):
        r = Rig(kv_miss={'client1-c1-w0'})
        out = r.run('load')
        failed = [o for o in out['outcomes'] if not o['exact']]
        self.assertTrue(failed and all(o['kind'] == 'kv_get' and o['key'] == 'client1-c1-w0' for o in failed))

    def test_failed_put_is_unknown_and_never_read_back(self):
        r = Rig(s3_mode='put-fails')
        out = r.run('load')
        self.assertIn('client1-c1-w2', out['unknown'])
        keys = [json.loads(x)['key'] for x in r.journal.read_text().splitlines()]
        self.assertNotIn('client1-c1-w2', keys)
        self.assertNotIn('client1-c1-w2', [o['key'] for o in out['outcomes'] if o['kind'] == 's3_get'])

    def test_wrong_bytes_and_hangs_are_fatal(self):
        for mode, error in (('wrong', AssertionError), ('hang', TimeoutError)):
            with self.subTest(mode=mode), self.assertRaises(error):
                Rig(s3_mode=mode).run('load')

    def test_readback_requires_every_ack_exact(self):
        self.assertEqual(Rig().run('readback')['status'], 'PASS')
        r = Rig()
        r.run('load')
        r.kv_miss = {'client1-c1-w0'}
        with self.assertRaises(AssertionError):
            with mock.patch.object(chaos_load, 's3', r.s3):
                chaos_load.readback(r.conn, C, 'http://s3', r.journal, r.helper)


if __name__ == '__main__':
    unittest.main()
