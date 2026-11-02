"""Labelled SYNTHETIC evidence for the soak verdict: one complete consistent
run, then one mutation per accepted rule; each must fail exactly its rule."""
import copy
import hashlib
import json
import shutil
import tempfile
import unittest
from pathlib import Path

import soak_assess
from soak_native_worker import payload

ROT = list(range(20, 31))  # 11 rotated volumes


def digest(seed, length):
    return hashlib.sha256(payload(seed, length)).hexdigest()


def evidence():
    acks, events, n = [], [], [0]
    def ack(key, writer, t, vid=3):
        n[0] += 1
        seed = 'seed-%d' % n[0]
        acks.append(dict(key=key, writer=writer, seed=seed, submitted=t - 0.05, completed=t, sequence=n[0], length=64,
                         digest=digest(seed, 64), fids=['%d,ab' % vid]))
        events.append(dict(op='s%d' % n[0], action='set', key=key, submitted=t - 0.05, completed=t, ok=True))
        return acks[-1]
    for w, t in enumerate((1010, 1020, 1030, 1040)):  # one key written by all four workers
        ack('shared', w, t)
    a = ack('k2', 1, 1050)
    events.append(dict(op='g1', action='get', key='k2', submitted=1060, completed=1060.1, ok=True, sequence=a['sequence'],
                       length=64, sha256=a['digest']))
    m = ack('m', 2, 1100, vid=20)  # its volume rotates: unwritable at 1150, gone at 1170
    events.append(dict(op='g2', action='get', key='m', submitted=1160, completed=1160.1, ok=False, miss=True,
                       sequence=m['sequence'], fids=m['fids'], error='DC batch push-read response status 1'))
    events.append(dict(op='g9', action='get', key='k2', submitted=1478, completed=1490, ok=False, fault_error=True,
                       episode='E1', sequence=a['sequence'], error='operation timed out'))
    events.append(dict(event='client_kill', kind='client_kill', worker=1, started=2320, finished=2321,
                       witness=dict(event='submitted'), replacement=dict(pid=77, starttime=1)))
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
    files = {'soak-load-start.json': dict(boot_id='b', mono=1000), 'faults.json': faults,
             'fault-ack-E1.json': dict(episode='E1', observed_mono=1479, pending_gets=[dict(op='g9', submitted=1478)]),
             'm02-collect.json': dict(status='SOAK_COLLECTED', collection='lmcache', dat_vids=[],
                                      rotated=[dict(vid=v, line='rotated non-replicated cache Volume %d' % v) for v in ROT]),
             'workers.json': {'0': dict(pid=10, transport='dc'), '1': dict(pid=11, transport='dc'),
                              '2': dict(pid=12, transport='dc'), '3': dict(pid=13, transport='http')}}
    stats = {}
    for pid, dc in ((10, True), (11, True), (12, True), (13, False), (77, True)):
        loader = dict(transport='rdma', http_fallback_allowed=False, direct_dc_reads=5, copied_reads=0) if dc else \
            dict(transport='http', http_fallback_allowed=True, direct_dc_reads=0, copied_reads=5)
        stats['native-stats-%d-1.jsonl' % pid] = [dict(status='OBSERVED', stats=dict(loader=loader))]
    return dict(files=files, acks=acks, events=events, polls=polls, stats=stats)


class SoakAssess(unittest.TestCase):
    def run_on(self, ev):
        root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, root)
        for name, body in ev['files'].items():
            (root / name).write_text(json.dumps(body))
        for name, key in (('ack-journal.jsonl', 'acks'), ('op-events.jsonl', 'events'), ('topology-polls.jsonl', 'polls')):
            (root / name).write_text(''.join(json.dumps(r) + '\n' for r in ev[key]))
        for name, rs in ev['stats'].items():
            (root / name).write_text(''.join(json.dumps(r) + '\n' for r in rs))
        return soak_assess.assess(dict(root=str(root), duration_s=1800, client_kill_at_s=1320, collection='lmcache',
                                       faults=ev['files']['faults.json']))

    def failing(self, row):
        return sorted(k for k, v in row['criteria'].items() if v['outcome'] != 'PASS')

    def test_the_complete_synthetic_run_passes_every_criterion(self):
        row = self.run_on(evidence())
        self.assertEqual((row['status'], self.failing(row), row['resources']), ('PASS', [], 'NOT_OBSERVED'), row)
        self.assertEqual([k['episode'] for k in row['k7']], ['E1', 'E2'])

    def mutate(self, change, rule):
        ev = evidence()
        change(ev)
        row = self.run_on(ev)
        self.assertEqual((row['status'], self.failing(row)), ('FAIL', [rule]), row['criteria'])

    def get(self, ev, op):
        return next(e for e in ev['events'] if e.get('op') == op)

    def test_wrong_bytes(self):
        self.mutate(lambda ev: self.get(ev, 'g1').update(sha256='0' * 64), 'wrong_reads')

    def test_failed_set(self):
        self.mutate(lambda ev: ev['events'].append(dict(op='sx', action='set', key='z', submitted=1, completed=2, ok=False)), 'failed_sets')

    def test_miss_before_the_volume_left_the_write_set(self):
        self.mutate(lambda ev: self.get(ev, 'g2').update(submitted=1140, completed=1140.1), 'misses')

    def test_miss_on_a_volume_writable_again_before_it_vanished(self):
        def ch(ev):  # back in the write set after the miss, before absence: the candidate is invalidated
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
            g2 = self.get(ev, 'g2')
            ev['events'].remove(g2)
            m = next(a for a in ev['acks'] if a['key'] == 'm')
            newer = dict(m, seed='newer', sequence=99, submitted=1104, completed=1105, digest=digest('newer', 64))
            ev['acks'].append(newer)
            ev['events'].append(g2)  # still names the OLD sequence
        self.mutate(ch, 'misses')

    def test_fault_error_outside_p_plus_60(self):
        self.mutate(lambda ev: self.get(ev, 'g9').update(completed=1541), 'fault_errors')

    def test_fault_error_not_acknowledged_pending(self):
        self.mutate(lambda ev: self.get(ev, 'g9').update(op='g10'), 'fault_errors')

    def test_non_transient_fault_error(self):
        self.mutate(lambda ev: self.get(ev, 'g9').update(error='permission denied'), 'fault_errors')

    def test_fault_error_of_an_episode_without_a_kill(self):
        self.mutate(lambda ev: ev['files']['faults.json'][0]['receipt']['Kill'].update(PreKillMono=0), 'fault_errors')

    def test_missing_body_end(self):
        self.mutate(lambda ev: ev['events'].remove(next(e for e in ev['events'] if e.get('event') == 'body_end')), 'body')

    def test_no_client_kill(self):
        def ch(ev):
            ev['events'].remove(next(e for e in ev['events'] if e.get('event') == 'client_kill'))
            del ev['stats']['native-stats-77-1.jsonl']  # no kill, so no replacement worker
        self.mutate(ch, 'client_kill')

    def test_too_few_rotations(self):
        def ch(ev):
            ev['files']['m02-collect.json']['dat_vids'] = [21, 22]
        self.mutate(ch, 'rotation')

    def test_dc_worker_with_fallback(self):
        self.mutate(lambda ev: ev['stats']['native-stats-10-1.jsonl'][0]['stats']['loader'].update(http_fallback_allowed=True), 'routes')

    def test_missing_worker_coverage(self):
        self.mutate(lambda ev: ev.update(acks=[dict(a, writer=0 if a['writer'] == 3 else a['writer']) for a in ev['acks']]), 'coverage')

    def test_foreign_or_malformed_m02_fails_closed(self):
        ev = evidence()
        ev['files']['m02-collect.json']['collection'] = 'other'
        with self.assertRaises(ValueError):
            self.run_on(ev)


if __name__ == '__main__':
    unittest.main()
