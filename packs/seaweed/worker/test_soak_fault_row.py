"""Stage 3 fault-row verdict, through the real caller (`python -I soak_assess.py --config ...`), on labelled
SYNTHETIC evidence: one consistent row per kind, then the plan's negative controls, each a plausible fake
PASS that lies about exactly one thing."""
import copy
import hashlib
import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import soak_assess
from soak_native_worker import payload
from soak_stats import boot_id
from test_soak_assess import M02_BOOT, sample

HERE = Path(__file__).resolve().parent
M02 = 4000.0  # M02 CLOCK_MONOTONIC = M01 mono + 4000 in the fixture (a different clock, joined only by witnesses)
WID = {i: dict(pid=10 + i, starttime=1) for i in range(4)}
VOL, REPL = dict(pid=500, starttime=9), dict(pid=501, starttime=20)
BOUND = 7 << 20  # configured: ranges 4 MiB + pool 2 MiB + cache 1 MiB (the product gauges agree in the fixture)
DATA = ['lmcache_3.dat', 'lmcache_3.idx', 'lmcache_4.dat', 'lmcache_4.idx']
MASTER, FILER = dict(pid=600, starttime=3), dict(pid=601, starttime=4)
TCP = [dict(state='LISTEN', local='127.0.0.1:8080', peer='*:*'), dict(state='ESTAB', local='127.0.0.1:50001', peer='127.0.0.1:19333')]
OWNERS = {'127.0.0.1:19333': MASTER}
FILER_CONN = dict(state='ESTAB', local='127.0.0.1:8080', peer='127.0.0.1:41000')


def digest(seed, length=64):
    return hashlib.sha256(payload(seed, length)).hexdigest()


class Row:
    """Builds one row's evidence: ops at M01 mono, samples at M02 mono, all mutually consistent."""

    def __init__(self, kind):
        self.kind, self.n, self.seq = kind, 0, 0
        self.acks, self.events, self.ledger, self.audit, self.marks = [], [], [], {}, []
        self.fault = dict(episode='E1', role='volume', kind=kind, dispatch_mono=1100.0, ready_mono=1130.0, receipt={}, injection=[])
        self.recovery = 60 if kind in ('volume_kill_io', 'client_kill_put') else 30

    def op(self):
        self.n += 1
        return str(self.n)

    def put(self, key, t, state='ACKED', worker=1, done=None, attributed=None, error=None, event=True, op=None):
        op, seed = op or self.op(), 'seed-%s-%s' % (key, t)
        self.seq += 1
        done = t + 0.05 if done is None else done
        row = dict(op=op, key=key, state=state, seed=seed, length=64, digest=digest(seed), sequence=self.seq, worker=worker,
                   dispatched=t - 0.01, submitted=t, completed=done, error=error, attributed=attributed, worker_identity=WID[worker])
        self.ledger.append(row)
        if event:
            self.events.append(dict(op=op, worker=worker, action='set', key=key, dispatched=t - 0.01, submitted=t, completed=done,
                                    ok=state == 'ACKED', error=error, state=state))
        if state == 'ACKED':
            self.acks.append(dict(key=key, writer=worker, seed=seed, submitted=t, completed=done, sequence=self.seq, length=64,
                                  digest=row['digest'], fids=['3,ab']))
        return row

    def latest(self, key):
        return [a for a in self.acks if a['key'] == key][-1]

    def get(self, key, t, worker=0, done=None, fault=False):
        a = self.latest(key)
        e = dict(op=self.op(), worker=worker, action='get', key=key, dispatched=t - 0.01, submitted=t,
                 completed=t + 0.01 if done is None else done, sequence=a['sequence'])
        if fault:
            e.update(ok=False, fault_error=True, episode='E1', error='operation timed out')
        else:
            e.update(ok=True, length=64, sha256=a['digest'])
        self.events.append(e)
        return e


def samples(kind, mutate=None):
    """The volume sampler: pre_load 950-1000, load to 1366, quiescence to 1486 (M01 mono; written at M02 mono)."""
    out, released = [], 0
    for seq, t in enumerate(range(950, 1490, 5)):
        phase = 'pre_load' if t < 1000 else 'load' if t < 1366 else 'quiescence'
        busy = phase == 'load'
        ident = REPL if kind == 'volume_kill_io' and t > 1100 else VOL
        if kind in ('client_kill_put', 'conn_kill') and t >= 1105:
            released = 1
        s = dict(seq=seq, mono=t + M02, epoch=t + 1793610000.0, phase=phase, identity=dict(ident),
                 watch=dict(master=dict(MASTER), filer=dict(FILER)), dat_sizes=[33554432],
                 # volumes the load created hold their .dat/.idx fds (listed by the volume server's /status), and the
                 # filer opened one connection to the volume: infrastructure, within baseline + 1 for its role
                 **(dict(fds=38 + 2, data_fds=0, data_files=[], volumes=[], socket_fds=2, tcp=TCP, tcp_owners=dict(OWNERS)) if phase == 'pre_load' else
                    dict(fds=(48 if busy else 38) + 3 + 4, data_fds=4, data_files=DATA, volumes=['lmcache_3', 'lmcache_4'],
                         socket_fds=3, tcp=TCP + [FILER_CONN], tcp_owners=dict(OWNERS, **{'127.0.0.1:41000': dict(FILER)}))),
                 limits=dict(memlock=[-1, -1], nofile=[1024, 1024]), rdma=dict(qp=6 if busy else 4, mr=3, mr_lens=[1 << 20] * 3),
                 vm_pin_kB=4096 if busy else 1024, port=['4: ACTIVE', 0], free=1 << 30,
                 metrics=dict(pinned=float(1 << 20), pool_bytes=float(2 << 20), cache_bytes=float(1 << 20), retained_bytes=0.0, retained_sources=0.0, released=float(released),
                              registration_refused=float(t >= 1110 and kind == 'mr_pressure'),
                              registration_fallback=0.0, accept_errors=float(t >= 1110 and kind == 'fd_exhaust')))
        if 1100 < t <= 1130:
            if kind == 'link_flap':
                s['port'] = ['1: DOWN', 1]
            if kind == 'mr_pressure':
                s['limits']['memlock'] = [1 << 20, -1]
            if kind == 'fd_exhaust':
                s['limits']['nofile'], s['fds'] = [60, 1024], 60
            if kind == 'disk_full':
                s['free'] = 1000
        if kind == 'link_flap' and t > 1130:
            s['port'] = ['4: ACTIVE', 1]
        if mutate:
            mutate(s, t)
        out.append(s)
    return out


def injection(kind):
    if kind in ('volume_kill_io', 'client_kill_put'):
        return []
    snap = lambda mono, **kw: dict(dict(mono=mono + M02, epoch=mono + 1793610000.0, identity=VOL, limits=dict(memlock=[-1, -1], nofile=[1024, 1024]), fds=40), **kw)
    before, after, plan, end = snap(1100.0), snap(1100.5), {}, snap(1130.0)
    if kind == 'link_flap':
        before['port'], after['port'] = ['4: ACTIVE', 0], ['1: DOWN', 1]
    if kind in ('conn_kill', 'control'):
        before['conns'], after['conns'] = ['198.51.100.2:18888>198.51.100.1:5000'], ([] if kind == 'conn_kill' else ['198.51.100.2:18888>198.51.100.1:5000'])
        end['conns'] = ['198.51.100.2:18888>198.51.100.1:5001'] if kind == 'conn_kill' else before['conns']
    if kind == 'mr_pressure':
        plan, after['limits'] = dict(memlock=1 << 20), dict(memlock=[1 << 20, -1], nofile=[1024, 1024])
    if kind == 'fd_exhaust':
        plan, after['limits'] = dict(nofile=60), dict(memlock=[-1, -1], nofile=[60, 1024])
    if kind == 'disk_full':
        before['free'], after['free'], plan = 1 << 30, 1000, dict(ballast_bytes=(1 << 30) - 1000)
    return [dict(action='apply', kind=kind, identity=VOL, before=before, after=after, plan=plan),
            dict(action='restore', kind=kind, identity=VOL, after=end, restored=True)]


def row_evidence(kind):
    r = Row(kind)
    for k in ('k1', 'k2', 'q'):
        r.put(k, 1010.0 + len(r.acks))
    for t in range(1020, 1090, 10):
        r.get('k1', float(t))
    r.fault['injection'] = injection(kind)
    if kind == 'volume_kill_io':
        r.fault.update(ready_mono=1110.0, receipt=dict(State='REPLACED', Kill=dict(Identity=dict(PID=500, StartTime=9), PreKillMono=1100.0,
                                                                                   PostKillMono=1100.3, Signal='SIGKILL'),
                                                       Replacement=dict(PID=501, StartTime=20)))
        put = r.put('q', 1099.9, state='UNKNOWN', done=1105.0, attributed='E1', error='connection reset')
        r.get('k1', 1099.95, done=1104.0, fault=True)
        # freeze-then-kill: the volume was stopped at 1099.8 and the load named the ops it dispatched after that
        r.freeze = dict(run_id='run-1', episode='E1', stopped_pid=500, stopped_starttime=9, freeze_mono=1099.8, wait_s=5.0,
                        inflight=dict(run_id='run-1', boot_id=boot_id(), episode='E1', stopped_pid=500, stopped_starttime=9,
                                      freeze_mono=1099.8, ack_mono=1099.97, pending_sets=[dict(op=put['op'], submitted=put['dispatched'])],
                                      pending_gets=[dict(op=r.events[-1]['op'], submitted=r.events[-1]['dispatched'])]))
    if kind == 'client_kill_put':
        r.fault.update(role='client', ready_mono=1100.0)
        op = r.op()
        r.put('f2', 1099.5, state='UNKNOWN', done=1101.0, attributed='E1', error='client killed with the PUT pending', event=False, op=op)
        r.marks.append(dict(event='client_kill', kind='client_kill_put', worker=1, started=1099.0, killed=1100.0, finished=1101.0,
                            witness=dict(event='submitted', operation_id=op, action='set', pid=11, monotonic=1099.5),
                            victim=dict(WID[1]), replacement=dict(pid=77, starttime=2)))
    if kind == 'link_flap':
        r.get('k2', 1104.0, done=1105.0, fault=True)
    if kind == 'disk_full':
        r.put('k9', 1105.0, state='REFUSED', error='No space left on device')
    heal = 1101.0 if kind == 'client_kill_put' else r.fault['ready_mono']
    r.get('k2', heal + 2)
    for t in range(1150, 1300, 25):
        r.get('k1', float(t))
    r.put('k2', 1300.0)
    r.get('k2', 1310.0)
    # The audit: every ledger key (ACKED or UNKNOWN), each answering its survivor or what its state allows.
    for key in sorted({x['key'] for x in r.ledger if x['state'] != 'REFUSED'}):
        acked = [x for x in r.ledger if x['key'] == key and x['state'] == 'ACKED']
        r.audit[key] = [dict(case='hit', sha256=acked[-1]['digest'], length=64)] if acked else [dict(case='miss')]
    return r


def files(r, kind):
    boot = boot_id()
    marks = r.marks + [dict(event='fault_window', phase='armed', episode='E1', mono=1100.0),
                       dict(event='fault_window', phase='closed', episode='E1', mono=1131.0),
                       dict(event='body_end', mono=1300.0),
                       dict(event='audit_done', mono=1365.0, keys=len(r.audit), complete=True, client=dict(pid=900, starttime=50)),
                       dict(event='quiescent', mono=1366.0, dispatched=r.n)]
    extra = {'fault-freeze-witness-E1.json': r.freeze} if getattr(r, 'freeze', None) else {}
    if kind == 'link_flap':  # r12: the F3 load always writes it (empty when nothing completed late)
        extra['late-completions.jsonl'] = getattr(r, 'late', [])
    return dict(extra, **{
        'soak-load-start.json': dict(boot_id=boot, mono=1000.0),
        'faults.json': [r.fault],
        'workers.json': {str(i): dict(WID[i], transport='http' if i == 3 else 'dc') for i in range(4)},
        'm02-collect.json': dict(status='SOAK_COLLECTED', collection='lmcache', dat_vids=[], rotated=[], events=[]),
        'soak-clock-s1.json': sample(980, 990.2, 980.1, 10), 'soak-clock-s2.json': sample(1500, 1500.2, 1500.1, 0),
        'ack-journal.jsonl': r.acks, 'write-ledger.jsonl': r.ledger,
        'op-events.jsonl': sorted(r.events, key=lambda e: e['completed']) + marks,
        'audit.jsonl': [dict(key=k, reads=v) for k, v in sorted(r.audit.items())],
        'topology-polls.jsonl': [dict(req=float(t), done=t + 0.05, ok=True, writables=[3], present=[1, 2, 3]) for t in range(1000, 1370, 10)],
        'resource-samples.jsonl': samples(kind),
    })


def config(root, kind):
    return dict(schema=soak_assess.SCHEMA, root=str(root), run_id='run-1', boot_id=boot_id(), m02_boot_id=M02_BOOT, duration_s=300,
                quiescence_s=60, client_kill_at_s=0, collection='lmcache',
                fault_row=dict(kind=kind, episode='E1', recovery_s=60 if kind in ('volume_kill_io', 'client_kill_put') else 30,
                               pin_ranges_bytes=4 << 20, pin_pool_bytes=2 << 20, pin_cache_bytes=1 << 20, retained_budget_bytes=4 << 20, free_below_bytes=1 << 20,
                               client_fallback_counters=['rdma_fallbacks'],
                               **(dict(netdev='enp1s0v0', rdma_device='mlx5_0') if kind == 'link_flap' else {})))


def write_row(root, kind, edit=None):
    """The row's evidence files in root (edit mutates the dict of files first). The Go bridge test calls this."""
    ev = files(row_evidence(kind), kind)
    if edit:
        edit(ev)
    for name, body in ev.items():
        text = ''.join(json.dumps(x) + '\n' for x in body) if name.endswith('.jsonl') else json.dumps(body)
        (Path(root) / name).write_text(text)


def no_registration(ev):
    for s in ev['resource-samples.jsonl']:
        s['metrics']['registration_refused'] = 0.0


def verdict(test, kind, edit=None, fix=None):
    """Write the row's evidence, run the real CLI, return its row."""
    root = Path(tempfile.mkdtemp())
    test.addCleanup(shutil.rmtree, root)
    write_row(root, kind, edit)
    c = config(root, kind)
    if fix:
        fix(c)
    out = subprocess.run([sys.executable, '-I', str(HERE / 'soak_assess.py'), '--config', json.dumps(c)],
                         capture_output=True, text=True, timeout=60)
    test.assertEqual(out.returncode, 0, out.stderr)
    return json.loads(out.stdout)


KINDS = list(soak_assess.ROW_KINDS)


class Positives(unittest.TestCase):
    def test_each_row_witnessed_on_fixtures(self):
        for kind in KINDS:
            with self.subTest(kind=kind):
                v = verdict(self, kind)
                self.assertNotIn('error', v, v.get('error'))
                self.assertEqual({k: x['outcome'] for k, x in v['criteria'].items() if x['outcome'] != 'PASS'},
                                 {'resources': 'NOT_COVERED'} if kind == 'volume_kill_io' else {}, v['criteria'])
                # F1: the replacement has no pre-load sample, so its resources cannot be PID-joined (plan rule).
                self.assertEqual(v['status'], 'NOT_COVERED' if kind == 'volume_kill_io' else 'PASS')
                self.assertEqual(v['row'], soak_assess.ROW_KINDS[kind])
                self.assertEqual(sorted(v['criteria']), sorted(soak_assess.ROW_CRITERIA))

    def test_f1_and_f2_report_their_attributed_unknown_count(self):
        for kind in ('volume_kill_io', 'client_kill_put'):
            with self.subTest(kind=kind):
                self.assertEqual(verdict(self, kind)['unknown_attributed'], 1)

    def test_f4_claims_nothing_about_qp(self):
        self.assertEqual(verdict(self, 'conn_kill')['claims'], dict(qp='NOT_COVERED'))

    def test_retained_within_budget_is_not_a_leak(self):
        def held(ev):
            for s in ev['resource-samples.jsonl']:
                if s['phase'] == 'quiescence':
                    s['vm_pin_kB'] += 1024
                    s['metrics'].update(retained_bytes=float(2 << 20), retained_sources=1.0)
        v = verdict(self, 'control', held)
        self.assertEqual(v['resource_classes']['vm_pin_kB'], 'retained')
        self.assertEqual(v['status'], 'PASS', v['criteria'])


def outcome(v, name):
    return v['criteria'][name]['outcome']


class NotCovered(unittest.TestCase):
    def test_each_injector_removed(self):
        def no_injector(kind):
            def edit(ev):
                ev['faults.json'][0]['injection'] = []
                ev['faults.json'][0]['receipt'] = {}
                ev['resource-samples.jsonl'] = samples('control')
                ev['op-events.jsonl'] = [e for e in ev['op-events.jsonl'] if e.get('event') != 'client_kill']
                ev['write-ledger.jsonl'] = [x for x in ev['write-ledger.jsonl'] if x['state'] != 'UNKNOWN']
                ev['audit.jsonl'] = [a for a in ev['audit.jsonl'] if any(x['key'] == a['key'] for x in ev['write-ledger.jsonl'])]
                # with no kill, the PUT that was pending at it simply completes: nothing else changes
                for e in ev['op-events.jsonl']:
                    if e.get('state') == 'UNKNOWN':
                        e.update(state='ACKED', ok=True)
            return edit
        for kind in KINDS[1:]:
            with self.subTest(kind=kind):
                ev_fix = no_injector(kind)
                def edit(ev, ev_fix=ev_fix, kind=kind):
                    ev_fix(ev)
                    if kind == 'volume_kill_io':  # the completed PUT is in the ledger and the ACK journal as ACKED
                        unknown = next(e for e in ev['op-events.jsonl'] if e.get('state') == 'ACKED' and e['completed'] == 1105.0)
                        seed = 'seed-q-1099.9'
                        ev['write-ledger.jsonl'].append(dict(op=unknown['op'], key='q', state='ACKED', seed=seed, length=64, digest=digest(seed),
                                                            sequence=4, worker=1, dispatched=1099.89, submitted=1099.9, completed=1105.0,
                                                            error=None, attributed=None, worker_identity=WID[1]))
                        ev['ack-journal.jsonl'].append(dict(key='q', writer=1, seed=seed, submitted=1099.9, completed=1105.0, sequence=4,
                                                           length=64, digest=digest(seed), fids=['3,ab']))
                        for a in ev['audit.jsonl']:
                            if a['key'] == 'q':
                                a['reads'] = [dict(case='hit', sha256=digest(seed), length=64)]
                    if kind == 'disk_full':  # no full disk: the PUT is not refused
                        ev['write-ledger.jsonl'] = [x for x in ev['write-ledger.jsonl'] if x['state'] != 'REFUSED']
                        ev['op-events.jsonl'] = [e for e in ev['op-events.jsonl'] if e.get('state') != 'REFUSED']
                        renumber(ev)
                    if kind == 'client_kill_put':
                        renumber(ev)
                v = verdict(self, kind, edit)
                self.assertNotIn('error', v, v.get('error'))
                self.assertEqual(outcome(v, 'causal'), 'NOT_COVERED', v['criteria'])
                self.assertEqual(v['status'], 'NOT_COVERED', v['criteria'])

    def test_injector_witness_present_but_host_event_or_effect_missing(self):
        """The injector's own rows stay; only the sampled host event, or only the effect, is taken away."""
        def samples_of(mutate):
            def edit(ev):
                for s in ev['resource-samples.jsonl']:
                    mutate(s)
            return edit
        def drop_ops(pred):
            def edit(ev):
                ev['op-events.jsonl'] = [e for e in ev['op-events.jsonl'] if not pred(e)]
                ev['write-ledger.jsonl'] = [x for x in ev['write-ledger.jsonl'] if not pred(x)]
                renumber(ev)
            return edit
        def f1_no_kill(ev):
            for s in ev['resource-samples.jsonl']:
                s['identity'] = dict(VOL)
        def f2_not_unknown(ev):
            next(x for x in ev['write-ledger.jsonl'] if x['state'] == 'UNKNOWN')['state'] = 'REFUSED'
        def f4_no_new(ev):
            ev['faults.json'][0]['injection'][1]['after']['conns'] = []
        def f5_unchanged(s):
            s['limits']['memlock'] = [-1, -1]
        cases = [
            ('F1 host event', 'volume_kill_io', f1_no_kill),
            ('F2 effect: not UNKNOWN', 'client_kill_put', f2_not_unknown),
            ('F2 effect: no release', 'client_kill_put', samples_of(lambda s: s['metrics'].update(released=0.0))),
            ('F3 host event', 'link_flap', samples_of(lambda s: s.update(port=['4: ACTIVE', 0]))),
            ('F3 effect', 'link_flap', drop_ops(lambda e: e.get('fault_error'))),
            ('F4 no new connection', 'conn_kill', f4_no_new),
            ('F4 effect: no release', 'conn_kill', samples_of(lambda s: s['metrics'].update(released=0.0))),
            ('F5 host event', 'mr_pressure', samples_of(f5_unchanged)),
            ('F6 host event', 'disk_full', samples_of(lambda s: s.update(free=1 << 30))),
            ('F6 effect', 'disk_full', drop_ops(lambda e: e.get('state') == 'REFUSED')),
            ('F7 host event: fds never at the limit', 'fd_exhaust', samples_of(lambda s: s.update(fds=min(s['fds'], 50)))),
            ('F7 effect', 'fd_exhaust', samples_of(lambda s: s['metrics'].update(accept_errors=0.0))),
        ]
        for name, kind, edit in cases:
            with self.subTest(name=name):
                v = verdict(self, kind, edit)
                self.assertNotIn('error', v, v.get('error'))
                self.assertEqual(outcome(v, 'causal'), 'NOT_COVERED', v['criteria']['causal'])
                self.assertNotEqual(v['status'], 'PASS')

    def test_an_rdma_view_that_could_not_be_pid_joined(self):
        def edit(ev):  # the sampler records null when any row lacks a pid: never a zero count
            for s in ev['resource-samples.jsonl']:
                s['rdma'] = None
        v = verdict(self, 'control', edit)
        self.assertEqual((v['resource_classes']['qp'], v['resource_classes']['mr']), ('NOT_COVERED', 'NOT_COVERED'))
        self.assertEqual((outcome(v, 'resources'), v['status']), ('NOT_COVERED', 'NOT_COVERED'))

    def test_qp_and_mr_need_a_positive_witness_from_a_load_sample(self):
        """0 QPs / 0 MRs is what the sampler records on the lab when it sees only the kernel's GSI rows (the captured
        M02 table): that must read NOT_COVERED, never released, unless a load sample of the same volume counts its
        own rows. An MR witness is required because the row configures a pool (RDMA registrations expected)."""
        def rdma(qp, mr):
            return dict(qp=qp, mr=mr, mr_lens=[8 << 20] * mr)

        def phases(load, other):
            def edit(ev):
                for s in ev['resource-samples.jsonl']:
                    s['rdma'] = load if s['phase'] == 'load' else other
            return edit

        def moved_load(ev):  # the load samples belong to another process: not this volume's witness
            for s in ev['resource-samples.jsonl']:
                s['rdma'] = rdma(2, 2) if s['phase'] == 'load' else rdma(0, 0)
                if s['phase'] == 'load':
                    s['identity'] = dict(pid=2 ** 22 + 9, starttime=1)
        cases = [('idle GSI-only table in every sample', phases(rdma(0, 0), rdma(0, 0)), 'NOT_COVERED', 'NOT_COVERED'),
                 ('the volume rows during load', phases(rdma(2, 2), rdma(0, 0)), 'released', 'released'),
                 ('QPs seen, no MR although a pool is configured', phases(rdma(2, 0), rdma(0, 0)), 'released', 'NOT_COVERED'),
                 ('rows only before the load', lambda ev: [s.update(rdma=rdma(2, 2) if s['phase'] == 'pre_load' else rdma(0, 0))
                                                           for s in ev['resource-samples.jsonl']], 'NOT_COVERED', 'NOT_COVERED'),
                 ('load rows of another process', moved_load, 'NOT_COVERED', 'NOT_COVERED')]
        for name, edit, qp, mr in cases:
            with self.subTest(name=name):
                v = verdict(self, 'control', edit)
                self.assertNotIn('error', v, v.get('error'))
                self.assertEqual((v['resource_classes']['qp'], v['resource_classes']['mr']), (qp, mr), v['criteria']['resources'])
                reason = v['criteria']['resources']['reason']
                if 'NOT_COVERED' in (qp, mr):
                    self.assertEqual(outcome(v, 'resources'), 'NOT_COVERED')
                    self.assertIn('volume RDMA objects not visible to the sampler', reason)
                else:
                    self.assertNotIn('not visible', reason)

    def test_a_malformed_volume_list_in_the_evidence_is_not_covered(self):
        for bad in ({}, 'lmcache_3', [3, 4], [None]):
            with self.subTest(bad=bad):
                def edit(ev, bad=bad):
                    for s in ev['resource-samples.jsonl']:
                        s['volumes'] = bad
                v = verdict(self, 'control', edit)
                self.assertEqual(v['resource_classes']['data_fds'], 'NOT_COVERED')
                self.assertEqual((outcome(v, 'resources'), v['status']), ('NOT_COVERED', 'NOT_COVERED'))

    def test_no_volume_list_leaves_data_fds_not_covered(self):
        for missing in ('volumes', 'data_files'):
            with self.subTest(missing=missing):
                def edit(ev, missing=missing):
                    for s in ev['resource-samples.jsonl']:
                        s[missing] = None  # the product list (or the fd names) could not be read
                v = verdict(self, 'control', edit)
                self.assertEqual(v['resource_classes']['data_fds'], 'NOT_COVERED')
                self.assertEqual((outcome(v, 'resources'), v['status']), ('NOT_COVERED', 'NOT_COVERED'))

    def test_an_unattributable_socket_is_not_covered(self):
        def edit(ev):  # a peer no sample ever saw owned by a role or a client (e.g. a process on another host)
            for s in ev['resource-samples.jsonl']:
                if s['phase'] == 'quiescence':
                    s['tcp'] = s['tcp'] + [dict(state='ESTAB', local='127.0.0.1:8080', peer='198.51.100.9:40000')]
                    s.update(socket_fds=s['socket_fds'] + 1, fds=s['fds'] + 1)
        v = verdict(self, 'control', edit)
        self.assertEqual(v['resource_classes']['sockets'], 'NOT_COVERED')
        self.assertEqual((outcome(v, 'resources'), v['status']), ('NOT_COVERED', 'NOT_COVERED'))

    def test_missing_peer_data_is_not_covered(self):
        for missing in ('tcp', 'tcp_owners', 'socket_fds'):
            with self.subTest(missing=missing):
                def edit(ev, missing=missing):
                    for s in ev['resource-samples.jsonl']:
                        s[missing] = None
                v = verdict(self, 'control', edit)
                self.assertEqual(v['resource_classes']['sockets'], 'NOT_COVERED')
                self.assertEqual((outcome(v, 'resources'), v['status']), ('NOT_COVERED', 'NOT_COVERED'))

    def test_non_tcp_socket_growth_is_not_covered(self):
        def edit(ev):  # a socket fd with no TCP row (unix, udp, ...): nothing to attribute it to
            for s in ev['resource-samples.jsonl']:
                if s['phase'] == 'quiescence':
                    s.update(socket_fds=s['socket_fds'] + 1, fds=s['fds'] + 1)
        self.assertEqual(verdict(self, 'control', edit)['resource_classes']['sockets'], 'NOT_COVERED')

    def test_resources_need_a_real_pre_load_sample(self):
        """Review finding: the baseline is a pre-load sample provably taken before the load started, not the
        request to start the sampler."""
        def none(ev):
            ev['resource-samples.jsonl'] = [s for s in ev['resource-samples.jsonl'] if s['phase'] != 'pre_load']
        def late(ev):  # labelled pre_load, but sampled after the load began (M02 time past the load-start bound)
            for s in ev['resource-samples.jsonl']:
                if s['phase'] == 'pre_load':
                    s['epoch'] += 60
        def no_clock(ev):
            del ev['soak-clock-s2.json']
        for edit in (none, late, no_clock):
            with self.subTest(edit=edit.__name__):
                v = verdict(self, 'control', edit)
                self.assertNotIn('error', v, v.get('error'))
                self.assertEqual({k for k, x in v['resource_classes'].items() if x != 'NOT_COVERED'}, {'data_fds'})  # no baseline needed
                self.assertEqual((outcome(v, 'resources'), v['status']), ('NOT_COVERED', 'NOT_COVERED'))

    def test_every_row_verdict_states_its_layout_scope(self):
        self.assertEqual(verdict(self, 'link_flap')['layout_scope'], soak_assess.LAYOUT_SCOPE)
        v = verdict(self, 'link_flap', lambda ev: ev.pop('faults.json'))  # an error row carries it too
        self.assertEqual((v['status'], v['layout_scope']), ('FAIL', soak_assess.LAYOUT_SCOPE))

    def f5_refused_put(self, ev, at, op='777', state='REFUSED'):
        """The registration counters stay flat; one PUT ends `state` at M01 time `at`."""
        for s in ev['resource-samples.jsonl']:
            s['metrics']['registration_refused'] = 0.0
        seed = 'f5-%s' % op
        ev['write-ledger.jsonl'].append(dict(op=op, key='f5', state=state, seed=seed, length=64, digest=digest(seed), sequence=70,
                                             worker=1, dispatched=at - 0.2, submitted=at - 0.1, completed=at,
                                             error='write refused before commit; nothing was written: reg_mr failed: Cannot allocate memory',
                                             attributed=None, worker_identity=WID[1]))
        ev['op-events.jsonl'].insert(0, dict(op=op, worker=1, action='set', key='f5', dispatched=at - 0.2, submitted=at - 0.1,
                                             completed=at, ok=False, error='refused', state=state))
        next(m for m in ev['op-events.jsonl'] if m.get('event') == 'quiescent')['dispatched'] = int(op)
        renumber(ev)

    def test_f5_write_side_witness_is_a_refused_put_after_the_change(self):
        """Ruling 05:31Z: the REFUSED PUT, after the limit change, in the window, on the same volume pid/starttime."""
        v = verdict(self, 'mr_pressure', lambda ev: self.f5_refused_put(ev, 1115.0))
        self.assertEqual(outcome(v, 'causal'), 'PASS', v['criteria']['causal'])
        self.assertIn('REFUSED after the change', v['criteria']['causal']['reason'])

    def test_f5_refused_put_that_does_not_witness(self):
        def before(ev):  # ended before the limit was lowered
            self.f5_refused_put(ev, 1099.0)
        def after_window(ev):  # ended after the row window
            self.f5_refused_put(ev, 1300.0)
        def other_volume(ev):  # a volume restart inside the window: not the pid the limit was lowered on
            self.f5_refused_put(ev, 1115.0)
            for s in ev['resource-samples.jsonl']:
                if 1110 <= s['mono'] - M02 <= 1120:
                    s['identity'] = dict(REPL)
        def no_clock(ev):
            self.f5_refused_put(ev, 1115.0)
            del ev['soak-clock-s1.json']
        for edit in (before, after_window, other_volume, no_clock):
            with self.subTest(edit=edit.__name__):
                v = verdict(self, 'mr_pressure', edit)
                self.assertNotIn('error', v, v.get('error'))
                self.assertEqual(outcome(v, 'causal'), 'NOT_COVERED', v['criteria']['causal'])

    def test_f5_an_unknown_put_is_no_witness_and_still_fails(self):
        v = verdict(self, 'mr_pressure', lambda ev: self.f5_refused_put(ev, 1115.0, state='UNKNOWN'))
        self.assertEqual((outcome(v, 'causal'), outcome(v, 'write_contract')), ('NOT_COVERED', 'FAIL'))

    def test_f5_without_a_new_registration(self):
        def edit(ev):
            for s in ev['resource-samples.jsonl']:
                s['metrics']['registration_refused'] = 0.0
        v = verdict(self, 'mr_pressure', edit)
        self.assertEqual(outcome(v, 'causal'), 'NOT_COVERED')
        self.assertIn('no new registration', v['criteria']['causal']['reason'])
        self.assertEqual(v['status'], 'NOT_COVERED')

    def test_f1_op_that_ended_before_the_kill(self):
        def edit(ev):  # the PUT arm's op completed at 1100.1, before the kill was done (1100.3)
            for e in ev['op-events.jsonl'] + ev['write-ledger.jsonl']:
                if e.get('state') == 'UNKNOWN':
                    e['completed'] = 1100.1
        v = verdict(self, 'volume_kill_io', edit)
        self.assertEqual(outcome(v, 'causal'), 'NOT_COVERED')
        self.assertIn("['set']", v['criteria']['causal']['reason'])

    def test_f1_freeze_then_kill_needs_named_ops_dispatched_after_the_freeze(self):
        """F1's causal witness is freeze-then-kill: no witness, no named op, or an op dispatched before the freeze
        (it may have been served before the stop) is NOT_COVERED; the fixture's named ops after the freeze PASS."""
        def drop(ev):
            ev.pop('fault-freeze-witness-E1.json')

        def none_named(ev):  # the load named no PUT and GET within the bound
            ev['fault-freeze-witness-E1.json'].update(inflight=None, reason='no in-flight PUT and GET named within 5s')

        def before_freeze(ev):  # the stop came after both pending ops were dispatched
            w = ev['fault-freeze-witness-E1.json']
            w['freeze_mono'] = w['inflight']['freeze_mono'] = 1099.96

        def unnamed(ev):  # pending ops the load never named
            ev['fault-freeze-witness-E1.json']['inflight'].update(pending_sets=[dict(op='999', submitted=1099.9)])
        self.assertEqual(outcome(verdict(self, 'volume_kill_io'), 'causal'), 'PASS')
        for name, edit, why in (('no witness', drop, 'no freeze witness'), ('nothing named', none_named, 'no in-flight PUT and GET'),
                                ('ops from before the freeze', before_freeze, 'no named op pending'),
                                ('a PUT the load never named', unnamed, "['set']")):
            with self.subTest(name=name):
                v = verdict(self, 'volume_kill_io', edit)
                self.assertEqual(outcome(v, 'causal'), 'NOT_COVERED', v['criteria']['causal'])
                self.assertIn(why, v['criteria']['causal']['reason'])

    def test_f1_freeze_receipts_must_be_this_rows(self):
        """Through the real CLI: a freeze witness or an in-flight answer from another run, episode, stopped pid or
        starttime (or another boot or freeze clock, for the answer) is NOT_COVERED, never PASS."""
        def witness(**kw):
            return lambda ev: ev['fault-freeze-witness-E1.json'].update(kw)

        def answer(**kw):
            return lambda ev: ev['fault-freeze-witness-E1.json']['inflight'].update(kw)
        cases = [('witness of another run', witness(run_id='run-0'), 'not bound to this row'),
                 ('witness of another episode', witness(episode='E2'), 'not bound to this row'),
                 ('witness of another pid', witness(stopped_pid=501), 'not bound to this row'),
                 ('witness of another starttime', witness(stopped_starttime=10), 'not bound to this row'),
                 ('answer of another run', answer(run_id='run-0'), 'answer not bound'),
                 ('answer of another episode', answer(episode='E2'), 'answer not bound'),
                 ('answer of another pid', answer(stopped_pid=501), 'answer not bound'),
                 ('answer of another starttime', answer(stopped_starttime=10), 'answer not bound'),
                 ('answer of another boot', answer(boot_id='0' * 32), 'answer not bound'),
                 ('answer to another freeze', answer(freeze_mono=1099.7), 'answer not bound')]
        for name, edit, why in cases:
            with self.subTest(name=name):
                v = verdict(self, 'volume_kill_io', edit)
                self.assertEqual(outcome(v, 'causal'), 'NOT_COVERED', v['criteria']['causal'])
                self.assertIn(why, v['criteria']['causal']['reason'])

    def test_audit_missing_a_ledger_key(self):
        v = verdict(self, 'control', lambda ev: ev['audit.jsonl'].pop(0))
        self.assertEqual(outcome(v, 'audit'), 'NOT_COVERED')
        self.assertEqual(v['status'], 'NOT_COVERED')

    def test_audit_incomplete_or_not_fresh(self):
        for change in (dict(complete=False), dict(client=dict(WID[2]))):
            with self.subTest(change=change):
                def edit(ev, change=change):
                    next(m for m in ev['op-events.jsonl'] if m.get('event') == 'audit_done').update(change)
                self.assertEqual(outcome(verdict(self, 'control', edit), 'audit'), 'NOT_COVERED')

    def test_no_rdma_read_after_the_heal(self):
        def edit(ev):
            ev['op-events.jsonl'] = [e for e in ev['op-events.jsonl'] if not (e.get('action') == 'get' and 1130 <= e['submitted'] <= 1160)]
            renumber(ev)
        self.assertEqual(outcome(verdict(self, 'link_flap', edit), 'recovery'), 'NOT_COVERED')


def renumber(ev):
    """Op ids stay 1..N after a fixture drops ops (the terminal check counts them)."""
    ops = [e for e in ev['op-events.jsonl'] if 'action' in e]
    kill = {str(m['witness']['operation_id']) for m in ev['op-events.jsonl'] if m.get('event') == 'client_kill'}
    keep = sorted({e['op'] for e in ops} | kill, key=int)
    new = {old: str(i + 1) for i, old in enumerate(keep)}
    for e in ops + ev['write-ledger.jsonl']:
        e['op'] = new[e['op']]
    for m in ev['op-events.jsonl']:
        if m.get('event') == 'client_kill':
            m['witness']['operation_id'] = new[str(m['witness']['operation_id'])]
        if m.get('event') == 'quiescent':
            m['dispatched'] = len(keep)


class Fail(unittest.TestCase):
    def check(self, kind, edit, criterion):
        v = verdict(self, kind, edit)
        self.assertNotIn('error', v, v.get('error'))
        self.assertEqual(outcome(v, criterion), 'FAIL', v['criteria'][criterion])
        self.assertEqual(v['status'], 'FAIL')
        return v

    def test_one_corrupted_object(self):
        def edit(ev):
            g = next(e for e in ev['op-events.jsonl'] if e.get('action') == 'get' and e.get('ok'))
            g['sha256'] = '0' * 64
        self.check('control', edit, 'bytes')

    def test_a_wrong_audit_read_fails_whatever_a_later_read_returns(self):
        """Review finding: an earlier wrong-digest read (at another length, or the same) followed by a good read of
        the same key was satisfied by the retry. Every audit read counts."""
        for length in (128, 64):
            with self.subTest(length=length):
                def edit(ev, length=length):
                    u = next(x for x in ev['write-ledger.jsonl'] if x['state'] == 'UNKNOWN')
                    a = next(a for a in ev['audit.jsonl'] if a['key'] == u['key'])
                    a['reads'] = [dict(case='hit', sha256='4' * 64, length=length), dict(case='hit', sha256=u['digest'], length=64)]
                self.check('volume_kill_io', edit, 'audit')

    def test_an_audit_error_read_then_a_good_read_fails(self):
        def edit(ev):
            a = next(a for a in ev['audit.jsonl'] if a['key'] == 'k1')
            a['reads'] = [dict(case='unexpected', ok=False, error='timed out')] + a['reads']
        self.check('control', edit, 'audit')

    def test_an_ack_whose_digest_is_not_its_seeds_bytes(self):
        def edit(ev):
            ev['ack-journal.jsonl'][0]['digest'] = '3' * 64
        self.check('control', edit, 'bytes')

    def test_f2_second_unknown_of_the_killed_worker_is_not_the_killed_op(self):
        def edit(ev):  # same worker identity, inside the kill window, but not the op pending at the kill
            seed = 'other'
            ev['write-ledger.jsonl'].append(dict(op='999', key='z', state='UNKNOWN', seed=seed, length=64, digest=digest(seed), sequence=60,
                                                 worker=1, dispatched=1100.4, submitted=1100.5, completed=1100.9, error='reset',
                                                 attributed='E1', worker_identity=WID[1]))
            ev['op-events.jsonl'].insert(0, dict(op='999', worker=1, action='set', key='z', dispatched=1100.4, submitted=1100.5,
                                                 completed=1100.9, ok=False, error='reset', state='UNKNOWN'))
        v = self.check('client_kill_put', edit, 'write_contract')
        self.assertIn('UNKNOWN 999 outside the attributed window', v['criteria']['write_contract']['reason'])

    def test_partial_put_read_back_as_other_bytes(self):
        def edit(ev):  # the quarantined key returns neither its survivor nor the UNKNOWN generation's exact bytes
            next(a for a in ev['audit.jsonl'] if a['key'] == 'q')['reads'] = [dict(case='hit', sha256='1' * 64, length=64)]
        self.check('volume_kill_io', edit, 'audit')

    def test_partial_put_exact_unknown_bytes_is_allowed(self):
        def edit(ev):
            u = next(x for x in ev['write-ledger.jsonl'] if x['state'] == 'UNKNOWN')
            next(a for a in ev['audit.jsonl'] if a['key'] == 'q')['reads'] = [dict(case='hit', sha256=u['digest'], length=64)]
        self.assertEqual(outcome(verdict(self, 'volume_kill_io', edit), 'audit'), 'PASS')

    def test_unknown_retried(self):
        def edit(ev):
            n = max(int(e['op']) for e in ev['op-events.jsonl'] if 'op' in e) + 1
            seed = 'retry'
            ev['op-events.jsonl'].insert(0, dict(op=str(n), worker=1, action='set', key='q', dispatched=1200.0, submitted=1200.01,
                                                 completed=1200.05, ok=True, error=None, state='ACKED'))
            ev['write-ledger.jsonl'].append(dict(op=str(n), key='q', state='ACKED', seed=seed, length=64, digest=digest(seed), sequence=99,
                                                 worker=1, dispatched=1200.0, submitted=1200.01, completed=1200.05, error=None,
                                                 attributed=None, worker_identity=WID[1]))
            ev['ack-journal.jsonl'].append(dict(key='q', writer=1, seed=seed, submitted=1200.01, completed=1200.05, sequence=99, length=64,
                                                digest=digest(seed), fids=['3,ab']))
            next(m for m in ev['op-events.jsonl'] if m.get('event') == 'quiescent')['dispatched'] = n
        v = self.check('volume_kill_io', edit, 'write_contract')
        self.assertIn('retried', v['criteria']['write_contract']['reason'])

    def test_unknown_cleared_to_an_ack(self):
        def edit(ev):
            u = next(x for x in ev['write-ledger.jsonl'] if x['state'] == 'UNKNOWN')
            ev['ack-journal.jsonl'].append(dict(key=u['key'], writer=1, seed=u['seed'], submitted=u['submitted'], completed=u['completed'],
                                                sequence=u['sequence'], length=64, digest=u['digest'], fids=['3,ab']))
        v = self.check('volume_kill_io', edit, 'write_contract')
        self.assertIn('turned into an ACK', v['criteria']['write_contract']['reason'])

    def test_ack_superseded_without_a_later_ack(self):
        def edit(ev):  # k2's audit returns its older generation, not its survivor
            first = next(x for x in ev['write-ledger.jsonl'] if x['key'] == 'k2' and x['state'] == 'ACKED')
            next(a for a in ev['audit.jsonl'] if a['key'] == 'k2')['reads'] = [dict(case='hit', sha256=first['digest'], length=64)]
        self.check('control', edit, 'audit')

    def test_ack_lost_without_retirement(self):
        def edit(ev):
            next(a for a in ev['audit.jsonl'] if a['key'] == 'k1')['reads'] = [dict(case='miss')]
        self.check('control', edit, 'audit')

    def test_quarantined_key_lost_its_prior_ack_unread_by_the_load(self):
        def edit(ev):  # the load never read q after the fault; only the audit can catch the lost survivor
            next(a for a in ev['audit.jsonl'] if a['key'] == 'q')['reads'] = [dict(case='miss')]
        self.check('volume_kill_io', edit, 'audit')

    def test_no_prior_ack_returning_other_bytes(self):
        def edit(ev):
            next(a for a in ev['audit.jsonl'] if a['key'] == 'f2')['reads'] = [dict(case='hit', sha256='2' * 64, length=64)]
        self.check('client_kill_put', edit, 'audit')

    def test_no_prior_ack_exact_unknown_bytes_is_allowed(self):
        def edit(ev):
            u = next(x for x in ev['write-ledger.jsonl'] if x['state'] == 'UNKNOWN')
            next(a for a in ev['audit.jsonl'] if a['key'] == 'f2')['reads'] = [dict(case='hit', sha256=u['digest'], length=64)]
        self.assertEqual(outcome(verdict(self, 'client_kill_put', edit), 'audit'), 'PASS')

    def test_unknown_outside_the_attributed_window_treated_as_continuing(self):
        def edit(ev):
            for e in ev['op-events.jsonl'] + ev['write-ledger.jsonl']:
                if e.get('state') == 'UNKNOWN':
                    e.update(submitted=1200.0, dispatched=1199.99, completed=1200.5)
        v = self.check('volume_kill_io', edit, 'write_contract')
        self.assertIn('outside the attributed window', v['criteria']['write_contract']['reason'])

    def test_unknown_that_ended_before_the_kill_began(self):
        def edit(ev):
            for e in ev['op-events.jsonl'] + ev['write-ledger.jsonl']:
                if e.get('state') == 'UNKNOWN':
                    e.update(submitted=1099.5, dispatched=1099.49, completed=1099.95)  # pre-kill clock is 1100.0
        self.check('volume_kill_io', edit, 'write_contract')

    def test_volume_gap_or_change_in_a_row_that_kills_nothing(self):
        for change in ('gap', 'restart'):
            with self.subTest(change=change):
                def edit(ev, change=change):
                    ev['resource-samples.jsonl'][40]['identity'] = None if change == 'gap' else dict(REPL)
                self.check('link_flap', edit, 'failure_mode')

    def test_f1_replacement_is_not_the_receipt(self):
        def edit(ev):
            for s in ev['resource-samples.jsonl']:
                if s['identity'] == REPL:
                    s['identity'] = dict(pid=502, starttime=21)
        self.check('volume_kill_io', edit, 'failure_mode')

    def test_unknown_in_a_row_without_attribution(self):
        def edit(ev):
            ev['write-ledger.jsonl'].append(dict(op='999', key='z', state='UNKNOWN', seed='z', length=64, digest=digest('z'), sequence=50,
                                                 worker=1, dispatched=1105.0, submitted=1105.0, completed=1106.0, error='reset',
                                                 attributed='E1', worker_identity=WID[1]))
        self.check('link_flap', edit, 'write_contract')

    def test_f2_unknown_of_another_worker(self):
        def edit(ev):
            next(x for x in ev['write-ledger.jsonl'] if x['state'] == 'UNKNOWN')['worker_identity'] = WID[2]
        self.check('client_kill_put', edit, 'write_contract')

    def test_one_leaked_fd(self):
        def edit(ev):
            for s in ev['resource-samples.jsonl']:
                if s['phase'] == 'quiescence':
                    s['fds'] += 1
        v = self.check('control', edit, 'resources')
        self.assertEqual(v['resource_classes']['fds'], 'leak')

    def test_data_fds_match_the_volume_servers_own_list(self):
        v = verdict(self, 'control')
        self.assertEqual((v['resource_classes']['data_fds'], v['resource_classes']['fds']), ('accounted', 'released'))

    def test_a_data_fd_surplus_is_a_leak(self):
        """Ruling (option a): data fds leave the leak class only up to the product's own volume list."""
        for name, extra in (('unlisted volume', 'lmcache_9.dat'), ('second fd on one file', 'lmcache_3.dat')):
            with self.subTest(name=name):
                def edit(ev, extra=extra):
                    for s in ev['resource-samples.jsonl']:
                        if s['phase'] == 'quiescence':
                            s.update(fds=s['fds'] + 1, data_fds=5, data_files=sorted(DATA + [extra]))
                v = self.check('control', edit, 'resources')
                self.assertEqual((v['resource_classes']['data_fds'], v['resource_classes']['fds']), ('leak', 'released'))

    def test_non_data_fd_growth_stays_a_leak(self):
        def edit(ev):  # the same 4 accounted data fds, plus one fd that is not a volume file
            for s in ev['resource-samples.jsonl']:
                if s['phase'] == 'quiescence':
                    s['fds'] += 1
        v = self.check('control', edit, 'resources')
        self.assertEqual((v['resource_classes']['data_fds'], v['resource_classes']['fds']), ('accounted', 'leak'))

    def quiet_sockets(self, ev, extra, owners=None, socket_fds=None):
        for s in ev['resource-samples.jsonl']:
            if s['phase'] == 'quiescence':
                s['tcp'] = s['tcp'] + extra
                s['tcp_owners'] = dict(s['tcp_owners'], **(owners or {}))
                s['socket_fds'] += len(extra) if socket_fds is None else socket_fds
                s['fds'] += len(extra) if socket_fds is None else socket_fds

    def test_cluster_sockets_within_the_cap_are_infrastructure(self):
        v = verdict(self, 'control')  # the filer's one new connection: baseline 0 + 1
        self.assertEqual((v['resource_classes']['sockets'], v['resource_classes']['fds']), ('infrastructure', 'released'))

    def test_cluster_sockets_over_the_cap_are_a_leak(self):
        conn = dict(state='ESTAB', local='127.0.0.1:8080', peer='127.0.0.1:41001')
        v = self.check('control', lambda ev: self.quiet_sockets(ev, [conn], {'127.0.0.1:41001': dict(FILER)}), 'resources')
        self.assertEqual(v['resource_classes']['sockets'], 'leak')  # the filer now holds 2 > 0 + 1

    def test_a_socket_of_an_exited_client_is_a_leak(self):
        conn = dict(state='ESTAB', local='127.0.0.1:8080', peer='127.0.0.1:52000')
        def edit(ev):
            for s in ev['resource-samples.jsonl']:
                if s['phase'] == 'load':  # while the load ran, a client owned the peer endpoint
                    s['tcp_owners'] = dict(s['tcp_owners'], **{'127.0.0.1:52000': dict(WID[0])})
            self.quiet_sockets(ev, [conn])  # at quiescence the client is gone; the volume's socket remains
        v = self.check('control', edit, 'resources')
        self.assertEqual(v['resource_classes']['sockets'], 'leak')

    def test_a_socket_of_a_client_still_open_at_quiescence_is_a_leak(self):
        conn = dict(state='ESTAB', local='127.0.0.1:8080', peer='127.0.0.1:52001')
        v = self.check('control', lambda ev: self.quiet_sockets(ev, [conn], {'127.0.0.1:52001': dict(WID[1])}), 'resources')
        self.assertEqual(v['resource_classes']['sockets'], 'leak')

    def test_a_client_socket_in_any_connected_state_is_a_leak(self):
        """Review P1: retained client sockets are not only ESTAB. CLOSE-WAIT, SYN-SENT and the other non-LISTEN
        states are classified by peer exactly like ESTAB."""
        for state in ('CLOSE-WAIT', 'SYN-SENT', 'FIN-WAIT-1', 'LAST-ACK', 'SYN-RECV', 'CLOSING'):
            with self.subTest(state=state):
                conn = dict(state=state, local='127.0.0.1:8080', peer='127.0.0.1:52002')
                def edit(ev, conn=conn):
                    for s in ev['resource-samples.jsonl']:
                        if s['phase'] == 'load':
                            s['tcp_owners'] = dict(s['tcp_owners'], **{'127.0.0.1:52002': dict(WID[2])})
                    self.quiet_sockets(ev, [conn])
                v = self.check('control', edit, 'resources')
                self.assertEqual(v['resource_classes']['sockets'], 'leak')

    def test_a_role_peer_in_close_wait_counts_toward_its_cap(self):
        within = dict(state='CLOSE-WAIT', local='127.0.0.1:8080', peer='127.0.0.1:41000')  # the filer's one socket, now closing
        def only_close_wait(ev):
            for s in ev['resource-samples.jsonl']:
                if s['phase'] != 'pre_load':
                    s['tcp'] = TCP + [within]
        self.assertEqual(verdict(self, 'control', only_close_wait)['resource_classes']['sockets'], 'infrastructure')
        over = dict(state='CLOSE-WAIT', local='127.0.0.1:8080', peer='127.0.0.1:41002')
        v = self.check('control', lambda ev: self.quiet_sockets(ev, [over], {'127.0.0.1:41002': dict(FILER)}), 'resources')
        self.assertEqual(v['resource_classes']['sockets'], 'leak')  # ESTAB + CLOSE-WAIT to the filer: 2 > 0 + 1

    def test_an_unattributable_socket_in_close_wait_is_not_covered(self):
        def edit(ev):
            for s in ev['resource-samples.jsonl']:
                if s['phase'] == 'quiescence':
                    s['tcp'] = s['tcp'] + [dict(state='CLOSE-WAIT', local='127.0.0.1:8080', peer='198.51.100.9:40001')]
                    s.update(socket_fds=s['socket_fds'] + 1, fds=s['fds'] + 1)
        self.assertEqual(verdict(self, 'control', edit)['resource_classes']['sockets'], 'NOT_COVERED')

    def test_a_listener_above_baseline_is_a_leak(self):
        v = self.check('control', lambda ev: self.quiet_sockets(ev, [dict(state='LISTEN', local='127.0.0.1:9999', peer='*:*')]), 'resources')
        self.assertEqual(v['resource_classes']['sockets'], 'leak')

    def test_leaked_qp_row_even_with_custody_reported(self):
        def edit(ev):
            for s in ev['resource-samples.jsonl']:
                if s['phase'] == 'quiescence':
                    s['rdma']['qp'] += 1
                    s['metrics'].update(retained_bytes=float(1 << 20), retained_sources=1.0)
        v = self.check('control', edit, 'resources')
        self.assertEqual(v['resource_classes']['qp'], 'leak')

    def test_retained_over_budget_or_growing_is_a_leak(self):
        for held in ([8 << 20] * 3, [1 << 20, 2 << 20, 3 << 20]):
            with self.subTest(held=held):
                def edit(ev, held=held):
                    quiet = [s for s in ev['resource-samples.jsonl'] if s['phase'] == 'quiescence']
                    for s in quiet:
                        s['vm_pin_kB'] += 1024
                        s['metrics'].update(retained_bytes=float(held[0]), retained_sources=1.0)
                    for s, b in zip(quiet[-3:], held):
                        s['metrics']['retained_bytes'] = float(b)
                self.check('control', edit, 'resources')

    def test_the_pin_bound_is_the_configured_sizes(self):
        def at_the_bound(ev):  # 7 MiB pinned = configured 4 + 2 + 1: legal only with the configured cache counted
            ev['resource-samples.jsonl'][30]['metrics']['pinned'] = float(7 << 20)
        self.assertEqual(outcome(verdict(self, 'control', at_the_bound), 'resources'), 'PASS')
        def one_byte_over(ev):
            ev['resource-samples.jsonl'][30]['metrics']['pinned'] = float((7 << 20) + 1)
        self.assertEqual(outcome(verdict(self, 'control', one_byte_over), 'resources'), 'FAIL')
        def smaller_pool(ev):  # the configured pool alone sets the term: 6 MiB + 1 pinned is over 4 + 1 + 1
            ev['resource-samples.jsonl'][30]['metrics']['pinned'] = float((6 << 20) + 1)
        v = verdict(self, 'control', smaller_pool, fix=lambda c: c['fault_row'].update(pin_pool_bytes=1 << 20))
        self.assertEqual(outcome(v, 'resources'), 'FAIL')

    def test_an_inflated_product_gauge_never_widens_the_bound(self):
        """Ruling 06:05Z: the product under test cannot set its own bound. A pool gauge of 5 MiB would make 8 MiB
        legal under 4 + 5 + 1; against the configured 2 MiB pool it is over, and the disagreement is reported."""
        def inflated(ev):
            ev['resource-samples.jsonl'][30]['metrics'].update(pinned=float(8 << 20), pool_bytes=float(5 << 20))
        v = verdict(self, 'control', inflated)
        self.assertEqual(outcome(v, 'resources'), 'FAIL')
        self.assertIn('pool_bytes gauge 5242880 > configured 2097152', v['criteria']['resources']['reason'])

    def test_a_pin_bound_without_a_configured_size_is_not_covered(self):
        for key, value in (('pin_pool_bytes', None), ('pin_cache_bytes', None), ('pin_pool_bytes', '2MiB'), ('pin_cache_bytes', True)):
            with self.subTest(key=key, value=value):
                v = verdict(self, 'control', fix=lambda c, key=key, value=value: c['fault_row'].update({key: value}))
                self.assertEqual((outcome(v, 'resources'), v['status']), ('NOT_COVERED', 'NOT_COVERED'), v['criteria']['resources'])

    def test_pin_over_the_bound_and_a_whole_dat_pin(self):
        def over(ev):
            ev['resource-samples.jsonl'][30]['metrics']['pinned'] = float(BOUND + 1)
        def whole(ev):
            ev['resource-samples.jsonl'][30]['rdma']['mr_lens'] = [33554432]
        for edit in (over, whole):
            with self.subTest(edit=edit.__name__):
                self.check('control', edit, 'resources')

    def test_untargeted_process_crash(self):
        def edit(ev):
            ev['resource-samples.jsonl'][40]['watch']['master'] = dict(pid=601, starttime=99)
        self.check('link_flap', edit, 'failure_mode')

    def test_op_past_the_limit(self):
        def edit(ev):
            g = next(e for e in ev['op-events.jsonl'] if e.get('action') == 'get')
            g['completed'] = g['submitted'] + 31.5
        self.check('control', edit, 'failure_mode')

    def test_r0_reports_a_fault_when_one_happened(self):
        for mutate in ('port', 'limits', 'identity'):
            with self.subTest(mutate=mutate):
                def edit(ev, mutate=mutate):
                    s = ev['resource-samples.jsonl'][30]
                    if mutate == 'port':
                        s['port'] = ['1: DOWN', 1]
                        for x in ev['resource-samples.jsonl'][31:]:
                            x['port'] = ['4: ACTIVE', 1]
                    elif mutate == 'limits':
                        s['limits']['memlock'] = [1 << 20, -1]
                    else:
                        for x in ev['resource-samples.jsonl'][30:]:
                            x['identity'] = dict(REPL)
                v = verdict(self, 'control', edit)
                self.assertEqual(outcome(v, 'causal'), 'FAIL', v['criteria']['causal'])

    def test_failed_recovery(self):
        def edit(ev):
            for e in ev['op-events.jsonl']:
                if e.get('action') == 'get' and 1130 <= e['submitted'] <= 1160:
                    e.update(ok=False, fault_error=True, episode='E1', error='operation timed out')
                    e.pop('sha256', None)
                    e.pop('length', None)
        self.check('link_flap', edit, 'recovery')

    def test_a_moved_recovery_bound_is_refused(self):
        v = verdict(self, 'link_flap', fix=lambda c: c['fault_row'].update(recovery_s=45))
        self.assertEqual(v['status'], 'FAIL')
        self.assertIn('recovery bound moved', v['error'])


# ---- F3 causal witness on a VF (ruling claude01 2026-11-11 05:16Z, r15): pid-bound port state ACTIVE -> DOWN ->
# ACTIVE inside the episode window (M02 clock; the DOWN sampled) AND carrier_down_count or carrier_changes +1;
# link_downed recorded, not required. The fixture's episode window: apply.before 1100 to restore.after 1130 + 30.
def vf_flap(ev, before=(5, 10), after=(6, 11), end=(6, 12), vf=True, down=(1100, 1130), carrier_in_samples=True,
            sample_ident=None):
    """r14's shape on the F3 fixture: link_downed 0 everywhere; the VF's carrier counters in the injector's rows and
    the samples; the sampled DOWN where `down` says (M01-fixture time, written at M02 = + M02)."""
    c = lambda v: None if v is None else dict(down=v[0], changes=v[1], vf=vf)
    ap, rs = ev['faults.json'][0]['injection']
    ap['before'].update(port=['4: ACTIVE', 0], carrier=c(before))
    ap['after'].update(port=['1: DOWN', 0], carrier=c(after))
    rs['after'].update(port=['4: ACTIVE', 0], carrier=c(end))
    for s in ev['resource-samples.jsonl']:
        t = s['mono'] - M02
        s['port'] = ['1: DOWN', 0] if down and down[0] < t <= down[1] else ['4: ACTIVE', 0]
        if carrier_in_samples:
            s['carrier'] = c(before if t <= 1100 else after if t <= 1130 else end)
        if sample_ident and down and down[0] < t <= down[1]:
            s['identity'] = dict(sample_ident)


class VfLinkFlapCausal(unittest.TestCase):
    def causal(self, **kw):
        v = verdict(self, 'link_flap', lambda ev: vf_flap(ev, **kw))
        self.assertNotIn('error', v, v.get('error'))
        return v['criteria']['causal']

    def test_state_and_carrier_on_a_vf_is_credited(self):
        c = self.causal()
        self.assertEqual(c['outcome'], 'PASS', c)

    def test_carrier_changes_alone_is_enough(self):
        c = self.causal(after=(5, 11), end=(5, 12))
        self.assertEqual(c['outcome'], 'PASS', c)

    def test_carrier_down_count_alone_is_enough(self):
        c = self.causal(after=(6, 10), end=(6, 10))
        self.assertEqual(c['outcome'], 'PASS', c)

    def test_a_port_already_down_at_the_episode_start_is_not_covered(self):
        def edit(ev):
            vf_flap(ev, down=(1095, 1130))  # sampled DOWN from before the episode; the injector saw it DOWN too
            ev['faults.json'][0]['injection'][0]['before']['port'] = ['1: DOWN', 0]
        v = verdict(self, 'link_flap', edit)
        self.assertEqual(v['criteria']['causal']['outcome'], 'NOT_COVERED', v['criteria']['causal'])
        self.assertIn('never ACTIVE -> DOWN', v['criteria']['causal']['reason'])

    def test_a_port_still_down_at_the_window_end_is_not_covered(self):
        def edit(ev):  # sampled DOWN through the window end, and the injector's after-restore row still DOWN
            vf_flap(ev, down=(1100, 1300))
            ev['faults.json'][0]['injection'][1]['after']['port'] = ['1: DOWN', 0]
        v = verdict(self, 'link_flap', edit)
        self.assertEqual(v['criteria']['causal']['outcome'], 'NOT_COVERED', v['criteria']['causal'])
        self.assertIn('never ACTIVE -> DOWN', v['criteria']['causal']['reason'])

    def test_state_without_a_carrier_delta_is_not_covered(self):
        c = self.causal(after=(5, 10), end=(5, 10))
        self.assertEqual(c['outcome'], 'NOT_COVERED', c)
        self.assertIn('carrier did not move', c['reason'])

    def test_carrier_delta_without_the_state_transition_is_not_covered(self):
        c = self.causal(down=None)
        self.assertEqual(c['outcome'], 'NOT_COVERED', c)
        self.assertIn('never ACTIVE -> DOWN', c['reason'])

    def test_the_state_transition_outside_the_episode_window_is_not_covered(self):
        c = self.causal(down=(1200, 1230))  # sampled DOWN, but after restore.after + recovery (1160)
        self.assertEqual(c['outcome'], 'NOT_COVERED', c)
        self.assertIn('never ACTIVE -> DOWN', c['reason'])

    def test_a_down_sampled_on_another_volume_identity_is_not_covered(self):
        c = self.causal(sample_ident=REPL)
        self.assertEqual(c['outcome'], 'NOT_COVERED', c)

    def test_an_unreadable_carrier_is_not_covered(self):
        for kw in (dict(before=None), dict(end=None)):
            with self.subTest(**{k: 'unreadable' for k in kw}):
                c = self.causal(**kw)
                self.assertEqual(c['outcome'], 'NOT_COVERED', c)
                self.assertIn('unreadable', c['reason'])

    def test_the_f3_r14_evidence_shape_is_not_covered(self):
        # r14: port DOWN sampled 14 times, link_downed 0 throughout, no carrier recorded at all
        def edit(ev):
            vf_flap(ev, carrier_in_samples=False)
            for r in ev['faults.json'][0]['injection']:
                for k in ('before', 'after'):
                    (r.get(k) or {}).pop('carrier', None)
        v = verdict(self, 'link_flap', edit)
        self.assertEqual(v['criteria']['causal']['outcome'], 'NOT_COVERED', v['criteria']['causal'])
        self.assertIn('link_downed', v['criteria']['causal']['reason'])

    def test_a_link_flap_on_a_non_vf_keeps_the_link_downed_rule(self):
        c = self.causal(vf=False)  # carrier moved but link_downed stayed 0: today's rule, not credited
        self.assertEqual(c['outcome'], 'NOT_COVERED', c)
        self.assertIn('link_downed delta', c['reason'])


# ---- F3 link flap attribution (ruling 2026-11-11 01:54Z), through the real CLI. The window is re-derived from the
# controller's M01 marks only: fault-pause pause_mono (before the apply is issued) and fault-close end_mono (after
# the restore proved restored=true) + recovery_s. The fixture's controller marks: dispatch 1100.0, ready 1130.0.
PAUSE_M01, CLOSE_M01 = 1099.9, 1130.01
FLAP_END = CLOSE_M01 + 30
TIMEOUT = "TimeoutError('native completion deadline')"


def flap_reason(submitted, **kw):
    """The receipt the load writes for one attributed UNKNOWN (LinkFlapWindow.reason)."""
    r = dict(run_id='run-1', boot_id=boot_id(), episode='E1', kind='link_flap', netdev='enp1s0v0', rdma_device='mlx5_0',
             start=PAUSE_M01, end=None, start_mark='fault-pause pause_mono', end_mark='fault-close end_mono + recovery_s',
             case='in flight at link-down' if submitted < PAUSE_M01 else 'submitted in the down window')
    r.update(kw)
    return r


def mixed(new_seed, old_seed, length=64):
    """A partial PUT read back: the first half of the new generation over the old one."""
    half = length // 2
    return hashlib.sha256(payload(new_seed, length)[:half] + payload(old_seed, length)[half:]).hexdigest()


def flap_verdict(test, submitted=1100.2, completed=1130.2, prior=True, audit='old', reason=None, edit=None, fix=None):
    """The F3 fixture row plus key f3: an optional prior ACK, then one quarantined UNKNOWN PUT with its receipt and
    the fresh audit's reads (the load's order: survivor first, then the UNKNOWN generation, stopping at an exact
    match or a miss); the controller's M01 pause/close marks bound to this run and boot."""
    r = row_evidence('link_flap')
    old = r.put('f3', 1050.0) if prior else None
    u = r.put('f3', submitted, state='UNKNOWN', done=completed, attributed='E1', error=TIMEOUT)
    u['reason'] = flap_reason(submitted) if reason is None else reason
    hit = lambda d: dict(case='hit', sha256=d, length=64)
    new = u['digest']
    reads = {'old': [hit(old['digest'])] if prior else None,
             'new': [hit(new), hit(new)] if prior else [hit(new)],
             'miss': [dict(case='miss')],
             'corrupt': [hit('9' * 64)] * (2 if prior else 1),
             'partial': [hit(mixed(u['seed'], old['seed']))] * 2 if prior else None}[audit]
    r.audit['f3'] = reads
    ev = files(r, 'link_flap')
    bind = dict(run_id='run-1', boot_id=boot_id(), episode='E1', role='volume')
    ev['fault-pause-E1.json'] = dict(bind, pause_mono=PAUSE_M01)
    ev['fault-close-E1.json'] = dict(bind, end_mono=CLOSE_M01)
    if edit:
        edit(ev)
    root = Path(tempfile.mkdtemp())
    test.addCleanup(shutil.rmtree, root)
    for name, body in ev.items():
        (root / name).write_text(''.join(json.dumps(x) + '\n' for x in body) if name.endswith('.jsonl') else json.dumps(body))
    c = config(root, 'link_flap')
    if fix:
        fix(c)
    out = subprocess.run([sys.executable, '-I', str(HERE / 'soak_assess.py'), '--config', json.dumps(c)],
                         capture_output=True, text=True, timeout=60)
    test.assertEqual(out.returncode, 0, out.stderr)
    return json.loads(out.stdout)


class LinkFlapAttribution(unittest.TestCase):
    def passes(self, v):
        self.assertNotIn('error', v, v.get('error'))
        self.assertEqual({k: x['outcome'] for k, x in v['criteria'].items() if x['outcome'] != 'PASS'}, {}, v['criteria'])
        self.assertEqual((v['status'], v['unknown_attributed']), ('PASS', 1))

    def refused(self, v, criterion='write_contract', why='outside the attributed window'):
        self.assertNotIn('error', v, v.get('error'))
        self.assertEqual({k: x['outcome'] for k, x in v['criteria'].items() if x['outcome'] != 'PASS'}, {criterion: 'FAIL'}, v['criteria'])
        self.assertIn(why, v['criteria'][criterion]['reason'])

    # (1) in the window: attributed, quarantined, the row passes
    def test_an_unknown_submitted_in_the_down_window_is_attributed(self):
        self.passes(flap_verdict(self))

    def test_an_unknown_in_flight_at_link_down_is_attributed(self):
        self.passes(flap_verdict(self, submitted=1099.0, completed=1101.0))

    def test_an_unknown_submitted_after_the_close_within_the_recovery_bound_is_attributed(self):
        self.passes(flap_verdict(self, submitted=1140.0, completed=1145.0, reason=flap_reason(1140.0, end=FLAP_END)))

    # (2) outside the window: unattributed, the write contract fails
    def test_an_unknown_that_ended_before_the_pause_mark_is_refused(self):
        self.refused(flap_verdict(self, submitted=1095.0, completed=1099.5))

    def test_an_unknown_submitted_after_the_window_end_is_refused(self):
        self.refused(flap_verdict(self, submitted=1161.0, completed=1165.0, reason=flap_reason(1161.0, end=FLAP_END)))

    # (3) a quarantined key read back as partial or wrong bytes fails the audit
    def test_a_quarantined_key_read_back_corrupt_or_partial_fails(self):
        for audit in ('corrupt', 'partial'):
            with self.subTest(audit=audit):
                self.refused(flap_verdict(self, audit=audit), 'audit', 'failed')

    def test_a_quarantined_key_with_a_prior_ack_read_back_missing_fails(self):
        self.refused(flap_verdict(self, audit='miss'), 'audit', 'failed')

    # (4) the old value or the new value passes; a miss only without a prior ACK (F2's rule)
    def test_a_quarantined_key_read_back_old_or_new_passes(self):
        for audit in ('old', 'new'):
            with self.subTest(audit=audit):
                self.passes(flap_verdict(self, audit=audit))

    def test_a_quarantined_key_without_a_prior_ack_may_read_back_new_or_missing(self):
        for audit in ('new', 'miss'):
            with self.subTest(audit=audit):
                self.passes(flap_verdict(self, prior=False, audit=audit))

    # (5) a bound on another host's clock cannot make the window
    def test_a_close_bound_to_another_hosts_boot_is_refused(self):
        def edit(ev):
            ev['fault-close-E1.json']['boot_id'] = M02_BOOT
        self.refused(flap_verdict(self, edit=edit))

    def test_a_pause_on_the_m02_clock_is_refused(self):
        def edit(ev):  # the injector's M02 monotonic value (fixture: M02 = M01 + 4000) in place of the M01 mark
            ev['fault-pause-E1.json']['pause_mono'] = PAUSE_M01 + M02
        self.refused(flap_verdict(self, edit=edit))

    def test_a_receipt_recorded_against_the_m02_clock_is_refused(self):
        self.refused(flap_verdict(self, reason=flap_reason(1100.2, start=1100.0 + M02)))

    def test_a_receipt_naming_another_end_is_refused(self):
        for end in (CLOSE_M01, CLOSE_M01 + M02 + 30):  # without the recovery bound; on the M02 clock
            with self.subTest(end=end):
                self.refused(flap_verdict(self, reason=flap_reason(1100.2, end=end)))

    def test_an_unknown_attributed_to_another_episode_is_refused(self):
        def edit(ev):
            next(x for x in ev['write-ledger.jsonl'] if x['state'] == 'UNKNOWN')['attributed'] = 'E2'
        self.refused(flap_verdict(self, edit=edit))

    def test_a_missing_or_out_of_order_mark_is_refused(self):
        cases = {'no pause': lambda ev: ev.pop('fault-pause-E1.json'),
                 'no close': lambda ev: ev.pop('fault-close-E1.json'),
                 'close before ready': lambda ev: ev['fault-close-E1.json'].update(end_mono=1129.0),
                 'pause after dispatch': lambda ev: ev['fault-pause-E1.json'].update(pause_mono=1100.05)}
        for name, edit in cases.items():
            with self.subTest(case=name):
                self.refused(flap_verdict(self, reason=flap_reason(1100.2, start=1100.05) if name == 'pause after dispatch' else None,
                                          edit=edit))

    def test_a_receipt_not_bound_to_this_row_is_refused(self):
        for field, value in (('run_id', 'run-2'), ('boot_id', M02_BOOT), ('episode', 'E2'), ('kind', 'conn_kill'),
                             ('netdev', 'enp1s0np0'), ('rdma_device', 'mlx5_1')):
            with self.subTest(field=field):
                self.refused(flap_verdict(self, reason=flap_reason(1100.2, **{field: value})))
        self.refused(flap_verdict(self, reason='link flap'))  # no receipt at all

    # r12 (b): late completions are counted for the reader and never used by a criterion
    @staticmethod
    def late_row(ev, **kw):
        u = next(x for x in ev['write-ledger.jsonl'] if x['state'] == 'UNKNOWN')
        ev['late-completions.jsonl'].append(dict(dict(
            run_id='run-1', episode='E1', worker=1, worker_identity=WID[1], op=u['op'], action='set', key=u['key'], fid=7,
            ok=True, error='', results=None, worker_mono=1131.0, received_mono=1131.01, ledger_state='UNKNOWN',
            quarantined=True), **kw))

    def test_a_late_ok_completion_is_counted_and_changes_no_criterion(self):
        v = flap_verdict(self, edit=self.late_row)
        self.passes(v)
        self.assertEqual(v['late_completions'], 1)
        self.assertEqual(flap_verdict(self)['late_completions'], 0)

    def test_a_late_completion_of_another_run_is_refused(self):
        for field, value in (('run_id', 'run-2'), ('episode', 'E2')):
            with self.subTest(field=field):
                v = flap_verdict(self, edit=lambda ev, field=field, value=value: self.late_row(ev, **{field: value}))
                self.assertIn('late completion not bound', v.get('error', ''))
                self.assertEqual(v['status'], 'FAIL')

    def test_only_f3_reports_late_completions(self):
        for kind in ('control', 'volume_kill_io', 'client_kill_put', 'conn_kill'):
            with self.subTest(kind=kind):
                v = verdict(self, kind)
                self.assertNotIn('error', v, v.get('error'))
                self.assertNotIn('late_completions', v)

    def test_an_assessor_without_the_target_attributes_nothing(self):
        for field in ('netdev', 'rdma_device'):
            with self.subTest(field=field):
                self.refused(flap_verdict(self, fix=lambda c, field=field: c['fault_row'].pop(field)))
                # even a receipt that names no target either
                self.refused(flap_verdict(self, reason=flap_reason(1100.2, **{field: None}),
                                          fix=lambda c, field=field: c['fault_row'].pop(field)))


if __name__ == '__main__':
    unittest.main()
