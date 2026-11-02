"""Labelled SYNTHETIC evidence for the soak verdict: one complete consistent
run, then negatives. Each negative is a PLAUSIBLE fake PASS (codex01's method):
evidence that looks whole but lies about one criterion, and must fail exactly it."""
import copy
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import soak_assess
from soak_native_worker import payload
from soak_stats import boot_id

ROT = list(range(20, 31))  # 11 rotated volumes
HERE = Path(__file__).resolve().parent


def digest(seed, length):
    return hashlib.sha256(payload(seed, length)).hexdigest()


def evidence():
    acks, events, n, op = [], [], [0], [0]
    def ident(label):
        op[0] += 1
        return dict(op=str(op[0]), label=label)
    def ack(key, writer, t, vid=3):
        n[0] += 1
        seed = 'seed-%d' % n[0]
        acks.append(dict(key=key, writer=writer, seed=seed, submitted=t - 0.05, completed=t, sequence=n[0], length=64,
                         digest=digest(seed, 64), fids=['%d,ab' % vid]))
        events.append(dict(ident('s%d' % n[0]), worker=writer, action='set', key=key, dispatched=t - 0.06, submitted=t - 0.05,
                           completed=t, ok=True))
        return acks[-1]
    def get(label, key, t, done, a, **kw):
        events.append(dict(ident(label), worker=kw.pop('worker', 1), action='get', key=key, dispatched=t - 0.01, submitted=t,
                           completed=done, sequence=a['sequence'], **kw))
    for w, t in enumerate((1010, 1020, 1030, 1040)):  # one key written by all four workers
        ack('shared', w, t)
    a = ack('k2', 1, 1050)
    get('g1', 'k2', 1060, 1060.1, a, ok=True, length=64, sha256=a['digest'])
    m = ack('m', 2, 1100, vid=20)  # its volume rotates: unwritable at 1150, gone at 1170
    get('g2', 'm', 1160, 1160.1, m, ok=False, miss=True, fids=m['fids'], error='DC batch push-read response status 1')
    get('g9', 'k2', 1478, 1490, a, ok=False, fault_error=True, episode='E1', error='operation timed out')
    kill = dict(event='client_kill', kind='client_kill', worker=1, started=2320, killed=2321, finished=2330,
                witness=dict(event='submitted', action='get', pid=11),
                replacement=dict(pid=77, starttime=1))
    events.append(kill)
    events.append(dict(event='body_end', mono=2800))
    unw = {v: 1150 + (v - 20) * 100 for v in ROT}
    polls = []
    for t in range(1000, 2862, 2):
        present = {1, 2, 3} | {v for v in ROT if t < unw[v] + 20} | ({40} if t > 1920 else set())
        writables = {3} | {v for v in ROT if t < unw[v]}
        polls.append(dict(req=t, done=t + 0.05, ok=True, writables=sorted(writables), present=sorted(present)))
    faults = [dict(episode='E1', role='volume', dispatch_mono=1480, ready_mono=1500, receipt=dict(Kill=dict(PreKillMono=1480))),
              dict(episode='E2', role='master', dispatch_mono=1900, ready_mono=1915, receipt=dict(Kill=dict(PreKillMono=1900)))]
    ack('after-master', 0, 1930)
    kill['witness']['operation_id'] = ident('kill')['op']  # the highest op id: killed, so never settled by an event
    g9 = next(e for e in events if e.get('label') == 'g9')
    events.append(dict(event='quiescent', mono=2861, dispatched=op[0]))
    events.sort(key=lambda e: 0 if 'action' in e else 1)  # marks after ops, quiescent last (order of the real log)
    boot = boot_id()
    files = {'soak-load-start.json': dict(boot_id=boot, mono=1000), 'faults.json': faults,
             'fault-ack-E1.json': dict(run_id='run-1', boot_id=boot, episode='E1', role='volume', observed_mono=1479,
                                       pending_gets=[dict(op=g9['op'], submitted=g9['dispatched'])]),
             'm02-collect.json': dict(status='SOAK_COLLECTED', collection='lmcache', dat_vids=[],
                                      rotated=[dict(vid=v, line='rotated non-replicated cache Volume %d' % v) for v in ROT]),
             'workers.json': {str(i): dict(pid=10 + i, starttime=1, transport='http' if i == 3 else 'dc') for i in range(4)}}
    stats = {}
    for pid, dc in ((10, True), (11, True), (12, True), (13, False), (77, True)):
        loader = dict(transport='rdma', http_fallback_allowed=False, direct_dc_reads=5, direct_rc_reads=0, copied_reads=0) if dc else \
            dict(transport='http', http_fallback_allowed=True, direct_dc_reads=0, direct_rc_reads=0, copied_reads=5)
        stats['native-stats-%d-1.jsonl' % pid] = [dict(status='OBSERVED', stats=dict(loader=loader))]
    return dict(files=files, acks=acks, events=events, polls=polls, stats=stats)


def config(root):
    return dict(schema=soak_assess.SCHEMA, root=str(root), run_id='run-1', boot_id=boot_id(), duration_s=1800,
                quiescence_s=60, client_kill_at_s=1320, collection='lmcache')


def write(root, ev):
    for name, body in ev['files'].items():
        (root / name).write_text(json.dumps(body))
    for name, key in (('ack-journal.jsonl', 'acks'), ('op-events.jsonl', 'events'), ('topology-polls.jsonl', 'polls')):
        (root / name).write_text(''.join(json.dumps(r) + '\n' for r in ev[key]))
    for name, rs in ev['stats'].items():
        (root / name).write_text(''.join(json.dumps(r) + '\n' for r in rs))


class SoakAssess(unittest.TestCase):
    def run_on(self, ev):
        root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, root)
        write(root, ev)
        return soak_assess.assess(config(root))

    def failing(self, row):
        return sorted(k for k, v in row['criteria'].items() if v['outcome'] != 'PASS')

    def test_the_complete_synthetic_run_passes_every_criterion(self):
        row = self.run_on(evidence())
        self.assertEqual((row['status'], self.failing(row), row['resources']), ('PASS', [], 'NOT_OBSERVED'), row)
        self.assertEqual(sorted(row['criteria']), sorted(soak_assess.MANDATORY))
        self.assertEqual([k['episode'] for k in row['k7']], ['E1', 'E2'])

    def mutate(self, change, rule):
        ev = evidence()
        change(ev)
        row = self.run_on(ev)
        self.assertEqual((row['status'], self.failing(row)), ('FAIL', [rule]), row['criteria'])

    def refused(self, change):
        ev = evidence()
        change(ev)
        with self.assertRaises(ValueError):
            self.run_on(ev)

    def ev(self, ev, label):
        return next(e for e in ev['events'] if e.get('label') == label)

    def add_get(self, ev, **kw):  # a new, properly settled op id, so only the rule under test can refuse it
        q = ev['events'][-1]
        q['dispatched'] += 1
        ev['events'].insert(-3, dict(dict(op=str(q['dispatched']), worker=0, action='get', key='k2', dispatched=1200, submitted=1200.01,
                                          completed=1200.1, sequence=5), **kw))

    # codex01's seven demonstrated false PASSes, each now RED on exactly its rule.
    def test_every_native_stats_file_removed(self):
        self.mutate(lambda ev: ev['stats'].clear(), 'routes')

    def test_an_uncredited_permission_denied_get(self):
        self.mutate(lambda ev: self.add_get(ev, ok=False, error='permission denied'), 'get_outcomes')

    def test_rc_reads_on_a_dc_worker(self):
        self.mutate(lambda ev: ev['stats']['native-stats-10-1.jsonl'][0]['stats']['loader'].update(direct_rc_reads=500), 'routes')

    def test_a_vid_reused_after_the_miss_tail(self):
        def ch(ev):
            p = next(p for p in ev['polls'] if p['req'] == 1200)
            p['present'] = sorted(set(p['present']) | {20})
        self.mutate(ch, 'misses')

    def test_a_foreign_fault_ack(self):
        self.mutate(lambda ev: ev['files']['fault-ack-E1.json'].update(run_id='other', role='master'), 'fault_errors')

    def test_a_post_p_get_borrowing_an_acked_op_id(self):
        self.mutate(lambda ev: self.ev(ev, 'g9').update(submitted=1481), 'fault_errors')

    def test_a_set_kill_witness(self):
        self.mutate(lambda ev: next(e for e in ev['events'] if e.get('event') == 'client_kill')['witness'].update(action='set'), 'client_kill')

    def test_a_45_s_replacement(self):
        self.mutate(lambda ev: next(e for e in ev['events'] if e.get('event') == 'client_kill').update(finished=2366), 'client_kill')

    # Further plausible fakes, one per criterion.
    def test_wrong_bytes(self):
        self.mutate(lambda ev: self.ev(ev, 'g1').update(sha256='0' * 64), 'wrong_reads')

    def test_failed_set(self):
        def ch(ev):
            s = next(e for e in ev['events'] if e.get('key') == 'after-master')
            s['ok'] = False
            ev['acks'].remove(next(a for a in ev['acks'] if a['key'] == 'after-master'))
        self.mutate(ch, 'failed_sets')

    def test_an_ack_row_without_its_set_event(self):
        self.mutate(lambda ev: self.ev(ev, 's1').update(worker=3), 'failed_sets')

    def test_a_dispatched_op_never_settled(self):
        self.mutate(lambda ev: ev['events'][-1].update(dispatched=ev['events'][-1]['dispatched'] + 1), 'terminal')

    def test_an_op_id_settled_twice(self):
        self.mutate(lambda ev: self.ev(ev, 'g1').update(op=self.ev(ev, 'g2')['op']), 'terminal')

    def test_no_quiescent_row(self):
        self.mutate(lambda ev: ev['events'].pop(), 'terminal')

    def test_quiescence_cut_short(self):
        self.mutate(lambda ev: ev['events'][-1].update(mono=2850), 'terminal')

    def test_miss_before_the_volume_left_the_write_set(self):
        self.mutate(lambda ev: self.ev(ev, 'g2').update(submitted=1140, completed=1140.1, dispatched=1139.9), 'misses')

    def test_miss_on_a_volume_writable_again_before_it_vanished(self):
        def ch(ev):
            p = next(p for p in ev['polls'] if p['req'] == 1162)
            p['writables'] = sorted(set(p['writables']) | {20})
        self.mutate(ch, 'misses')

    def test_a_fresh_candidate_after_an_earlier_return_to_writable_still_excuses(self):
        ev = evidence()  # writable again at 1156, unwritable again at 1158, miss at 1160: a fresh candidate
        p = next(p for p in ev['polls'] if p['req'] == 1156)
        p['writables'] = sorted(set(p['writables']) | {20})
        row = self.run_on(ev)
        self.assertEqual((row['status'], self.failing(row)), ('PASS', []), row['criteria'])

    def test_miss_with_a_failed_poll_in_its_window(self):
        def ch(ev):
            p = next(p for p in ev['polls'] if p['req'] == 1162)
            p.update(ok=False)
            del p['present'], p['writables']
        self.mutate(ch, 'misses')

    def test_miss_with_a_poll_gap(self):
        self.mutate(lambda ev: ev.update(polls=[p for p in ev['polls'] if not 1150 < p['req'] < 1160]), 'misses')

    def test_miss_without_the_m02_rotation_line(self):
        def ch(ev):
            ev['files']['m02-collect.json']['rotated'] = [r for r in ev['files']['m02-collect.json']['rotated'] if r['vid'] != 20] + [dict(vid=31, line='x')]
        self.mutate(ch, 'misses')

    def test_miss_of_a_stale_version(self):
        def ch(ev):
            m = next(a for a in ev['acks'] if a['key'] == 'm')
            ev['acks'].append(dict(m, seed='newer', sequence=99, submitted=1104, completed=1105, digest=digest('newer', 64), writer=0))
            q = ev['events'][-1]
            q['dispatched'] += 1
            ev['events'].insert(0, dict(op=str(q['dispatched']), worker=0, action='set', key='m', dispatched=1103.9, submitted=1104,
                                        completed=1105, ok=True))
        self.mutate(ch, 'misses')

    def test_a_miss_without_the_miss_error(self):
        self.mutate(lambda ev: self.ev(ev, 'g2').update(error='permission denied'), 'get_outcomes')

    def test_fault_error_outside_p_plus_60(self):
        self.mutate(lambda ev: self.ev(ev, 'g9').update(completed=1541), 'fault_errors')

    def test_fault_error_not_acknowledged_pending(self):
        self.mutate(lambda ev: ev['files']['fault-ack-E1.json'].update(pending_gets=[]), 'fault_errors')

    def test_fault_error_with_a_different_dispatch(self):
        self.mutate(lambda ev: self.ev(ev, 'g9').update(dispatched=1477), 'fault_errors')

    def test_non_transient_fault_error(self):
        self.mutate(lambda ev: self.ev(ev, 'g9').update(error='permission denied'), 'fault_errors')

    def test_fault_error_of_an_episode_without_a_kill(self):
        self.mutate(lambda ev: ev['files']['faults.json'][0]['receipt']['Kill'].update(PreKillMono=0), 'fault_errors')

    def test_missing_body_end(self):
        self.mutate(lambda ev: ev['events'].remove(next(e for e in ev['events'] if e.get('event') == 'body_end')), 'body')

    def test_an_op_dispatched_after_body_end(self):
        def ch(ev):
            self.add_get(ev, ok=True, length=64, sha256=next(a for a in ev['acks'] if a['key'] == 'k2')['digest'], sequence=5)
            g = next(e for e in ev['events'] if e.get('dispatched') == 1200)
            g.update(dispatched=2801, submitted=2801.1, completed=2801.2)
        self.mutate(ch, 'body')

    def test_no_client_kill(self):
        def ch(ev):
            ev['events'].remove(next(e for e in ev['events'] if e.get('event') == 'client_kill'))
            del ev['stats']['native-stats-77-1.jsonl']  # no kill, so no replacement worker
            ev['events'][-1]['dispatched'] -= 1  # nor its witness op
        self.mutate(ch, 'client_kill')

    def test_a_kill_witness_from_another_process(self):
        self.mutate(lambda ev: next(e for e in ev['events'] if e.get('event') == 'client_kill')['witness'].update(pid=13), 'client_kill')

    def test_too_few_rotations(self):
        self.mutate(lambda ev: ev['files']['m02-collect.json'].update(dat_vids=[21, 22]), 'rotation')

    def test_no_master_fault(self):
        self.mutate(lambda ev: ev['files']['faults.json'].pop(), 'max_vid_advanced')

    def test_dc_worker_with_fallback(self):
        self.mutate(lambda ev: ev['stats']['native-stats-10-1.jsonl'][0]['stats']['loader'].update(http_fallback_allowed=True), 'routes')

    def test_replacement_series_missing(self):
        self.mutate(lambda ev: ev['stats'].pop('native-stats-77-1.jsonl'), 'routes')

    def test_a_foreign_stats_series(self):
        self.mutate(lambda ev: ev['stats'].update({'native-stats-99-1.jsonl': ev['stats']['native-stats-10-1.jsonl']}), 'routes')

    def test_http_worker_without_copied_reads(self):
        self.mutate(lambda ev: ev['stats']['native-stats-13-1.jsonl'][0]['stats']['loader'].update(copied_reads=0), 'routes')

    def test_missing_worker_coverage(self):
        def ch(ev):  # the fourth writer of the shared key becomes worker 0, consistently in journal and events
            for a in ev['acks']:
                a['writer'] = 0 if a['writer'] == 3 else a['writer']
            for e in ev['events']:
                if e.get('action') == 'set' and e['worker'] == 3:
                    e['worker'] = 0
        self.mutate(ch, 'coverage')

    # Malformed or foreign evidence is refused outright (main() turns it into a FAIL row).
    def test_foreign_m02(self):
        self.refused(lambda ev: ev['files']['m02-collect.json'].update(collection='other'))

    def test_foreign_boot_witness(self):
        self.refused(lambda ev: ev['files']['soak-load-start.json'].update(boot_id='other'))

    def test_workers_not_three_dc_one_http(self):
        self.refused(lambda ev: ev['files']['workers.json']['3'].update(transport='dc'))

    def test_a_mistyped_clock(self):
        self.refused(lambda ev: self.ev(ev, 'g1').update(completed='1060'))

    def test_an_unknown_event_kind(self):
        self.refused(lambda ev: ev['events'].insert(-1, dict(event='surprise')))


class StrictReader(unittest.TestCase):
    """The one evidence reader: no-follow, regular, bounded, strict JSON."""
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root)
        write(self.root, evidence())
        self.config = config(self.root)

    def refused(self):
        with self.assertRaises(ValueError):
            soak_assess.assess(self.config)

    def test_duplicate_key(self):
        p = self.root / 'op-events.jsonl'  # wrong bytes first, the right digest second: a last-value parser PASSes
        rows = p.read_text().splitlines(True)
        i = next(i for i, r in enumerate(rows) if '"g1"' in r)
        rows[i] = rows[i].replace('"sha256": ', '"sha256": "%s", "sha256": ' % ('0' * 64), 1)
        p.write_text(''.join(rows))
        self.refused()

    def test_nan(self):
        p = self.root / 'topology-polls.jsonl'
        p.write_text(p.read_text().replace('"done": 1000.05', '"done": NaN', 1))
        self.refused()

    def test_two_values_on_one_line(self):
        p = self.root / 'ack-journal.jsonl'
        p.write_text(p.read_text().replace('\n', ' {}\n', 1))
        self.refused()

    def test_torn_last_line(self):
        p = self.root / 'op-events.jsonl'
        p.write_text(p.read_text()[:-1])
        self.refused()

    def test_symlinked_evidence(self):
        real = self.root / 'real.json'
        (self.root / 'workers.json').rename(real)
        os.symlink(real, self.root / 'workers.json')
        self.refused()

    def test_line_over_bound(self):
        p = self.root / 'ack-journal.jsonl'
        p.write_text(p.read_text() + json.dumps(dict(pad='x' * (65 << 10))) + '\n')
        self.refused()


class AdmittedArgv(unittest.TestCase):
    """The literal admitted launch form: python -I <helper> --config, from a foreign cwd."""
    def test_the_assessor_runs_isolated_and_passes_the_complete_run(self):
        root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, root)
        write(root, evidence())
        out = subprocess.run([sys.executable, '-I', str(HERE / 'soak_assess.py'), '--config', json.dumps(config(root))],
                             cwd=root, capture_output=True, text=True, timeout=120)
        self.assertEqual(out.returncode, 0, out.stderr)
        row = json.loads(out.stdout)
        self.assertEqual((row['status'], row['binding']), ('PASS', dict(schema=soak_assess.SCHEMA, run_id='run-1', boot_id=boot_id(), root=str(root))), row)

    def test_the_load_imports_isolated(self):
        out = subprocess.run([sys.executable, '-I', str(HERE / 'soak_load.py'), '--help'], cwd=tempfile.gettempdir(),
                             capture_output=True, text=True, timeout=60)
        self.assertEqual(out.returncode, 0, out.stderr)


class RealWorkerFiles(unittest.TestCase):
    """The assessor reads the files the REAL soak_load writes (fake native child,
    no fault): the replay-based criteria must PASS on them. Routes and rotation
    can't (no native stats, nothing rotates), which proves only the format."""

    def test_assessor_reads_the_workers_own_evidence(self):
        import test_soak_load
        import soak_load
        case = test_soak_load.FaultWindowWorker('test_a_future_pause_is_refused')
        case.setUp()
        try:
            case.config['native'] = dict(fake_delay_get=0.05, fake_delay_set=0.02)
            soak_load.run(case.config)
            root = case.root
            (root / 'faults.json').write_text('[]')
            (root / 'm02-collect.json').write_text(json.dumps(dict(status='SOAK_COLLECTED', collection='lmcache', dat_vids=[], rotated=[])))
            row = soak_assess.assess(dict(config(root), run_id=case.config['smoke']['run_id'], duration_s=case.config['duration_s'],
                                          quiescence_s=case.config['quiescence_s'], client_kill_at_s=10 ** 6))
        finally:
            case.doCleanups()
        c = row['criteria']
        self.assertEqual({k: c[k]['outcome'] for k in ('wrong_reads', 'failed_sets', 'get_outcomes', 'terminal', 'fault_errors', 'misses', 'body')},
                         dict.fromkeys(('wrong_reads', 'failed_sets', 'get_outcomes', 'terminal', 'fault_errors', 'misses', 'body'), 'PASS'), c)
        self.assertEqual(row['status'], 'FAIL')  # routes/rotation/client kill: not produced by this fake run


if __name__ == '__main__':
    unittest.main()
