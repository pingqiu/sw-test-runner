"""Stage 3 load rows through the real soak_load.run() with the fake native child (spawned processes) and a test
thread as the controller: the armed window, ACKED/REFUSED/UNKNOWN, F1/F2 quarantine, the ledger and the audit."""
import json
import threading
import time
import unittest
from pathlib import Path

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


class PressureRows(unittest.TestCase):
    """r26 F5 (plan v6) through the real soak_load.run() with the fake native child (spawned processes): window failures
    named and counted, unresolved ops never settled, poison, the reuse barrier, and the one-deadline teardown BEFORE
    the fresh audit and the terminal row. The reap gaps are shrunk (production: 20/3/2/2/3 s, test_soak_ownership)."""
    setUp_rows = Rows.setUp
    write, row, FLAP = Rows.write, Rows.row, Rows.FLAP
    serve = test_soak_load.TopologyPoller.serve

    def setUp(self):
        self.setUp_rows()
        saved = soak_load.REAP_GAPS_S
        soak_load.REAP_GAPS_S = (2, 0.5, 0.3, 0.3, 0.3)
        self.addCleanup(setattr, soak_load, 'REAP_GAPS_S', saved)
        self.row('mr_pressure', recovery_s=1, audit_s=3)
        self.config['pacing_s'] = 0.05
        self.config['duration_s'] = 6

    def controller(self, at=1.0, fail=0.6, files=('fake-fail',), then=(('fake-fail-set',), 0.6), before=None, text=''):
        """pause at `at`, then `files` present for `fail` s after the ack (GETs failing: PUTs still ACK, so their
        read-after-ACK GETs run), then `then` = (files, s) (None: nothing), then the close; before: (delay, files)
        written before the pause instead."""
        seen = {}

        def wait(path):  # bounded: a run that stopped early never publishes what this waits for
            end = time.monotonic() + 60
            while not path.exists():
                if time.monotonic() > end:
                    raise OSError('gave up waiting for ' + path.name)
                time.sleep(0.01)

        def run():
            try:
                wait(self.root / 'soak-load-start.json')
                if before:
                    time.sleep(before[0])
                    for f in before[1]:
                        (self.root / f).write_text('')
                time.sleep(at)
                seen['pause'] = time.monotonic()
                self.write('fault-pause-E1.json', dict(episode='E1', role='volume', pause_mono=seen['pause']))
                wait(self.root / 'fault-ack-E1.json')
                for f in files:
                    (self.root / f).write_text(text)  # the fake child's error text ('' = its default)
                time.sleep(fail)
                for f in files:
                    (self.root / f).unlink()
                for f in (then or ((), 0))[0]:
                    (self.root / f).write_text(text)
                time.sleep((then or ((), 0))[1])
                for f in (then or ((), 0))[0]:
                    (self.root / f).unlink()
                seen['close'] = time.monotonic()
                self.write('fault-close-E1.json', dict(episode='E1', role='volume', end_mono=seen['close']))
            except OSError:
                pass
        t = threading.Thread(target=run, daemon=True)
        t.start()
        return t, seen

    def own(self):
        return jsonl(self.root / 'ownership.jsonl')

    def exits(self):
        return {x['child']: x for x in self.own() if x['event'] == 'exit'}

    def assert_proven_teardown_before_the_audit(self, killed=()):
        own = self.own()
        td = [x for x in own if x['event'] == 'teardown']
        self.assertEqual((len(td), td[0]['gated'], td[0]['unproven']), (1, True, []), own)
        ex = self.exits()
        self.assertEqual(sorted(ex), ['0', '1', '2', '3', 'audit'])
        for k, x in ex.items():
            self.assertIsNotNone(x['exitcode'], x)
            self.assertEqual(x['killed'], k in killed, x)
        events = jsonl(self.root / 'op-events.jsonl')
        done = [e for e in events if e.get('event') == 'audit_done']
        self.assertTrue(done and done[0]['mono'] > max(ex[str(i)]['observed'] for i in range(4)))
        self.assertEqual(events[-1]['event'], 'quiescent')
        return ex

    def test_f5_window_failures_are_named_counted_and_the_teardown_precedes_the_audit(self):
        t, seen = self.controller()
        summary = soak_load.run(self.config)
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        unknown = [r for r in jsonl(self.root / 'write-ledger.jsonl') if r['state'] == 'UNKNOWN']
        self.assertTrue(unknown)
        for u in unknown:  # settled, counted, attributed with the M01 window receipt; never REFUSED or retried
            r = u['reason']
            self.assertEqual((u['attributed'], r['kind'], r['phase'], r['native_ok'], r['failed_delta'], r['start']),
                             ('E1', 'mr_pressure', 'complete', False, 1, seen['pause']))
        failed = [e for e in jsonl(self.root / 'op-events.jsonl') if e.get('state') == 'failed']
        self.assertTrue(failed)
        self.assertTrue(all(e['failed_delta'] == 1 and e['untouched'] is True and 'received' in e for e in failed), failed)
        self.assert_proven_teardown_before_the_audit()
        self.assertEqual(len(list(self.root.glob('fake-own-flag-*'))), 5)  # 4 load workers + the audit worker
        names = soak_load.evidence_names(self.config['fault_row'])  # what main() hashes into the summary
        for n in ('ownership.jsonl', 'late-completions.jsonl', 'write-ledger.jsonl', 'audit.jsonl'):
            self.assertIn(n, names)
            self.assertTrue((self.root / n).exists())

    def test_f5_a_timed_out_put_is_unresolved_the_worker_blocked_then_unblocked(self):
        t, _ = self.controller(fail=0.6, files=('fake-deadline-set',), then=None)
        summary = soak_load.run(self.config)
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        unknown = [r for r in jsonl(self.root / 'write-ledger.jsonl') if r['state'] == 'UNKNOWN']
        self.assertTrue(unknown and all(u['reason']['phase'] == 'wait' and u['reason']['failed_delta'] is None for u in unknown), unknown)
        self.assertTrue([x for x in self.own() if x['event'] == 'unblocked'])
        self.assertTrue(jsonl(self.root / 'late-completions.jsonl'))
        ops = [e for e in jsonl(self.root / 'op-events.jsonl') if e.get('phase') == 'wait']
        self.assertTrue(ops and all(e['state'] == 'UNKNOWN' and e['ok'] is False for e in ops))
        self.assert_proven_teardown_before_the_audit()

    def test_f5_a_raised_submit_poisons_its_worker_for_good(self):
        t, _ = self.controller(fail=0.4, files=('fake-submit-raise', 'fake-orphan'), then=None)
        summary = soak_load.run(self.config)  # a 'refused' (an op sent to a poisoned worker) would raise
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        unknown = [r for r in jsonl(self.root / 'write-ledger.jsonl') if r['state'] == 'UNKNOWN']
        self.assertTrue(unknown and all(u['reason']['phase'] == 'submit' for u in unknown), unknown)
        poisoned = {str(u['worker']) for u in unknown}
        ex = self.assert_proven_teardown_before_the_audit()
        self.assertEqual({k for k, x in ex.items() if x['poisoned']}, poisoned)
        self.assertTrue([x for x in self.own() if x['event'] == 'orphan'])
        later = [e for e in jsonl(self.root / 'op-events.jsonl') if 'action' in e and str(e['worker']) in poisoned
                 and e['dispatched'] > max(u['dispatched'] for u in unknown if str(u['worker']) == str(e['worker']))]
        self.assertEqual(later, [])  # no op after the poison

    def test_f5_a_failure_before_the_window_still_stops(self):
        t, _ = self.controller(at=2.0, files=(), then=None, before=(0.3, ('fake-fail-set',)))
        with self.assertRaisesRegex(OracleError, 'UNKNOWN_WRITE'):
            soak_load.run(self.config)
        (self.root / 'fault-ack-E1.json').write_text('{}')  # release the controller: the run stopped before any ack
        t.join(10)

    def test_f5_a_destructor_hang_is_killed_seen_gone_and_recorded(self):
        (self.root / 'fake-final-hang').write_text('')
        t, _ = self.controller(fail=0.3)
        began = time.monotonic()
        summary = soak_load.run(self.config)
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        ex = self.assert_proven_teardown_before_the_audit(killed=('0', '1', '2', '3', 'audit'))
        self.assertTrue(all(x['final'] and x['exitcode'] == -9 for x in ex.values()), ex)  # the final row was no exit proof
        self.assertLess(time.monotonic() - began, self.config['duration_s'] + self.config['quiescence_s'] + 3 + 30)

    def test_f5_children_never_seen_gone_leave_the_teardown_unproven_and_no_terminal_row(self):
        real = soak_load.reap

        def stuck(children, ids, kill_at, until, exits, kills, stopped, drain, **kw):
            live = real(children, ids, kill_at, until, exits, kills, stopped, drain, **kw)
            if '3' not in [str(k) for k in ids] and 3 not in ids:
                return live
            exits.pop(3, None)  # as if child 3 survived its kill: never observed gone
            return sorted(set(live) | {3}, key=str)
        soak_load.reap = stuck
        self.addCleanup(setattr, soak_load, 'reap', real)
        t, _ = self.controller(fail=0.3)
        with self.assertRaisesRegex(RuntimeError, 'teardown'):
            soak_load.run(self.config)
        t.join()
        events = jsonl(self.root / 'op-events.jsonl')
        self.assertNotIn('quiescent', [e.get('event') for e in events])  # no terminal claim on unproven state
        self.assertNotIn('audit_done', [e.get('event') for e in events])  # and no fresh audit
        td = [x for x in self.own() if x['event'] == 'teardown'][0]
        self.assertEqual((td['gated'], td['unproven']), (False, ['3']))

    def test_f5_a_final_after_six_seconds_is_not_killed(self):
        soak_load.REAP_GAPS_S = (9, 0.5, 0.3, 0.3, 0.3)
        (self.root / 'fake-final-delay').write_text('6')
        t, _ = self.controller(fail=0.3)
        summary = soak_load.run(self.config)
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        # every load worker's exit clean and none killed (the legacy join(5) would have); the audit child, stopped on
        # the shrunk test gaps (0.3 s) with the same 6 s delay, is the one killed
        self.assert_proven_teardown_before_the_audit(killed=('audit',))

    def bound(self):
        """The whole run's wall bound: the load, its quiescence, the reap schedule and a start-up margin; an audit wait
        not clamped to A (45 s ready, 35 s per GET) overruns it."""
        return self.config['duration_s'] + self.config['quiescence_s'] + sum(soak_load.REAP_GAPS_S) + 3 + 12

    def test_f5_an_audit_child_never_ready_is_clamped_killed_and_seen_gone(self):
        (self.root / 'fake-audit-hang').write_text('')
        t, _ = self.controller(fail=0.3)
        began = time.monotonic()
        summary = soak_load.run(self.config)
        self.assertLess(time.monotonic() - began, self.bound())
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        ex = self.assert_proven_teardown_before_the_audit(killed=('audit',))
        self.assertEqual(ex['audit']['exitcode'], -9)
        done = [e for e in jsonl(self.root / 'op-events.jsonl') if e.get('event') == 'audit_done']
        self.assertIs(done[0]['complete'], False)  # stopped at the clamped deadline A, never ready

    def test_f5_an_audit_get_that_never_answers_is_clamped_and_the_child_killed(self):
        (self.root / 'fake-audit-get-hang').write_text('')
        t, _ = self.controller(fail=0.3)
        began = time.monotonic()
        summary = soak_load.run(self.config)
        self.assertLess(time.monotonic() - began, self.bound())
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        ex = self.assert_proven_teardown_before_the_audit(killed=('audit',))
        self.assertEqual(ex['audit']['exitcode'], -9)
        done = [e for e in jsonl(self.root / 'op-events.jsonl') if e.get('event') == 'audit_done']
        self.assertIs(done[0]['complete'], False)

    # ---- r26 correction (source review of c0a826ae), through the shipping load
    def test_f5_a_pressure_error_matching_a_refused_class_is_never_refused(self):
        self.assertIn('No space left on device', self.config['fault_row']['refused'])  # the admitted literal
        t, _ = self.controller(fail=0.6, files=('fake-fail',), then=(('fake-fail-set',), 0.6), text='No space left on device')
        summary = soak_load.run(self.config)
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        ledger = jsonl(self.root / 'write-ledger.jsonl')
        self.assertEqual([r['op'] for r in ledger if r['state'] == 'REFUSED'], [])  # generic text certifies nothing
        unknown = [r for r in ledger if r['state'] == 'UNKNOWN']
        self.assertTrue(unknown and all('No space left on device' in (r['error'] or '') for r in unknown), unknown)
        gets = [e for e in jsonl(self.root / 'op-events.jsonl') if e.get('action') == 'get' and e['ok'] is False]
        self.assertTrue(gets and all(e.get('state') == 'failed' and not e.get('fault_error') for e in gets), gets)

    def test_f5_an_unproven_teardown_leaves_the_process_by_its_bound(self):
        """The shipping CLI (soak_load.main) in its own process: a child survives its kill (modelled: kill() on child 3
        is a no-op, so the real child stays live), the teardown is UNPROVEN, and the process exits non-zero at once
        instead of multiprocessing's unbounded exit-time join of that live child."""
        import os
        import subprocess
        import sys
        (self.root / 'fake-stop-ignore').write_text('')
        code = ("import json, sys, soak_load, test_soak_fake_child as f\n"
                "soak_load.child = f.child\n"
                "soak_load.REAP_GAPS_S = (2, 0.5, 0.3, 0.3, 0.3)\n"
                "real = soak_load.reap\n"
                "def reap(children, ids, *a, **kw):\n"
                "    if 3 in ids:\n"
                "        children[3]['process'].kill = lambda: None  # survives its kill (modelled)\n"
                "    return real(children, ids, *a, **kw)\n"
                "soak_load.reap = reap\n"
                "sys.argv = ['soak_load.py', '--config', sys.argv[1]]\n"
                "soak_load.main()\n")
        t, _ = self.controller(fail=0.3)
        began = time.monotonic()
        # the PROCESS exit is what is bounded: its output goes to files, since the live child it leaves behind still
        # holds inherited descriptors (a pipe would never reach EOF)
        with open(self.root / 'cli.out', 'w') as so, open(self.root / 'cli.err', 'w') as se:
            p = subprocess.Popen([sys.executable, '-c', code, json.dumps(self.config)], cwd=str(Path(soak_load.__file__).parent),
                                 stdout=so, stderr=se, stdin=subprocess.DEVNULL, env=dict(os.environ, PYTHONDONTWRITEBYTECODE='1'))
            try:
                rc = p.wait(timeout=120)
            except subprocess.TimeoutExpired:
                p.kill()
                rc = p.wait()
        elapsed = time.monotonic() - began
        err = (self.root / 'cli.err').read_text()
        t.join(10)
        try:
            self.assertEqual(rc, soak_load.TEARDOWN_EXIT, err[-2000:])
            td = [x for x in jsonl(self.root / 'ownership.jsonl') if x['event'] == 'teardown']
            self.assertEqual(td[0]['unproven'], ['3'])  # the record is the evidence file, written before the exit
            self.assertLess(elapsed, self.config['duration_s'] + self.config['quiescence_s'] + sum(soak_load.REAP_GAPS_S) + 3 + 15)
            events = jsonl(self.root / 'op-events.jsonl')
            self.assertNotIn('quiescent', [e.get('event') for e in events])
            self.assertNotIn('audit_done', [e.get('event') for e in events])
        finally:  # the live child the CLI left behind is this test's to stop: by its recorded pid, checked first
            pid = json.loads((self.root / 'workers.json').read_text())['3']['pid']
            try:
                cmdline = Path('/proc/%d/cmdline' % pid).read_bytes()
            except OSError:
                cmdline = b''
            if b'python' in cmdline:
                os.kill(pid, 9)

    def cli_unproven(self, sink):
        """The shipping CLI main() in its own process, child 3 surviving its kill (modelled: kill() a no-op, so a real
        child stays live), with stdout/stderr bound to `sink` ('file', or a pipe nobody reads that is already FULL).
        -> (rc or None if it never exited within 60 s, elapsed). Always stops what it started, by recorded pids."""
        import fcntl
        import os
        import subprocess
        import sys
        (self.root / 'fake-stop-ignore').write_text('')
        code = ("import json, sys, soak_load, test_soak_fake_child as f\n"
                "soak_load.child = f.child\n"
                "soak_load.REAP_GAPS_S = (2, 0.5, 0.3, 0.3, 0.3)\n"
                "real = soak_load.reap\n"
                "def reap(children, ids, *a, **kw):\n"
                "    if 3 in ids:\n"
                "        children[3]['process'].kill = lambda: None  # survives its kill (modelled)\n"
                "    return real(children, ids, *a, **kw)\n"
                "soak_load.reap = reap\n"
                "sys.argv = ['soak_load.py', '--config', sys.argv[1]]\n"
                "soak_load.main()\n")
        fds = []
        if sink == 'file':
            out = err = open(self.root / 'cli.err', 'w')
            fds.append(out)
        else:  # a pipe nobody reads, filled to capacity before the CLI starts
            r, w = os.pipe()
            fcntl.fcntl(w, fcntl.F_SETFL, fcntl.fcntl(w, fcntl.F_GETFL) | os.O_NONBLOCK)
            try:
                while True:
                    os.write(w, b'x' * 65536)
            except BlockingIOError:
                pass
            fcntl.fcntl(w, fcntl.F_SETFL, fcntl.fcntl(w, fcntl.F_GETFL) & ~os.O_NONBLOCK)
            out = err = w
            fds += [r, w]
        t, _ = self.controller(fail=0.3)
        began = time.monotonic()
        p = subprocess.Popen([sys.executable, '-c', code, json.dumps(self.config)], cwd=str(Path(soak_load.__file__).parent),
                             stdout=out, stderr=err, stdin=subprocess.DEVNULL, env=dict(os.environ, PYTHONDONTWRITEBYTECODE='1'))
        try:
            rc = p.wait(timeout=60)
        except subprocess.TimeoutExpired:
            rc = None
        elapsed = time.monotonic() - began
        if rc is None:  # blocked: stop it, by its own handle
            p.kill()
            p.wait()
        for f in fds:
            f.close() if hasattr(f, 'close') else os.close(f)
        t.join(10)
        try:  # the live child the CLI left behind: by its recorded pid, checked first
            pid = json.loads((self.root / 'workers.json').read_text())['3']['pid']
            if b'python' in Path('/proc/%d/cmdline' % pid).read_bytes():
                os.kill(pid, 9)
        except (OSError, KeyError, ValueError):
            pass
        return rc, elapsed

    def test_f5_a_full_stderr_or_stdout_pipe_never_blocks_the_hard_exit(self):
        rc, elapsed = self.cli_unproven('full-pipe')
        self.assertEqual(rc, soak_load.TEARDOWN_EXIT)  # None: blocked writing to the full pipe
        self.assertLess(elapsed, self.config['duration_s'] + self.config['quiescence_s'] + sum(soak_load.REAP_GAPS_S) + 3 + 15)
        td = [x for x in jsonl(self.root / 'ownership.jsonl') if x['event'] == 'teardown']
        self.assertEqual(td[0]['unproven'], ['3'])
        self.assertNotIn('quiescent', [e.get('event') for e in jsonl(self.root / 'op-events.jsonl')])

    def test_f5_the_hard_exit_with_an_empty_file_sink_still_exits_78(self):
        rc, _ = self.cli_unproven('file')
        self.assertEqual(rc, soak_load.TEARDOWN_EXIT)

    # ---- r27 (ruling option b): after the restore, fresh keys written and read back; one old-data read or its skip
    def recovery_run(self):
        t, seen = self.controller()
        summary = soak_load.run(self.config)
        t.join()
        self.assertEqual(summary['status'], 'LOAD_DONE')
        events = jsonl(self.root / 'op-events.jsonl')
        return seen, events, [e for e in events if 'action' in e]

    def test_f5_after_the_restore_fresh_keys_are_written_and_read_back(self):
        self.config['fault_row']['recovery_s'] = 4  # room for every fresh key: half the bound, 2 s
        seen, events, ops = self.recovery_run()
        fresh = [e for e in ops if e.get('recovery') == 'fresh']
        puts, gets = [e for e in fresh if e['action'] == 'set'], [e for e in fresh if e['action'] == 'get']
        keys = [soak_load.recovery_key(self.config['key_prefix'], n) for n in range(1, soak_load.RECOVERY_KEYS + 1)]
        self.assertEqual([e['key'] for e in sorted(puts, key=lambda e: int(e['op']))], keys)  # each once, in order
        self.assertEqual(sorted(e['key'] for e in gets), sorted(keys))  # each read back once
        closed = next(m for m in events if m.get('event') == 'fault_window' and m['phase'] == 'closed')
        for e in fresh:  # only after the restore: the close's end_mono (M01) and the load's own closed mark
            self.assertGreaterEqual(e['dispatched'], seen['close'], e)
            self.assertGreaterEqual(e['dispatched'], closed['mono'], e)
        self.assertTrue(all(e['state'] == 'ACKED' for e in puts), puts)
        acks = {a['key']: a for a in jsonl(self.root / 'ack-journal.jsonl')}
        for g in gets:  # exact: the ACKed generation's digest (the load's oracle raises on anything else)
            self.assertTrue(g['ok'] and g['sha256'] == acks[g['key']]['digest'] and g['submitted'] >= acks[g['key']]['completed'], g)
        # fresh: no op touched these keys before the restore; they go through the ledger and the fresh audit
        self.assertFalse({e['key'] for e in ops if e['dispatched'] < seen['close']} & set(keys))
        ledger = [r for r in jsonl(self.root / 'write-ledger.jsonl') if r['key'] in keys]
        self.assertEqual(sorted((r['key'], r['state']) for r in ledger), sorted((k, 'ACKED') for k in keys))
        self.assertTrue(set(keys) <= {a['key'] for a in jsonl(self.root / 'audit.jsonl')})
        self.assert_old_read(seen, events, ops)

    def assert_old_read(self, seen, events, ops):
        """The one recovery mark: the heal it used, and either one old-data read (a key ACKed before the pause, no op on
        it since, read once after the restore) or the recorded skip with no old read at all."""
        mark = [m for m in events if m.get('event') == 'recovery']
        self.assertEqual(len(mark), 1, mark)
        self.assertEqual(mark[0]['heal'], seen['close'])
        old = [e for e in ops if e.get('recovery') == 'old']
        if mark[0]['old_key'] is None:
            self.assertIn('none available', mark[0]['why'])
            self.assertEqual(old, [])
            return
        key = mark[0]['old_key']
        self.assertEqual([(e['action'], e['key']) for e in old], [('get', key)])
        self.assertGreaterEqual(old[0]['dispatched'], seen['close'])
        acked = [a for a in jsonl(self.root / 'ack-journal.jsonl') if a['key'] == key]
        self.assertTrue(acked and acked[0]['completed'] < seen['pause'])
        for e in ops:  # untouched since the pause until the old read
            if e['key'] == key and e is not old[0] and e['dispatched'] < old[0]['dispatched']:
                self.assertLess(e['received'], seen['pause'], e)

    def test_f5_fresh_puts_stop_at_half_the_recovery_bound(self):
        seen, events, ops = self.recovery_run()  # recovery_s 1: fresh PUTs only until the close + 0.5 s
        puts = [e for e in ops if e.get('recovery') == 'fresh' and e['action'] == 'set']
        self.assertTrue(puts)
        self.assertTrue(all(seen['close'] <= e['dispatched'] < seen['close'] + 0.5 for e in puts), puts)
        self.assertLess(len(puts), soak_load.RECOVERY_KEYS)
        self.assert_old_read(seen, events, ops)

    def test_f5_a_stale_loop_clock_never_dispatches_a_fresh_put_past_the_cutoff(self):
        """r27b red control (the source review's probe): the loop reads `now` before its pumps and the old-key decision;
        here that decision is slow (0.7 s, no old key), so the loop's `now` is past heal + recovery_s/2 (recovery_s 1:
        0.5 s) by the time the first fresh PUT is due. A dispatch judged on that stale clock went out late (op44 at
        heal + 0.937 s); judged on the clock read at the dispatch, it is refused."""
        self.addCleanup(setattr, soak_load, 'old_key', soak_load.old_key)
        probe = {}

        def slow(*args):
            time.sleep(0.7)
            probe['done'] = time.monotonic()
            return None
        soak_load.old_key = slow
        seen, events, ops = self.recovery_run()
        self.assertGreater(probe['done'], seen['close'] + 0.5)  # the probe really aged the loop's clock past the cutoff
        puts = [e for e in ops if e.get('recovery') == 'fresh' and e['action'] == 'set']
        self.assertEqual([e['op'] for e in puts if e['dispatched'] >= seen['close'] + 0.5], [], puts)

    def test_f5_without_an_old_key_the_skip_is_recorded_and_fresh_keys_still_run(self):
        """r26's case: no key ACKed before the window is left untouched (old_key finds none): the load records the skip,
        reads no old key, and the fresh keys still meet the floors on their own."""
        self.config['fault_row']['recovery_s'] = 4
        self.addCleanup(setattr, soak_load, 'old_key', soak_load.old_key)
        soak_load.old_key = lambda *a: None
        seen, events, ops = self.recovery_run()
        self.assert_old_read(seen, events, ops)
        mark = next(m for m in events if m.get('event') == 'recovery')
        self.assertEqual((mark['old_key'], mark['why']),
                         (None, 'none available (every ACKed key quarantined, busy or touched since the window opened)'))
        fresh = [e for e in ops if e.get('recovery') == 'fresh']
        self.assertEqual(sum(e['action'] == 'set' for e in fresh), soak_load.RECOVERY_KEYS)
        self.assertEqual(sum(e['action'] == 'get' for e in fresh), soak_load.RECOVERY_KEYS)

    def test_f5_a_recovery_key_already_written_stops_the_run(self):
        """The freshness guard: a recovery key that names a key the load already wrote (here the soak's own slots) is
        never written over or read as fresh; the run stops."""
        self.config['fault_row']['recovery_s'] = 4
        self.addCleanup(setattr, soak_load, 'recovery_key', soak_load.recovery_key)
        soak_load.recovery_key = lambda prefix, n: prefix + soak_load.hashlib.sha256(('soak-key-%d' % (n - 1)).encode()).hexdigest()
        t, _ = self.controller()
        with self.assertRaisesRegex(OracleError, 'recovery key not fresh'):
            soak_load.run(self.config)
        t.join()

    def test_only_f5_issues_recovery_ops(self):
        Rows.flap_row(self)  # F3: the same M01 close mark and window bound, no recovery ops
        t, _ = Rows.flap_controller(self, fail=0.3)
        soak_load.run(self.config)
        t.join()
        events = jsonl(self.root / 'op-events.jsonl')
        self.assertFalse([e for e in events if 'recovery' in e or e.get('event') == 'recovery'])
        fresh = {soak_load.recovery_key(self.config['key_prefix'], n) for n in range(1, soak_load.RECOVERY_KEYS + 1)}
        self.assertFalse(fresh & {r['key'] for r in jsonl(self.root / 'write-ledger.jsonl')})

    def test_only_f5_children_get_the_ownership_flag(self):
        Rows.flap_row(self)
        t, _ = Rows.flap_controller(self, fail=0.3)
        soak_load.run(self.config)
        t.join()
        self.assertEqual(list(self.root.glob('fake-own-flag-*')), [])


class OldKey(unittest.TestCase):
    """r27 F5: the old-data key is ACKed (in latest), not quarantined, idle, and last touched strictly before the pressure
    window's start; a key with no recorded op is never assumed untouched."""

    def test_the_old_key_rules(self):
        latest, start = dict.fromkeys('abcde', 1), 10.0
        touched = dict(a=5.0, b=9.0, c=10.0, d=4.0, x=1.0)  # e: no recorded op; x: not ACKed
        self.assertEqual(soak_load.old_key(latest, {'a'}, {'d'}, touched, start), 'b')
        for name, kw, want in (('quarantined', dict(quarantined={'a', 'b', 'd'}), None),
                               ('busy', dict(busy={'a', 'b', 'd'}), None),
                               ('touched at the start', dict(touched=dict(touched, a=10.0, b=10.0, d=12.0)), None),
                               ('touched after the start', dict(touched=dict(touched, a=11.0, b=10.5)), 'd'),
                               ('the first in sorted order', dict(), 'a')):
            with self.subTest(case=name):
                args = dict(latest=latest, quarantined=set(), busy=set(), touched=touched, start=start)
                args.update(kw)
                self.assertEqual(soak_load.old_key(**args), want)

    def test_the_fresh_namespace_is_its_own(self):
        keys = [soak_load.recovery_key('k@', n) for n in range(1, soak_load.RECOVERY_KEYS + 1)]
        self.assertEqual(len(set(keys)), soak_load.RECOVERY_KEYS)
        p = soak_load.Pattern(dict(keys=512, distribution='uniform', phases=[]), None, 'k@')
        others = {p.key(i) for i in range(512)} | {'k@' + soak_load.hashlib.sha256(('%s-%d' % (s, i)).encode()).hexdigest()
                                                  for s in ('soak-key', 'f2-key') for i in range(4096)}
        self.assertFalse(set(keys) & others)


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
