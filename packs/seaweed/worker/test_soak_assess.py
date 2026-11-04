"""Labelled SYNTHETIC evidence for the soak verdict: one complete consistent
run, then negatives. Each negative is a PLAUSIBLE fake PASS (codex01's method):
evidence that looks whole but lies about one criterion, and must fail exactly it."""
import copy
import datetime
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
M02_BOOT = 'm02-boot'
EPOCH = 1793610000.0  # M02 wall clock at M01 mono 0 in the fixture: offset M02 epoch minus M01 mono


def stamp(mono, offset=EPOCH):
    t = datetime.datetime.fromtimestamp(mono + offset, datetime.timezone.utc)
    return t.strftime('%m%d %H:%M:%S.%f')


def line(kind, vid, mono, gen=0, offset=EPOCH):
    text = {'birth': 'Created Volume %d on', 'unwritable': 'Volume %d becomes unwritable', 'start': 'retiring before seal: cache Volume %d collection=lmcache',
            'success': 'rotated non-replicated cache Volume %d collection=lmcache'}[kind]
    raw = stamp(mono, offset) + ' ' + text % vid
    return dict(kind=kind, vid=vid, stamp=stamp(mono, offset), gen=gen, line=raw, sha256=hashlib.sha256(raw.encode()).hexdigest())


def sample(t1, t4, entry_mono, residence, offset=EPOCH):
    return dict(boot_id=M02_BOOT, run_id='run-1', t1=t1, t4=t4, entry_epoch=entry_mono + offset, exit_epoch=entry_mono + residence + offset,
                entry_mono=5000.0 + entry_mono, exit_mono=5000.0 + entry_mono + residence)
HERE = Path(__file__).resolve().parent


class MissVersion:
    def __init__(self, fids):
        self.fids = fids


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
    m = ack('m', 2, 1100, vid=20)  # its volume rotates: unwritable at 1150, gone at 1170, success line S at 1175
    get('g1m', 'm', 1110, 1110.1, m, ok=True, length=64, sha256=m['digest'])  # C5: this version read exactly before S
    get('g2', 'm', 1180, 1180.1, m, ok=False, miss=True, fids=m['fids'], error='DC batch push-read response status 1')
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
                                      rotated=[dict(vid=v, line='rotated non-replicated cache Volume %d' % v) for v in ROT],
                                      events=[e for v in ROT for e in (line('birth', v, 995), line('unwritable', v, unw[v]), line('success', v, unw[v] + 25))]),
             # S1 wraps a 10 s setup: paired capture removes its residence from the width.
             'soak-clock-s1.json': sample(980, 990.2, 980.1, 10), 'soak-clock-s2.json': sample(2870, 2870.2, 2870.1, 0),
             'workers.json': {str(i): dict(pid=10 + i, starttime=1, transport='http' if i == 3 else 'dc') for i in range(4)}}
    stats = {}
    for pid, dc in ((10, True), (11, True), (12, True), (13, False), (77, True)):
        loader = dict(transport='rdma', http_fallback_allowed=False, direct_dc_reads=5, direct_rc_reads=0, copied_reads=0) if dc else \
            dict(transport='http', http_fallback_allowed=True, direct_dc_reads=0, direct_rc_reads=0, copied_reads=5)
        stats['native-stats-%d-1.jsonl' % pid] = [dict(status='OBSERVED', stats=dict(loader=loader))]
    return dict(files=files, acks=acks, events=events, polls=polls, stats=stats)


def config(root):
    return dict(schema=soak_assess.SCHEMA, root=str(root), run_id='run-1', boot_id=boot_id(), m02_boot_id=M02_BOOT, duration_s=1800,
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

    def collected(self, ev):
        return ev['files']['m02-collect.json']

    def passes(self, change):
        ev = evidence()
        change(ev)
        row = self.run_on(ev)
        self.assertEqual((row['status'], self.failing(row)), ('PASS', []), row['criteria'])

    def test_a_later_unwritable_line_after_a_return_to_writable_still_excuses(self):
        def ch(ev):  # writable again at 1156, a new unwritable line at 1157, miss at 1160
            p = next(p for p in ev['polls'] if p['req'] == 1156)
            p['writables'] = sorted(set(p['writables']) | {20})
            self.collected(ev)['events'].append(line('unwritable', 20, 1157))
        self.passes(ch)

    def test_a_failed_poll_is_no_evidence_either_way(self):
        def ch(ev):
            p = next(p for p in ev['polls'] if p['req'] == 1162)
            p.update(ok=False)
            del p['present'], p['writables']
        self.passes(ch)

    def test_a_poll_gap_alone_does_not_refuse_under_the_clock_rule(self):
        self.passes(lambda ev: ev.update(polls=[p for p in ev['polls'] if not 1150 < p['req'] < 1160]))

    def test_no_success_line(self):
        def ch(ev):
            c = self.collected(ev)
            c['events'] = [e for e in c['events'] if not (e['vid'] == 20 and e['kind'] == 'success')]
        self.mutate(ch, 'misses')

    def test_an_event_after_the_success(self):
        self.mutate(lambda ev: self.collected(ev)['events'].append(line('unwritable', 20, 1190)), 'misses')

    def test_vid_reuse_by_a_birth_after_the_success(self):
        self.mutate(lambda ev: self.collected(ev)['events'].append(line('birth', 20, 1400)), 'misses')

    def test_a_generation_out_of_time_order(self):
        def ch(ev):
            c = self.collected(ev)
            for e in c['events']:
                if e['vid'] == 20:
                    e['gen'] = 1 if e['kind'] == 'unwritable' else 0
        self.mutate(ch, 'misses')

    def test_a_line_in_two_sources_counts_once(self):
        def ch(ev):
            c = self.collected(ev)
            c['events'] += [dict(e) for e in c['events'] if e['vid'] == 20]
        self.passes(ch)

    def test_a_differing_duplicate_is_refused(self):
        def ch(ev):
            c = self.collected(ev)
            e = next(e for e in c['events'] if e['vid'] == 20 and e['kind'] == 'unwritable')
            c['events'].append(dict(e, stamp=stamp(1149)))
        self.mutate(ch, 'misses')

    def test_success_with_the_glog_tail_lost_only_needs_one_source(self):  # volume 21's shape in r5
        self.passes(lambda ev: self.collected(ev)['events'].sort(key=lambda e: e['vid']))

    def test_a_log_date_outside_the_samples(self):
        def ch(ev):
            c = self.collected(ev)
            for e in c['events']:
                if e['vid'] == 20 and e['kind'] == 'unwritable':
                    e['stamp'] = '0101 00:00:00.000000'
        self.mutate(ch, 'misses')

    def test_year_crossover_has_one_instant(self):
        self.assertEqual(soak_assess.log_instants('0101 00:00:01.000000', 1798761600.0 - 60, 1798761600.0 + 60), [1798761601.0])

    def test_same_year_at_both_samples_is_one_instant_not_ambiguity(self):
        self.assertEqual(len(soak_assess.log_instants(stamp(1000), EPOCH + 900, EPOCH + 2900)), 1)

    def test_distinct_candidate_instants_are_ambiguous(self):  # a window over a year wide: not a real run
        begin = datetime.datetime(2026, 1, 1, tzinfo=datetime.timezone.utc).timestamp()
        end = datetime.datetime(2027, 6, 1, tzinfo=datetime.timezone.utc).timestamp()
        self.assertEqual(len(soak_assess.log_instants('0301 00:00:00.000000', begin, end)), 2)

    def test_a_failed_reply_is_not_absence(self):
        def ch(ev):
            for p in ev['polls']:  # the only absent replies inside miss+25 s fail, so absence is first proven too late
                if 1170 <= p['req'] <= 1206:
                    p.update(ok=False)
                    del p['present'], p['writables']
        self.mutate(ch, 'misses')

    def test_absence_too_late(self):
        def ch(ev):
            for p in ev['polls']:
                if p['req'] <= 1190 and p['req'] > 1150:
                    p['present'] = sorted(set(p['present']) | {20})
        self.mutate(ch, 'misses')

    def test_reappearance_after_absence(self):
        def ch(ev):
            p = next(p for p in ev['polls'] if p['req'] == 1300)
            p['present'] = sorted(set(p['present']) | {20})
        self.mutate(ch, 'misses')

    def test_a_later_failed_poll_after_absence_does_not_refuse(self):
        def ch(ev):
            p = next(p for p in ev['polls'] if p['req'] == 1902)
            p.update(ok=False)
            del p['present'], p['writables']
        self.passes(ch)

    # Clock evidence (section 2): any of these leaves every miss unexcused.
    def test_s2_missing(self):
        self.mutate(lambda ev: ev['files'].pop('soak-clock-s2.json'), 'misses')

    def test_s1_only_is_r9s_shape(self):
        self.mutate(lambda ev: ev['files'].update({'soak-clock-s2.json': ev['files']['soak-clock-s1.json']}), 'misses')

    def test_bracket_over_the_ceiling(self):
        self.mutate(lambda ev: ev['files'].update({'soak-clock-s2.json': sample(2870, 2873, 2871, 0)}), 'misses')

    def test_long_setup_residence_without_the_pair_is_refused_by_the_ceiling(self):
        self.mutate(lambda ev: ev['files'].update({'soak-clock-s1.json': sample(980, 990.2, 985, 0)}), 'misses')

    def test_foreign_m02_boot(self):
        self.mutate(lambda ev: ev['files']['soak-clock-s1.json'].update(boot_id='other'), 'misses')

    def test_foreign_run(self):
        self.mutate(lambda ev: ev['files']['soak-clock-s2.json'].update(run_id='other'), 'misses')

    def test_an_unordered_pair(self):
        self.mutate(lambda ev: ev['files']['soak-clock-s1.json'].update(t4=970), 'misses')

    def test_epoch_vs_mono_skew_during_a_job(self):
        self.mutate(lambda ev: ev['files']['soak-clock-s1.json'].update(exit_epoch=ev['files']['soak-clock-s1.json']['exit_epoch'] + 0.5), 'misses')

    def test_a_net_step_between_samples(self):
        self.mutate(lambda ev: ev['files']['soak-clock-s2.json'].update(entry_mono=ev['files']['soak-clock-s2.json']['entry_mono'] + 0.5,
                                                                       exit_mono=ev['files']['soak-clock-s2.json']['exit_mono'] + 0.5), 'misses')

    def test_an_epoch_value_in_a_mono_field(self):
        self.mutate(lambda ev: ev['files']['soak-clock-s1.json'].update(t1=EPOCH), 'misses')

    # Each of these is the only check that can refuse (mutation-verified).
    def test_dat_still_on_disk_refuses_the_miss(self):  # J8; vid 20 stays out of rotation's proven set via its own margin
        def ch(ev):
            c = self.collected(ev)
            c['dat_vids'] = [20]
        ev = evidence()
        ch(ev)
        self.assertIn('misses', self.failing(self.run_on(ev)))
        hit = [dict(submitted=1110, completed=1110.1)]
        args = (dict(submitted=1180, completed=1180.1), MissVersion(['20,ab']), dict(completed=1100),
                [dict(req=1178, done=1178.1, writables=[], present=[])], {20: EPOCH + 1175}, EPOCH - .1, EPOCH + .1)
        self.assertFalse(soak_assess.miss_excused(*args, {20}, hit))
        self.assertTrue(soak_assess.miss_excused(*args, set(), hit))

    def ledger(self, *rows, begin=EPOCH + 900, end=EPOCH + 2900):
        return soak_assess.rotation_boundaries(list(rows), begin, end)

    def test_ledger_positive(self):
        self.assertEqual(self.ledger(line('birth', 20, 995), line('unwritable', 20, 1150), line('success', 20, 1160)), {20: EPOCH + 1160})

    def test_ledger_no_unwritable_line_still_has_its_boundary(self):  # rotation of an already-full volume logs none
        self.assertEqual(self.ledger(line('birth', 20, 995), line('success', 20, 1160)), {20: EPOCH + 1160})

    def test_ledger_an_event_after_the_success_leaves_the_vid_out(self):
        self.assertEqual(self.ledger(line('birth', 20, 995), line('unwritable', 20, 1150), line('success', 20, 1160), line('unwritable', 20, 1161)), {})

    def test_ledger_equal_times_leave_the_vid_out(self):
        self.assertEqual(self.ledger(line('birth', 20, 1160), line('success', 20, 1160)), {})

    def test_ledger_an_ambiguous_date_leaves_the_vid_out(self):  # all three lines fit both years, in a valid order
        begin = datetime.datetime(2026, 2, 1, tzinfo=datetime.timezone.utc).timestamp()
        end = datetime.datetime(2027, 3, 15, tzinfo=datetime.timezone.utc).timestamp()
        rows = [dict(kind=k, vid=20, stamp=t + ' 00:00:00.000000', gen=0, line=k, sha256=k)
                for k, t in (('birth', '0210'), ('unwritable', '0220'), ('success', '0301'))]
        self.assertEqual(self.ledger(*rows, begin=begin, end=end), {})

    def test_clock_an_unordered_sample_alone_refuses(self):
        s2 = sample(2870, 2870.2, 2870.1, 0)
        c = dict(m02_boot_id=M02_BOOT, run_id='run-1')
        self.assertEqual(soak_assess.clock_bracket(c, sample(980, 980.2, 980.1, 0), s2)[1] > 0, True)
        with self.assertRaisesRegex(ValueError, 'unordered'):
            soak_assess.clock_bracket(c, sample(980, 979.9, 980, 0), s2)

    def test_clock_samples_must_bracket_the_body(self):
        s1 = sample(980, 980.2, 980.1, 0)
        with self.assertRaisesRegex(ValueError, 'bracket the body'):
            soak_assess.clock_bracket(dict(m02_boot_id=M02_BOOT, run_id='run-1'), s1, s1)

    # codex01 review b954b275.
    def test_a_birthless_episode_is_refused(self):
        def ch(ev):
            c = self.collected(ev)
            c['events'] = [e for e in c['events'] if not (e['vid'] == 20 and e['kind'] == 'birth')]
        self.mutate(ch, 'misses')

    def test_ledger_exactly_one_birth_before_the_success(self):
        self.assertEqual(self.ledger(line('unwritable', 20, 1150), line('success', 20, 1160)), {})
        self.assertEqual(self.ledger(line('birth', 20, 995), line('birth', 20, 996), line('success', 20, 1160)), {})
        self.assertEqual(self.ledger(line('unwritable', 20, 990), line('birth', 20, 995), line('success', 20, 1160)), {})
        self.assertEqual(self.ledger(line('birth', 20, 995), line('birth', 20, 996), line('unwritable', 20, 1150), line('success', 20, 1160)), {})

    def test_a_conflicting_line_refuses_every_implicated_vid(self):
        def ch(ev):
            c = self.collected(ev)
            original = next(e for e in c['events'] if e['vid'] == 20 and e['kind'] == 'unwritable')
            c['events'].append(dict(original, vid=999))
        self.mutate(ch, 'misses')

    def test_long_lines_with_one_shared_prefix_stay_distinct(self):
        a, b = line('birth', 20, 995), line('unwritable', 20, 1150)
        c = line('success', 20, 1160)
        for e in (a, b, c):
            e['line'] = 'same presentation prefix'
        self.assertEqual(self.ledger(a, b, c), {20: EPOCH + 1160})

    # The retiring boundary B (mono #969) and C9 (claude02 miss analysis 2026-11-04).
    def starts(self, *rows, begin=EPOCH + 900, end=EPOCH + 2900):
        rows = list(rows)
        return soak_assess.start_boundaries(rows, begin, end, soak_assess.rotation_boundaries(rows, begin, end))

    def test_ledger_a_retiring_line_keeps_the_success_boundary(self):
        rows = (line('birth', 20, 995), line('unwritable', 20, 1150), line('start', 20, 1159.9), line('success', 20, 1160))
        self.assertEqual(self.ledger(*rows), {20: EPOCH + 1160})
        self.assertAlmostEqual(self.starts(*rows)[20], EPOCH + 1159.9, places=5)

    def test_start_boundary_is_the_first_retiring_line_of_a_resumed_rotation(self):  # mono #972: the seal fence is monotonic
        rows = (line('birth', 20, 995), line('start', 20, 1150), line('start', 20, 1159.9), line('success', 20, 1160))
        self.assertAlmostEqual(self.starts(*rows)[20], EPOCH + 1150, places=5)  # the first attempt's seal fenced readers

    def test_the_accepted_limit_a_failed_first_attempt_without_a_later_read(self):  # codex01 R2 on fa1bad9, pinned
        # Failed first attempt line 1150; exact read 1154.1; loss 1155; miss 1156; retry line 1159.9; S 1160. With no
        # later same-VID read the miss is EXCUSED: inside the accepted last-exact..S blind spot, not a guarantee of C9.
        args = self.boundary_args(1156)
        self.assertTrue(soak_assess.miss_excused(*args, {20: EPOCH + 1150}, {20: 1154.1}))
        self.assertFalse(soak_assess.miss_excused(*args, {20: EPOCH + 1159.9}, {20: 1154.1}))  # a later B would refuse it
        self.assertFalse(soak_assess.miss_excused(*args, {20: EPOCH + 1150}, {20: 1157.1}))  # an observed later hit vetoes

    def test_a_resumed_rotation_whose_first_attempt_left_the_volume_serving_is_c9s(self):  # claude03 P3-1, via C9
        args = self.boundary_args(1159.95)
        starts = {20: EPOCH + 1150}  # first line long before the miss
        self.assertTrue(soak_assess.miss_excused(*args, starts, {20: 1110.1}))  # fenced since: excused
        self.assertFalse(soak_assess.miss_excused(*args, starts, {20: 1159.97}))  # the Volume still served after the miss

    def test_start_boundary_falls_back_to_the_success_line(self):
        self.assertEqual(self.starts(line('birth', 20, 995), line('success', 20, 1160)), {20: EPOCH + 1160})  # no retiring line
        # a retiring line after the success leaves the VID out entirely (rotation_boundaries' history rule)
        self.assertEqual(self.starts(line('birth', 20, 995), line('success', 20, 1160), line('start', 20, 1161)), {})

    def boundary_args(self, submitted):
        # The field shape: exact reads up to shortly before the rotation, then a GET landing after B but before S.
        hit = [dict(submitted=1110, completed=1110.1)]
        return (dict(submitted=submitted, completed=submitted + 0.001), MissVersion(['20,ab']), dict(completed=1100),
                [dict(req=1180, done=1180.1, writables=[], present=[])], {20: EPOCH + 1160}, EPOCH - .02, EPOCH + .02, set(), hit)

    def test_c4_red_green_a_get_between_retiring_and_success(self):
        args = self.boundary_args(1159.95)  # after late(B) = 1159.92, before late(S) = 1160.02
        self.assertFalse(soak_assess.miss_excused(*args))  # red: the success line alone cannot order it
        self.assertFalse(soak_assess.miss_excused(*args, {20: EPOCH + 1160}))  # B defaulted to S: still refused
        self.assertTrue(soak_assess.miss_excused(*args, {20: EPOCH + 1159.9}))  # green: after the retiring line

    def test_c4_a_get_straddling_the_retiring_line_still_refuses(self):
        self.assertFalse(soak_assess.miss_excused(*self.boundary_args(1159.89), {20: EPOCH + 1159.9}))  # before late(B)

    def test_c9_a_later_hit_on_the_same_vid_refuses(self):
        args = self.boundary_args(1159.95)
        self.assertTrue(soak_assess.miss_excused(*args, {20: EPOCH + 1159.9}, {20: 1159.951}))  # latest hit before the miss ends
        self.assertFalse(soak_assess.miss_excused(*args, {20: EPOCH + 1159.9}, {20: 1159.96}))  # the VID still served
        self.assertTrue(soak_assess.miss_excused(*args, {20: EPOCH + 1159.9}, {3: 1999.0}))  # another VID's later hit

    # codex01 R2 on 3a0e87c: C9 through the full assessor, a different key on the miss's VID and on another VID.
    def add_hit_on(self, ev, key, vid, submitted):
        q = ev['events'][-1]
        seq = max(a['sequence'] for a in ev['acks']) + 1
        seed = 'seed-c9-%d' % seq
        a = dict(key=key, writer=0, seed=seed, submitted=1104.95, completed=1105, sequence=seq, length=64,
                 digest=digest(seed, 64), fids=['%d,cd' % vid])
        ev['acks'].append(a)
        for kind in ('set', 'get'):
            q['dispatched'] += 1
            row = dict(op=str(q['dispatched']), worker=0, key=key, action=kind)
            row.update(dict(dispatched=1104.94, submitted=1104.95, completed=1105, ok=True) if kind == 'set' else
                       dict(dispatched=submitted - 0.01, submitted=submitted, completed=submitted + 0.1, ok=True, length=64,
                            sha256=a['digest'], sequence=seq))
            ev['events'].insert(-3, row)

    def test_c9_a_different_key_hit_on_the_miss_vid_after_the_miss_refuses(self):
        self.mutate(lambda ev: self.add_hit_on(ev, 'other-on-20', 20, 1181), 'misses')

    def test_c9_a_hit_on_another_vid_after_the_miss_changes_nothing(self):
        ev = evidence()
        self.add_hit_on(ev, 'other-on-3', 3, 1181)
        row = self.run_on(ev)
        self.assertEqual((row['status'], self.failing(row)), ('PASS', []), row['criteria'])

    def test_c9_a_same_vid_hit_before_the_miss_changes_nothing(self):
        ev = evidence()
        self.add_hit_on(ev, 'other-on-20', 20, 1170)
        row = self.run_on(ev)
        self.assertEqual((row['status'], self.failing(row)), ('PASS', []), row['criteria'])

    def test_a_miss_reporting_foreign_fids_is_refused(self):
        self.mutate(lambda ev: self.ev(ev, 'g2').update(fids=['3,ab']), 'misses')

    def test_a_miss_reporting_no_fids_is_refused(self):
        self.mutate(lambda ev: self.ev(ev, 'g2').update(fids=[]), 'misses')

    def in_flight(self, ev, submitted, completed):
        m = next(a for a in ev['acks'] if a['key'] == 'm')
        ev['acks'].append(dict(m, seed='newer', sequence=98, submitted=submitted, completed=completed, digest=digest('newer', 64), writer=0, fids=['3,ab']))
        q = ev['events'][-1]
        q['dispatched'] += 1
        ev['events'].insert(0, dict(op=str(q['dispatched']), worker=0, action='set', key='m', dispatched=submitted - .01, submitted=submitted, completed=completed, ok=True))

    def test_an_ack_completing_while_the_get_is_in_flight_is_not_its_latest(self):
        self.passes(lambda ev: self.in_flight(ev, 1180.01, 1180.05))

    def test_an_ack_completed_exactly_at_submission_does_not_precede_it(self):
        self.passes(lambda ev: self.in_flight(ev, 1179.9, 1180))

    def test_an_ack_completed_before_submission_is_the_latest(self):
        self.mutate(lambda ev: self.in_flight(ev, 1179.8, 1179.9), 'misses')

    def test_miss_without_the_m02_rotation_line(self):
        def ch(ev):
            c = ev['files']['m02-collect.json']
            c['rotated'] = [r for r in c['rotated'] if r['vid'] != 20] + [dict(vid=31, line='x')]
            c['events'] = [e for e in c['events'] if e['vid'] != 20]
        self.mutate(ch, 'misses')

    def test_miss_of_a_stale_version(self):
        def ch(ev):
            m = next(a for a in ev['acks'] if a['key'] == 'm')
            ev['acks'].append(dict(m, seed='newer', sequence=99, submitted=1111, completed=1112, digest=digest('newer', 64), writer=0))
            q = ev['events'][-1]
            q['dispatched'] += 1
            ev['events'].insert(0, dict(op=str(q['dispatched']), worker=0, action='set', key='m', dispatched=1110.9, submitted=1111,
                                        completed=1112, ok=True))
        self.mutate(ch, 'misses')

    def test_an_ok_not_hit_miss_candidate_is_judged_like_any_miss(self):
        # r9 (soak_workload.py sha 6a4bbb6b l.74-99): ok=True with results != [True] is a miss candidate, no error text.
        ev = evidence()
        self.ev(ev, 'g2').update(ok=True, error='', results=[False])
        row = self.run_on(ev)
        self.assertEqual((row['status'], self.failing(row)), ('PASS', []), row['criteria'])

    # codex01 review abacdf35: an ok=True row labelled miss whose results are hit-shaped or absent must not earn
    # retirement credit (same shape rule as the worker's classify_get).
    def test_an_ok_true_miss_with_hit_results(self):
        self.mutate(lambda ev: self.ev(ev, 'g2').update(ok=True, error='', results=[True], length=1, sha256='wrong'), 'get_outcomes')

    def test_an_ok_true_miss_with_none_results(self):
        self.mutate(lambda ev: self.ev(ev, 'g2').update(ok=True, error='', results=None, length=1, sha256='wrong'), 'get_outcomes')

    def test_an_ok_true_miss_without_results(self):
        def ch(ev):
            self.ev(ev, 'g2').update(ok=True, error='', length=1, sha256='wrong')
            self.ev(ev, 'g2').pop('results', None)
        self.mutate(ch, 'get_outcomes')

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

    # Decision 3 revision (e45019f9, codex01 8c56cca4), section 3: the pre-registered counterexamples, by number.
    # S is vid 20's success line at 1175; m is ACKed at 1100, read exactly at 1110, missed at 1180.
    def m_ack(self, ev, at):
        a = next(a for a in ev['acks'] if a['key'] == 'm')
        e = next(e for e in ev['events'] if e.get('key') == 'm' and e.get('action') == 'set')
        a.update(submitted=at - 0.05, completed=at)
        e.update(dispatched=at - 0.06, submitted=at - 0.05, completed=at)

    def vid20(self, ev, kind):
        c = self.collected(ev)
        c['events'] = [e for e in c['events'] if not (e['vid'] == 20 and e['kind'] == kind)]

    def test_cx01_already_full_no_new_unwritable_line_is_excused(self):
        self.passes(lambda ev: self.vid20(ev, 'unwritable'))

    def test_cx02_ack_just_after_the_capacity_line_is_excused(self):
        def ch(ev):
            self.collected(ev)['events'] = [line('unwritable', 20, 1099.9) if e['vid'] == 20 and e['kind'] == 'unwritable' else e
                                            for e in self.collected(ev)['events']]
        self.passes(ch)

    def test_cx03_ack_during_the_seal_grace_is_excused(self):
        def ch(ev):
            self.m_ack(ev, 1160)
            self.ev(ev, 'g1m').update(dispatched=1164.99, submitted=1165, completed=1165.1)
        self.passes(ch)

    def test_cx04_resumed_rotation_without_grace_is_excused(self):
        def ch(ev):
            self.vid20(ev, 'unwritable')
            self.collected(ev)['events'] = [line('success', 20, 1175, gen=1) if e['vid'] == 20 and e['kind'] == 'success' else e
                                            for e in self.collected(ev)['events']]
        self.passes(ch)

    def test_cx05_dispatch_inferred_from_an_unwritable_line_and_elapsed_time_is_refused(self):
        self.mutate(lambda ev: self.vid20(ev, 'success'), 'misses')

    def test_cx06_failed_rotation_without_a_success_line_is_refused(self):
        def ch(ev):
            self.vid20(ev, 'success')
            self.vid20(ev, 'unwritable')
        self.mutate(ch, 'misses')

    def test_cx07_miss_during_the_grace_before_s_is_refused(self):
        self.mutate(lambda ev: self.ev(ev, 'g2').update(dispatched=1170.9, submitted=1171, completed=1171.1), 'misses')

    def test_cx08_straddling_get_is_refused(self):
        self.mutate(lambda ev: self.ev(ev, 'g2').update(dispatched=1174.8, submitted=1174.9, completed=1176), 'misses')

    def test_cx09_ack_inside_the_clock_bracket_of_s_is_refused(self):
        def ch(ev):
            self.m_ack(ev, 1174.95)
            self.ev(ev, 'g1m').update(dispatched=1174.96, submitted=1174.97, completed=1174.98)
        self.mutate(ch, 'misses')

    def test_cx10_two_success_lines_are_refused(self):
        self.mutate(lambda ev: self.collected(ev)['events'].append(line('success', 20, 1176)), 'misses')

    def test_cx11_a_second_vid_without_s_is_refused(self):
        def ch(ev):
            m = next(a for a in ev['acks'] if a['key'] == 'm')
            m['fids'] = ['20,ab', '3,cd']
            self.ev(ev, 'g2')['fids'] = list(m['fids'])
        self.mutate(ch, 'misses')

    def test_cx12_never_read_back_before_s_is_refused(self):
        def ch(ev):
            g = self.ev(ev, 'g1m')
            g.update(ok=False, miss=True, error='DC batch push-read response status 1')
            del g['sha256'], g['length']
            g['fids'] = list(next(a for a in ev['acks'] if a['key'] == 'm')['fids'])
            g.update(dispatched=1184.99, submitted=1185, completed=1185.1)  # the only other read is itself a miss after S
        self.mutate(ch, 'misses')

    def test_cx13_wrong_bytes_before_s_fail_the_run(self):
        ev = evidence()
        self.ev(ev, 'g1m').update(sha256='0' * 64)
        self.assertIn('wrong_reads', self.failing(self.run_on(ev)))

    def test_cx14_an_exact_read_of_an_older_version_only_is_refused(self):
        def ch(ev):  # v1 ACKed 1100 and read exactly at 1110; v2 ACKed 1120, never read; the miss is of v2
            n = max(a['sequence'] for a in ev['acks']) + 1
            m = next(a for a in ev['acks'] if a['key'] == 'm')
            ev['acks'].append(dict(m, sequence=n, submitted=1119.95, completed=1120, seed='seed-v2', digest=digest('seed-v2', 64), writer=0))
            q = ev['events'][-1]
            q['dispatched'] += 1
            ev['events'].insert(-3, dict(op=str(q['dispatched']), worker=0, action='set', key='m', dispatched=1119.94, submitted=1119.95,
                                         completed=1120, ok=True))
            self.ev(ev, 'g2')['sequence'] = n
        self.mutate(ch, 'misses')

    def test_cx15_vid_present_after_s_is_refused(self):
        def ch(ev):
            p = next(p for p in ev['polls'] if p['req'] == 1178)
            p['present'] = sorted(set(p['present']) | {20})
        self.mutate(ch, 'misses')

    def test_cx16_dat_present_at_collect_is_refused(self):
        ev = evidence()
        self.collected(ev)['dat_vids'] = [20]
        self.assertIn('misses', self.failing(self.run_on(ev)))

    def test_cx17_master_restart_between_ack_and_s_is_excused(self):
        def ch(ev):
            self.collected(ev)['events'] = [line(e['kind'], 20, {'birth': 995, 'unwritable': 1150, 'success': 1175}[e['kind']],
                                                 gen=0 if e['kind'] == 'birth' else 1) if e['vid'] == 20 else e
                                            for e in self.collected(ev)['events']]
        self.passes(ch)

    def test_cx18_miss_of_an_older_version_after_a_newer_ack_is_refused(self):
        def ch(ev):
            n = max(a['sequence'] for a in ev['acks']) + 1
            m = next(a for a in ev['acks'] if a['key'] == 'm')
            ev['acks'].append(dict(m, sequence=n, submitted=1130 - 0.05, completed=1130, seed='seed-new', digest=digest('seed-new', 64), writer=0))
            q = ev['events'][-1]
            q['dispatched'] += 1
            ev['events'].insert(-3, dict(op=str(q['dispatched']), worker=0, action='set', key='m', dispatched=1129.94, submitted=1129.95,
                                         completed=1130, ok=True))
        ev = evidence()
        ch(ev)
        self.assertIn('misses', self.failing(self.run_on(ev)))

    def test_cx19_a_get_submitted_before_the_ack_is_no_read_back(self):
        self.mutate(lambda ev: self.ev(ev, 'g1m').update(dispatched=1099.9, submitted=1099.97, completed=1100.2), 'misses')

    def test_cx20_absence_only_before_birth_with_failed_polls_after_s_is_refused(self):
        def ch(ev):
            for p in ev['polls']:
                if p['req'] > 1170:
                    p.update(ok=False)
                    del p['present'], p['writables']
            ev['polls'].insert(0, dict(req=990, done=990.05, ok=True, writables=[], present=[1, 2, 3]))
        ev = evidence()
        ch(ev)
        self.assertIn('misses', self.failing(self.run_on(ev)))

    # codex01 review f94b5c19: the read-back is bound to the GET's own recorded version, not relabelled as the latest.
    def test_c5_a_hit_with_an_unknown_sequence_is_no_read_back(self):
        self.mutate(lambda ev: self.ev(ev, 'g1m').update(sequence=999), 'misses')

    def test_c5_a_hit_of_an_older_version_with_the_same_bytes_is_no_read_back(self):
        def ch(ev):  # v2 has v1's bytes on another FID; the only exact read is recorded against v1
            n = max(a['sequence'] for a in ev['acks']) + 1
            m = next(a for a in ev['acks'] if a['key'] == 'm')
            ev['acks'].append(dict(m, sequence=n, submitted=1104.95, completed=1105, fids=['20,cd'], writer=0))
            q = ev['events'][-1]
            q['dispatched'] += 1
            ev['events'].insert(-3, dict(op=str(q['dispatched']), worker=0, action='set', key='m', dispatched=1104.94, submitted=1104.95,
                                         completed=1105, ok=True))
            self.ev(ev, 'g2').update(sequence=n, fids=['20,cd'])
        self.mutate(ch, 'misses')

    # codex01 review f490de8a: raw worker sequences, not the replay's ACK-order ordinals.
    def raw_m(self, ev, raw):
        a = next(a for a in ev['acks'] if a['key'] == 'm')
        a['sequence'] = raw
        for e in ev['events']:
            if e.get('key') == 'm' and e.get('action') in ('get',):
                e['sequence'] = raw

    def test_c5_a_raw_sequence_out_of_ack_order_still_counts(self):
        self.passes(lambda ev: self.raw_m(ev, 99))  # m's intent began after later-ACKed keys: raw 99, replay ordinal lower

    def test_c5_a_hit_recorded_against_an_unacknowledged_raw_sequence_credits_nothing(self):
        def ch(ev):
            self.raw_m(ev, 99)
            self.ev(ev, 'g1m')['sequence'] = 98  # never ACKed for m
        self.mutate(ch, 'misses')

    def test_the_emitted_verdict_states_its_qualification(self):
        row = self.run_on(evidence())
        q = row.get('misses_qualification') or {}
        self.assertIn('qualified observed ordering under the stated limit', q.get('method', ''))
        self.assertIn('no parity claim', q.get('method', ''))
        self.assertIn('paired clock samples', q.get('clock', ''))
        self.assertIn('constant offset between the samples', q.get('clock', ''))
        self.assertIn('out-and-back', q.get('clock', ''))
        self.assertIn('last exact read', q.get('limit', ''))
        root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, root)
        out = subprocess.run([sys.executable, str(HERE / 'soak_assess.py'), '--config', json.dumps(dict(config(root), schema='wrong'))],
                             capture_output=True, text=True)
        failed = json.loads(out.stdout.splitlines()[-1])
        self.assertEqual(failed['status'], 'FAIL')
        self.assertEqual(failed.get('misses_qualification'), q)

    def test_cx21_known_limit_latent_loss_between_last_exact_read_and_s_is_excused(self):
        """Recorded, not refused, by Ping's acceptance: ACK 1100, exact read 1110, a loss in between, S 1175, miss 1180."""
        self.passes(lambda ev: None)


KV_PATTERN = dict(clients=2, transport='dc', keys=4, distribution='uniform', object_bytes=64, rate_per_client=1,
                  phases=[dict(kind='write_once'), dict(kind='read', duration_s=100)])


KV_RECEIPT = dict(State='REPLACED',  # what directSoak.Restart (cloud_soak_linux.go) records on success
                  Kill=dict(Identity=dict(Node='m02', PID=4242), AliveBefore=True, Signal='SIGKILL',
                            AppliedAt='2026-11-04T07:00:00Z', PreKillMono=1010.5),
                  Replacement=dict(Node='m02', PID=5151))


def receipt(**change):  # KV_RECEIPT with Kill fields (kill_*), Kill.Identity (victim_*) or top-level fields changed
    r = json.loads(json.dumps(KV_RECEIPT))
    for k, v in change.items():
        (r['Kill'] if k.startswith('kill_') else r['Kill']['Identity'] if k.startswith('victim_') else r)[k.split('_', 1)[1] if '_' in k and k.split('_')[0] in ('kill', 'victim') else k] = v
    return r


def kv_evidence(miss_at=None):
    """A complete kv_load run: 2 DC clients write 4 keys once (barrier at 1004), then read them
    uniformly at 1/s each for 100 s; volume fault E1 pauses 1010-1015; client 1 is killed at 1070."""
    acks, events, op = [], [], [0]
    def event(**e):
        op[0] += 1
        events.append(dict(e, op=str(op[0])))
        return events[-1]
    for i in range(4):
        seed, t = 'kv-load-%d-0' % i, 1001 + i
        acks.append(dict(key='k%d' % i, writer=i % 2, seed=seed, submitted=t - .05, completed=t, sequence=i + 1, length=64,
                         digest=digest(seed, 64), fids=['3,ab']))
        event(worker=i % 2, action='set', key='k%d' % i, dispatched=t - .06, submitted=t - .05, completed=t, ok=True)
    for n in range(200):
        t, a = 1005 + n / 2, acks[n % 4]
        hit = dict(ok=True, length=64, sha256=a['digest'])
        miss = dict(ok=False, miss=True, fids=a['fids'], error='DC batch push-read response status 1')
        event(worker=n % 2, action='get', key=a['key'], dispatched=t, submitted=t + .01, completed=t + .02,
              sequence=a['sequence'], **(miss if miss_at is not None and n == miss_at else hit))
    kill = dict(event='client_kill', kind='client_kill', worker=1, started=1070, killed=1071, finished=1072,
                witness=dict(event='submitted', action='get', pid=11, operation_id=str(op[0] + 1)), replacement=dict(pid=77, starttime=1))
    events += [kill, dict(event='body_end', mono=1200), dict(event='quiescent', mono=1261, dispatched=op[0] + 1)]
    boot, dc = boot_id(), dict(transport='rdma', http_fallback_allowed=False, direct_dc_reads=5, direct_rc_reads=0, copied_reads=0)
    files = {'soak-load-start.json': dict(boot_id=boot, mono=1000), 'workers.json': {str(i): dict(pid=10 + i, starttime=1, transport='dc') for i in range(2)},
             'faults.json': [dict(episode='E1', role='volume', dispatch_mono=1010, ready_mono=1015, receipt=KV_RECEIPT)],
             'm02-collect.json': dict(status='SOAK_COLLECTED', collection='lmcache', dat_vids=[], rotated=[], events=[])}
    stats = {'native-stats-%d-1.jsonl' % pid: [dict(status='OBSERVED', stats=dict(loader=dc))] for pid in (10, 11, 77)}
    return dict(files=files, acks=acks, events=events, polls=[], stats=stats)


class KVLoadAssess(unittest.TestCase):
    """kv_load's criteria set: its own write_once and load_as_declared rows."""
    failing = SoakAssess.failing

    def run_on(self, ev):
        root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, root)
        write(root, ev)
        return soak_assess.assess(dict(config(root), duration_s=200, client_kill_at_s=50, pattern=getattr(self, 'pattern', KV_PATTERN),
                                       faults=getattr(self, 'declared', [dict(at_s=10, role='volume', episode='E1')])))

    def kv_fails(self, change, rule):
        ev = kv_evidence()
        change(ev)
        row = self.run_on(ev)
        self.assertEqual((row['status'], self.failing(row)), ('FAIL', [rule]), row['criteria'])

    def test_a_complete_kv_load_run_passes_the_kv_load_set(self):
        row = self.run_on(kv_evidence())
        self.assertEqual((row['status'], self.failing(row)), ('PASS', []), row['criteria'])
        self.assertEqual(sorted(row['criteria']), sorted(soak_assess.KV_LOAD))

    def test_write_once_needs_every_key_once_and_no_read_before_the_barrier(self):
        ev = kv_evidence()  # key 3's first version recorded as key 2's: one key twice, one never (wrong_reads fails too)
        ev['acks'][3]['seed'] = 'kv-load-2-0'
        self.assertEqual(self.run_on(ev)['criteria']['write_once']['outcome'], 'FAIL')
        self.kv_fails(lambda ev: next(e for e in ev['events'] if e.get('action') == 'get').__setitem__('submitted', 1003), 'write_once')

    def test_load_as_declared_needs_every_client_and_its_rate(self):
        self.kv_fails(lambda ev: [e.__setitem__('worker', 0) for e in ev['events'] if e.get('action') == 'get'], 'load_as_declared')
        ev = kv_evidence()  # a third of the reads gone (terminal fails too: check the row itself)
        ev['events'] = [e for e in ev['events'] if not (e.get('action') == 'get' and int(e['op']) % 3 == 0)]
        self.assertEqual(self.run_on(ev)['criteria']['load_as_declared']['outcome'], 'FAIL')

    def test_a_skewed_uniform_read_share_is_not_as_declared(self):
        ev = kv_evidence()
        for e in ev['events']:
            if e.get('action') == 'get':
                e['key'], e['sequence'], e['sha256'] = 'k0', 1, ev['acks'][0]['digest']
        self.assertEqual(self.run_on(ev)['criteria']['load_as_declared']['outcome'], 'FAIL')

    def test_misses_inside_a_fault_window_are_reported_outside_they_fail(self):
        row = self.run_on(kv_evidence(miss_at=20))  # submitted at 1015.01, inside E1's window [1010, 1075]
        self.assertEqual(row['criteria']['misses']['outcome'], 'PASS', row['criteria']['misses'])
        self.assertIn('1 inside fault windows', row['criteria']['misses']['reason'])
        self.assertEqual(self.run_on(kv_evidence(miss_at=180))['criteria']['misses']['outcome'], 'FAIL')  # at 1095.01, after it

    def row(self, ev, name, **pattern):
        self.pattern = dict(KV_PATTERN, **pattern)
        return self.run_on(ev)['criteria'][name]['outcome']

    def test_objects_of_another_size_than_declared_fail_write_once(self):
        self.assertEqual(self.row(kv_evidence(), 'write_once', object_bytes=65), 'FAIL')

    def test_an_all_read_mixed_phase_is_not_as_declared(self):
        self.assertEqual(self.row(kv_evidence(), 'load_as_declared', phases=[dict(kind='write_once'), dict(kind='mixed', duration_s=100, read_ratio=.75)]), 'FAIL')

    def test_one_busy_client_and_one_idle_client_are_not_as_declared(self):
        ev = kv_evidence()
        gets = [e for e in ev['events'] if e.get('action') == 'get']
        for e in gets[1:]:
            e['worker'] = 0  # 199 to 1
        self.assertEqual(self.row(ev, 'load_as_declared'), 'FAIL')

    def test_a_plan_fault_missing_from_the_evidence_is_not_as_declared(self):
        ev = kv_evidence()
        ev['files']['faults.json'] = []
        self.assertEqual(self.row(ev, 'load_as_declared'), 'FAIL')

    def forged(self, **change):
        ev = kv_evidence(miss_at=20)  # a miss at 1015.01, inside the declared fault's window
        ev['files']['faults.json'][0].update(change)
        return self.run_on(ev)['criteria']

    def test_a_forged_fault_opens_no_window_and_is_not_as_declared(self):
        for name, change in (('empty kill receipt', dict(receipt=receipt(Kill={}))),
                             ('replacement on M01', dict(receipt=receipt(Replacement=dict(Node='m01', PID=5151)))),
                             ('kill on M01', dict(receipt=receipt(victim_Node='m01'))),
                             ('kill not applied', dict(receipt=receipt(kill_AppliedAt='0001-01-01T00:00:00Z'))),
                             ('restart failed', dict(receipt=receipt(State='FAILED', Error='replacement did not start'))),
                             ('replacement is the victim', dict(receipt=receipt(Replacement=dict(Node='m02', PID=4242)))),
                             ('PreKillMono after READY', dict(receipt=receipt(kill_PreKillMono=1016))),
                             ('READY 120 s after dispatch', dict(ready_mono=1130)),
                             ('dispatched 30 s off the declared 10 s', dict(dispatch_mono=1040, ready_mono=1043)),
                             ('another episode', dict(episode='E9'))):
            crit = self.forged(**change)
            with self.subTest(name):
                self.assertEqual((crit['load_as_declared']['outcome'], crit['misses']['outcome']), ('FAIL', 'FAIL'), crit)

    def test_the_approved_runners_success_state_also_qualifies(self):
        crit = self.forged(receipt=receipt(State='REPLACEMENT_STARTED'))
        self.assertEqual((crit['load_as_declared']['outcome'], crit['misses']['outcome']), ('PASS', 'PASS'), crit)

    def test_a_duplicate_episode_cannot_stand_in_for_a_missing_one(self):
        self.declared = [dict(at_s=10, role='volume', episode='E1'), dict(at_s=60, role='master', episode='E2')]
        ev = kv_evidence(miss_at=20)
        ev['files']['faults.json'].append(dict(ev['files']['faults.json'][0]))  # E1 twice, no E2
        crit = self.run_on(ev)['criteria']
        self.assertEqual((crit['load_as_declared']['outcome'], crit['misses']['outcome']), ('FAIL', 'FAIL'), crit)

    def test_rates_are_per_client_in_every_phase_not_balanced_across_them(self):
        ev = kv_evidence()
        for e in ev['events']:
            if e.get('action') == 'get':
                e['worker'] = 0 if e['dispatched'] < 1054 else 1  # one client per 50 s phase
        self.assertEqual(self.row(ev, 'load_as_declared', phases=[dict(kind='write_once'), dict(kind='read', duration_s=50),
                                                                  dict(kind='read', duration_s=50)]), 'FAIL')
        self.assertEqual(self.row(kv_evidence(), 'load_as_declared', phases=[dict(kind='write_once'), dict(kind='read', duration_s=50),
                                                                             dict(kind='read', duration_s=50)]), 'PASS')

    def test_a_miss_outside_the_fault_windows_fails_even_if_the_soak_would_excuse_it(self):
        saved = soak_assess.miss_excused
        soak_assess.miss_excused = lambda *a: True
        self.addCleanup(setattr, soak_assess, 'miss_excused', saved)
        self.assertEqual(self.run_on(kv_evidence(miss_at=180))['criteria']['misses']['outcome'], 'FAIL')

    def test_an_http_client_is_refused(self):
        ev = kv_evidence()
        ev['files']['workers.json']['1']['transport'] = 'http'
        with self.assertRaises(ValueError):
            self.run_on(ev)


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
