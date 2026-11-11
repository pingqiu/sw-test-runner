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

    def test_f1_freeze_answers_with_ops_dispatched_after_the_freeze(self):
        """F1 freeze-then-kill, the load's side: after the armed ack the controller freezes the volume and publishes
        its clock; the load answers once with PUTs and GETs dispatched after it (never one dispatched before, though
        long PUTs from before the freeze are still pending), and keeps its contract through the kill."""
        self.row('volume_kill_io')
        self.config['native'].update(fake_delay_set=0.4, fake_delay_get=0.05)  # PUTs from before the freeze still run
        seen = {}

        def run():
            while not (self.root / 'soak-load-start.json').exists():
                time.sleep(0.01)
            time.sleep(1.0)
            self.write('fault-pause-E1.json', dict(episode='E1', role='volume', pause_mono=time.monotonic()))
            while not (self.root / 'fault-ack-E1.json').exists():
                time.sleep(0.01)
            (self.root / 'fake-frozen').write_text('')
            seen['freeze'] = time.monotonic()
            self.write('fault-freeze-E1.json', dict(episode='E1', role='volume', freeze_mono=seen['freeze'], stopped_pid=4242, stopped_starttime=66))
            end = time.monotonic() + 5  # the controller's bound
            while not (self.root / 'fault-inflight-E1.json').exists() and time.monotonic() < end:
                time.sleep(0.01)
            (self.root / 'fake-fail-set').write_text('connection reset')  # the kill: every pending op errors
            (self.root / 'fake-fail').write_text('connection reset')
            (self.root / 'fake-frozen').unlink()
            time.sleep(1.0)
            for f in ('fake-fail-set', 'fake-fail'):
                (self.root / f).unlink()
            self.write('fault-close-E1.json', dict(episode='E1', role='volume', end_mono=time.monotonic()))
        t = threading.Thread(target=run, daemon=True)
        t.start()
        summary = soak_load.run(self.config)
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        inflight = json.loads((self.root / 'fault-inflight-E1.json').read_text())
        self.assertEqual((inflight['freeze_mono'], inflight['stopped_pid'], inflight['stopped_starttime']), (seen['freeze'], 4242, 66))
        self.assertTrue(inflight['pending_sets'] and inflight['pending_gets'], inflight)
        listed = inflight['pending_sets'] + inflight['pending_gets']
        self.assertTrue(all(o['submitted'] > seen['freeze'] for o in listed), listed)
        events = [e for e in jsonl(self.root / 'op-events.jsonl') if 'action' in e and 'dispatched' in e]
        before = [e for e in events if e['action'] == 'set' and e['dispatched'] < seen['freeze'] < e.get('completed', 1e18)]
        self.assertTrue(before, 'no PUT from before the freeze was still pending: the filter is untested')
        self.assertFalse({str(e['op']) for e in before} & {o['op'] for o in listed})

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
        # F4 (r12: F3 now attributes its window, so the property is held by a row that still has none)
        self.row('conn_kill')
        self.armed_controller(1.0, 3.0)
        with self.assertRaisesRegex(OracleError, 'UNKNOWN_WRITE'):
            soak_load.run(self.config)

    # ---- F3 link flap (ruling 2026-11-11 01:54Z): an UNKNOWN in flight at link-down or submitted in [start, end]
    # is attributed and quarantined and the load continues; start = fault-pause pause_mono, end = fault-close
    # end_mono + recovery_s, both M01 marks; any other UNKNOWN stops the run.
    FLAP = dict(recovery_s=1, netdev='enp1s0v0', rdma_device='mlx5_0')

    def flap_row(self, **kw):
        self.row('link_flap', **dict(self.FLAP, **kw))
        self.config['pacing_s'] = 0.05  # a PUT is always in flight: some span the pause
        self.config['duration_s'] = 6  # PUTs stop write_stop_s (1 s) before the end: room for the post-close cases

    def flap_controller(self, at=1.0, fail=None, close_row=None, after_close=None, before_pause=None, fail_file='fake-fail-set'):
        """pause at `at`; fail: seconds of failing PUTs right after the ack; after_close: (delay, seconds) of failing
        PUTs after the close; before_pause: seconds of PUTs that fail and are delivered only after the ack."""
        seen = {}

        def run():
            try:
                steps()
            except OSError:  # the load stopped and the test removed its run root: nothing left to control
                pass

        def steps():
            while not (self.root / 'soak-load-start.json').exists():
                time.sleep(0.01)
            time.sleep(at)
            if before_pause:
                (self.root / 'fake-hold-complete').write_text('')
                (self.root / 'fake-fail-set').write_text('connection reset')
                time.sleep(before_pause)
                (self.root / 'fake-fail-set').unlink()
            seen['pause'] = time.monotonic()
            self.write('fault-pause-E1.json', dict(episode='E1', role='volume', pause_mono=seen['pause']))
            while not (self.root / 'fault-ack-E1.json').exists():
                time.sleep(0.01)
            if before_pause:
                (self.root / 'fake-hold-complete').unlink()
            if fail:
                (self.root / fail_file).write_text("TimeoutError('native completion deadline')")
                time.sleep(fail)
                (self.root / fail_file).unlink()
            seen['close'] = time.monotonic()
            self.write('fault-close-E1.json', close_row or dict(episode='E1', role='volume', end_mono=seen['close']))
            if after_close:
                time.sleep(after_close[0])
                (self.root / 'fake-fail-set').write_text('connection reset')
                time.sleep(after_close[1])
                (self.root / 'fake-fail-set').unlink()
        t = threading.Thread(target=run, daemon=True)
        t.start()
        return t, seen

    def flap_unknowns(self):
        return [r for r in jsonl(self.root / 'write-ledger.jsonl') if r['state'] == 'UNKNOWN']

    def test_f3_an_unknown_in_the_flap_window_is_quarantined_with_its_receipt_and_the_load_continues(self):
        self.flap_row()
        t, seen = self.flap_controller(fail=1.0)
        summary = soak_load.run(self.config)
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        unknown = self.flap_unknowns()
        self.assertTrue(unknown)
        self.assertEqual(summary['counts']['unknown_attributed'], len(unknown))
        for u in unknown:
            r = u['reason']
            self.assertEqual((u['attributed'], r['kind'], r['episode'], r['run_id'], r['boot_id'], r['netdev'], r['rdma_device']),
                             ('E1', 'link_flap', 'E1', 'run-1', self.boot, 'enp1s0v0', 'mlx5_0'))
            self.assertEqual((r['start'], r['end']), (seen['pause'], None))  # decided before the close was published
            self.assertTrue(u['completed'] >= r['start'])
            self.assertEqual(r['case'], 'in flight at link-down' if u['submitted'] < r['start'] else 'submitted in the down window')
        cases = {u['reason']['case'] for u in unknown}
        self.assertEqual(cases, {'in flight at link-down', 'submitted in the down window'}, unknown)
        ledger = jsonl(self.root / 'write-ledger.jsonl')
        for u in unknown:  # never retried
            self.assertFalse([r for r in ledger if r['key'] == u['key'] and r['dispatched'] > u['dispatched']])
        audited = {a['key'] for a in jsonl(self.root / 'audit.jsonl')}
        self.assertTrue({u['key'] for u in unknown} <= audited)

    def test_f3_an_unknown_submitted_after_the_close_within_the_recovery_bound_is_in_the_window(self):
        self.flap_row(recovery_s=2)
        t, seen = self.flap_controller(fail=None, after_close=(0.4, 0.5))
        summary = soak_load.run(self.config)
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        late = [u for u in self.flap_unknowns() if u['submitted'] > seen['close']]
        self.assertTrue(late, self.flap_unknowns())
        self.assertTrue(all(u['reason']['end'] == seen['close'] + 2 for u in late), late)

    def test_f3_an_unknown_that_failed_before_the_pause_stops_the_run(self):
        # failed (completed) before the pause mark, delivered after the ack: not in flight at link-down
        self.flap_row()
        self.flap_controller(before_pause=0.6)
        with self.assertRaisesRegex(OracleError, 'UNKNOWN_WRITE'):
            soak_load.run(self.config)
        last = jsonl(self.root / 'write-ledger.jsonl')[-1]
        self.assertEqual((last['state'], last['attributed'], 'reason' in last), ('UNKNOWN', None, False))
        pause = json.loads((self.root / 'fault-pause-E1.json').read_text())['pause_mono']
        self.assertLess(last['completed'], pause)

    def test_f3_an_unknown_before_any_window_stops_the_run(self):
        self.flap_row()
        self.toggle('fake-fail-set', 1.0, 5)
        with self.assertRaisesRegex(OracleError, 'UNKNOWN_WRITE'):
            soak_load.run(self.config)
        self.assertEqual(jsonl(self.root / 'write-ledger.jsonl')[-1]['attributed'], None)

    def test_f3_an_unknown_submitted_after_the_window_end_stops_the_run(self):
        self.flap_row(recovery_s=1)
        t, seen = self.flap_controller(after_close=(1.6, 3.0))  # failing PUTs from close + 1.6 s, past close + 1 s
        with self.assertRaisesRegex(OracleError, 'UNKNOWN_WRITE'):
            soak_load.run(self.config)
        last = jsonl(self.root / 'write-ledger.jsonl')[-1]
        self.assertEqual((last['state'], last['attributed'], 'reason' in last), ('UNKNOWN', None, False))
        self.assertGreater(last['submitted'], seen['close'] + 1)

    def test_f3_a_close_from_another_hosts_clock_is_refused(self):
        # M02's boot: a bound on another host's clock cannot reach the window (the binding refuses the file)
        self.flap_row()
        self.flap_controller(close_row=dict(episode='E1', role='volume', end_mono=time.monotonic(), boot_id='m02-boot'))
        with self.assertRaisesRegex(ValueError, 'binding differs'):
            soak_load.run(self.config)

    def test_f3_a_close_dated_after_its_observation_is_refused(self):
        self.flap_row()
        self.flap_controller(close_row=dict(episode='E1', role='volume', end_mono=time.monotonic() + 1000))
        with self.assertRaisesRegex(ValueError, 'dated after its observation'):
            soak_load.run(self.config)

    # ---- F3 late completions (r12 (b), team-lead 02:3xZ): a completion of a future the worker abandoned at its
    # deadline is evidence only; the op keeps its terminal state.
    def test_f3_a_late_ok_completion_is_recorded_and_never_upgrades_the_ledger(self):
        self.flap_row()
        t, seen = self.flap_controller(fail=0.6, fail_file='fake-deadline-set')
        summary = soak_load.run(self.config)
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        late = jsonl(self.root / 'late-completions.jsonl')
        self.assertTrue(late)
        ledger = jsonl(self.root / 'write-ledger.jsonl')
        acks = jsonl(self.root / 'ack-journal.jsonl')
        workers = json.loads((self.root / 'workers.json').read_text())
        for x in late:
            self.assertEqual((x['run_id'], x['episode'], x['action'], x['ok'], x['ledger_state'], x['quarantined']),
                             ('run-1', 'E1', 'set', True, 'UNKNOWN', True), x)
            self.assertEqual(x['worker_identity']['pid'], workers[str(x['worker'])]['pid'])
            rows = [r for r in ledger if r['op'] == x['op']]
            self.assertEqual([r['state'] for r in rows], ['UNKNOWN'], rows)  # never upgraded by the late ok
            self.assertFalse([a for a in acks if a['key'] == x['key'] and a['sequence'] == rows[0]['sequence']])
            self.assertGreaterEqual(x['received_mono'], rows[0]['completed'])

    def test_f3_a_late_completion_of_an_op_that_never_timed_out_stops_the_run(self):
        self.flap_row()
        self.toggle('fake-late-bogus', 1.0, 3)
        with self.assertRaisesRegex(RuntimeError, 'worker error'):
            soak_load.run(self.config)

    def test_only_f3_workers_get_the_late_completion_flag(self):
        for kind in ('control', 'volume_kill_io', 'client_kill_put', 'conn_kill', 'link_flap'):
            with self.subTest(kind=kind):
                self.setUp()
                if kind == 'link_flap':
                    self.flap_row()
                else:
                    self.row(kind)
                self.config['duration_s'] = 3
                soak_load.run(self.config)
                flags = [json.loads(p.read_text()) for p in self.root.glob('fake-late-flag-*')]
                self.assertEqual(flags, [True] * len(flags) if kind == 'link_flap' else [], kind)
                if kind == 'link_flap':
                    self.assertGreaterEqual(len(flags), 4)  # every load worker (and the audit client)
                self.assertEqual((self.root / 'late-completions.jsonl').exists(), kind == 'link_flap')

    def test_f3_a_row_without_its_window_bound_or_target_is_refused(self):
        for drop in ('recovery_s', 'netdev', 'rdma_device'):
            with self.subTest(drop=drop):
                self.flap_row()
                del self.config['fault_row'][drop]
                with self.assertRaisesRegex(ValueError, 'link_flap row needs'):
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

    def test_f2_on_an_http_only_load_kills_one_of_its_own_workers(self):
        # the rehearsal's load: kv_load over HTTP, no RDMA worker at all
        self.row('client_kill_put')
        self.config['pattern'] = dict(clients=2, transport='http', keys=4, distribution='uniform', object_bytes=64, rate_per_client=20,
                                      phases=[dict(kind='write_once'), dict(kind='mixed', duration_s=2, read_ratio=0.5)])
        self.config['native']['fake_delay_set'] = 0.5
        self.config['fault_row']['at_s'] = 2.5  # after write_once (4 keys, 2 clients, 0.5 s per PUT)
        self.config['duration_s'] = 6
        summary = soak_load.run(self.config)
        k = summary['kills'][0]
        workers = json.loads((self.root / 'workers.json').read_text())
        self.assertEqual((k['kind'], workers[str(k['worker'])]['transport']), ('client_kill_put', 'http'))
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
