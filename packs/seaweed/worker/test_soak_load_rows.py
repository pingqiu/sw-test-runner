"""Stage 3 load rows through the real soak_load.run() with the fake native child (spawned processes) and a test
thread as the controller: the armed window, ACKED/REFUSED/UNKNOWN, F1/F2 quarantine, the ledger and the audit."""
import json
import threading
import time
import unittest

import soak_load
import test_soak_load
from soak_oracle import Oracle, OracleError


def jsonl(path):
    return [json.loads(line) for line in path.read_text().splitlines()]


class Rows(unittest.TestCase):
    # The existing fault-window fixture (fake child, topology server, run root), without re-running its tests.
    setUp, write, toggle = (test_soak_load.FaultWindowWorker.setUp, test_soak_load.FaultWindowWorker.write,
                            test_soak_load.FaultWindowWorker.toggle)
    serve = test_soak_load.TopologyPoller.serve
    def row(self, kind, **kw):
        self.config['fault_row'] = dict(dict(kind=kind, episode='E1', at_s=1, refused=['No space left on device'], audit_s=20), **kw)
        self.config['native'].update(fake_delay_get=0.05, fake_delay_set=0.3)

    def armed_controller(self, at, fail_for, error='connection reset', close=True):
        def run():
            while not (self.root / 'soak-load-start.json').exists():
                time.sleep(0.01)
            time.sleep(at)
            self.write('fault-pause-E1.json', dict(episode='E1', role='volume', pause_mono=time.monotonic()))
            while not (self.root / 'fault-ack-E1.json').exists():
                time.sleep(0.01)
            (self.root / 'fake-fail-set').write_text(error)
            time.sleep(fail_for)
            (self.root / 'fake-fail-set').unlink()
            if close:
                self.write('fault-close-E1.json', dict(episode='E1', role='volume', end_mono=time.monotonic()))
        t = threading.Thread(target=run, daemon=True)
        t.start()
        return t

    def test_f1_armed_window_quarantines_attributed_unknowns_and_the_audit_reads_every_ledger_key(self):
        self.row('volume_kill_io')
        self.armed_controller(1.0, 1.0)
        summary = soak_load.run(self.config)
        self.assertEqual(summary['status'], 'LOAD_DONE')
        ack = json.loads((self.root / 'fault-ack-E1.json').read_text())
        self.assertTrue(ack['armed'] and ack['inflight_sets'] == len(ack['pending_sets']))
        ledger = jsonl(self.root / 'write-ledger.jsonl')
        unknown = [r for r in ledger if r['state'] == 'UNKNOWN']
        self.assertTrue(unknown and all(r['attributed'] == 'E1' for r in unknown), ledger[-5:])
        self.assertEqual(summary['counts']['unknown_attributed'], len(unknown))
        # never retried: no PUT of a quarantined key dispatched after its UNKNOWN
        for u in unknown:
            self.assertFalse([r for r in ledger if r['key'] == u['key'] and r['dispatched'] > u['dispatched']])
        # the load kept running through the window (no pause), and the audit covered every ledger key
        audited = {a['key'] for a in jsonl(self.root / 'audit.jsonl')}
        self.assertEqual(audited, {r['key'] for r in ledger if r['state'] != 'REFUSED'})
        done = [e for e in jsonl(self.root / 'op-events.jsonl') if e.get('event') == 'audit_done']
        workers = json.loads((self.root / 'workers.json').read_text())
        self.assertTrue(done[0]['complete'] and done[0]['client']['pid'] not in [w['pid'] for w in workers.values()])
        events = jsonl(self.root / 'op-events.jsonl')
        marks = [e for e in events if e.get('event') == 'fault_window']
        self.assertEqual([m['phase'] for m in marks], ['armed', 'closed'])
        # armed, not paused: the load kept dispatching between the ack and the close
        inside = [e for e in events if 'action' in e and marks[0]['mono'] < e['dispatched'] < marks[1]['mono']]
        self.assertGreater(len(inside), 2, 'no dispatch while armed')

    def test_f1_quarantined_keys_are_never_selected_again(self):
        # a long window over the soak's 32 hot slots: without the busy pin a quarantined key is re-drawn and the
        # oracle stops the run (UNKNOWN_WRITE retry); with it the load runs to the end
        self.row('volume_kill_io')
        self.config.update(duration_s=8, pacing_s=0.05)
        self.config['native']['fake_delay_set'] = 0.05
        self.armed_controller(0.5, 5.0)
        self.assertEqual(soak_load.run(self.config)['status'], 'LOAD_DONE')
        self.assertGreater(len([r for r in jsonl(self.root / 'write-ledger.jsonl') if r['state'] == 'UNKNOWN']), 5)

    def test_an_unknown_outside_the_window_stops_the_run_as_today(self):
        self.row('volume_kill_io')
        self.toggle('fake-fail-set', 1.0, 5)
        with self.assertRaisesRegex(OracleError, 'UNKNOWN_WRITE'):
            soak_load.run(self.config)
        last = jsonl(self.root / 'write-ledger.jsonl')[-1]
        self.assertEqual((last['state'], last['attributed']), ('UNKNOWN', None))

    def test_an_unknown_inside_a_non_kill_row_window_still_stops(self):
        self.row('link_flap')
        self.armed_controller(1.0, 3.0)
        with self.assertRaisesRegex(OracleError, 'UNKNOWN_WRITE'):
            soak_load.run(self.config)

    def test_a_named_refusal_is_refused_and_the_load_continues(self):
        self.row('disk_full')
        self.armed_controller(1.0, 1.0, error='write failed: No space left on device')
        summary = soak_load.run(self.config)
        self.assertEqual(summary['status'], 'LOAD_DONE')
        ledger = jsonl(self.root / 'write-ledger.jsonl')
        self.assertTrue([r for r in ledger if r['state'] == 'REFUSED'])
        self.assertFalse([r for r in ledger if r['state'] == 'UNKNOWN'])

    def test_f2_kills_a_worker_with_its_put_pending(self):
        self.row('client_kill_put')
        self.config['native']['fake_delay_set'] = 0.5
        summary = soak_load.run(self.config)
        self.assertEqual(summary['status'], 'LOAD_DONE')
        k = summary['kills'][0]
        self.assertEqual((k['kind'], k['witness']['action']), ('client_kill_put', 'set'))
        ledger = jsonl(self.root / 'write-ledger.jsonl')
        u = [r for r in ledger if r['state'] == 'UNKNOWN']
        self.assertEqual(len(u), 1)
        self.assertEqual((u[0]['op'], u[0]['attributed'], u[0]['worker_identity']),
                         (k['witness']['operation_id'], 'E1', k['victim']))
        self.assertNotEqual(k['victim']['pid'], k['replacement']['pid'])
        # its key had no prior ACK: the audit must read it at the UNKNOWN generation's own seed and length
        audit = {a['key']: a['reads'] for a in jsonl(self.root / 'audit.jsonl')}
        self.assertIn(u[0]['key'], audit)
        self.assertEqual([r['seed'] for r in audit[u[0]['key']]], [u[0]['seed']])
        self.assertEqual(summary['counts']['unknown_attributed'], 1)

    def test_rows_never_run_the_soak_client_kill(self):
        self.row('control')
        self.config['client_kill_at_s'] = 0
        summary = soak_load.run(self.config)
        self.assertEqual(summary['kills'], [])


class OracleStates(unittest.TestCase):
    def test_quarantine_refuses_retry_and_read_and_refusal_keeps_the_survivor(self):
        o = Oracle()
        v = o.begin_write('k', b'a')
        o.acknowledge('k', v.sequence, 1, ['3,ab'])
        o.begin_write('k', b'bb')
        survivor, intent = o.quarantine_write('k')
        self.assertEqual((survivor.length, intent.length), (1, 2))
        for call in (lambda: o.begin_write('k', b'c'), lambda: o.begin_read('k')):
            with self.assertRaises(OracleError):
                call()
        o.begin_write('j', b'x')
        o.refuse_write('j')
        self.assertNotIn('j', o.pending)
        with self.assertRaisesRegex(OracleError, 'UNKNOWN_WRITE'):  # everywhere else, unchanged
            o.begin_write('m', b'x')
            o.abandon_write('m')


if __name__ == '__main__':
    unittest.main()
