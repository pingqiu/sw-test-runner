"""M01 soak verdict (step 3b): one pinned job that recomputes every accepted
criterion from the run's own evidence files, never from the load's summary.
Reuses soak_oracle (write/ack replay, Version.matches), soak_native_worker.payload
(digests) and soak_load's strict reader, classifiers and poll format. Prints one
bounded verdict row bound to its run; resources are NOT_OBSERVED by ruling."""
import argparse, datetime, json, math, sys, types
from pathlib import Path

if 'soak_load' not in sys.modules:  # -I drops the script dir: the exact protected sibling, which pins its own
    _source = Path(__file__).resolve().with_name('soak_load.py')
    _module = sys.modules['soak_load'] = types.ModuleType('soak_load')
    _module.__file__ = str(_source)
    exec(compile(_source.read_bytes(), str(_source), 'exec'), _module.__dict__)
from soak_native_worker import k8_refusal_misses, payload
from soak_oracle import Oracle
from soak_load import MISS, TRANSIENT, classify_get, read_bound, read_evidence
from soak_stats import WITNESS, boot_id

SCHEMA = 'seaweed-soak-verdict/1'
MANDATORY = ('wrong_reads', 'failed_sets', 'get_outcomes', 'terminal', 'fault_errors', 'misses', 'coverage', 'body',
             'client_kill', 'rotation', 'max_vid_advanced', 'routes')  # Go's soakCriteria, same set
KV_LOAD = ('wrong_reads', 'failed_sets', 'get_outcomes', 'terminal', 'fault_errors', 'misses', 'write_once', 'body',
           'client_kill', 'load_as_declared', 'routes')  # Go's kvLoadCriteria, same set
FILE, LINE, ROWS, OBJECT = 64 << 20, 64 << 10, 200000, 4 << 20
OP_ROW = 2 << 10  # kv_load's bound per op-events row (a full run averaged about 372 bytes)
CLOCK_CEILING_S, CLOCK_ANOMALY_S = 2.0, 0.1  # owner-chosen and frozen (decision 3 r6): never raised to pass misses
COUNTERS = ('direct_dc_reads', 'direct_rc_reads', 'copied_reads')


def typed(row, **spec):
    """Every named field present with its type; numbers finite, never bool."""
    if not isinstance(row, dict):
        raise ValueError('evidence row is not an object')
    for k, t in spec.items():
        v = row.get(k)
        ok = (not isinstance(v, bool) and isinstance(v, (int, float)) and math.isfinite(v)) if t is float else \
            (isinstance(v, int) and not isinstance(v, bool)) if t is int else isinstance(v, t)
        if not ok:
            raise ValueError('evidence field %s missing or mistyped' % k)
    return row


def op_event_bounds(pat):
    """(rows, bytes) op-events.jsonl may hold. The soak: its fixed caps. kv_load: what its declared
    pattern can write (each key's write and its verification, every operation of every timed phase at
    the declared rate, a margin per client and for the marks), at OP_ROW bytes a row, never below the
    soak's caps. A full kv_load writes more than 64 MiB."""
    if not pat:
        return ROWS, FILE
    ops = 2 * pat['keys'] + sum(pat['clients'] * pat['rate_per_client'] * ph['duration_s'] for ph in pat['phases'][1:])
    rows = math.ceil(ops) + 2 * pat['clients'] + 16
    return max(ROWS, rows), max(FILE, rows * OP_ROW)


def lines(root, name, bounds=None):
    max_rows, limit = bounds or (ROWS, FILE)
    rows = read_evidence(root / name, limit, LINE, max_rows)
    if rows is None:
        raise ValueError('evidence missing: ' + name)
    return rows


def obj(root, name):
    row = read_evidence(root / name, OBJECT)
    if row is None:
        raise ValueError('evidence missing: ' + name)
    return row


def get_case(e):
    """Every GET outcome is exactly one recognised case; anything else is unknown."""
    case = {(True, False, False): 'hit', (False, True, False): 'miss', (True, True, False): 'miss', (False, False, True): 'fault'}.get(
        (e['ok'], e.get('miss') is True, e.get('fault_error') is True), 'unknown')
    try:
        if case == 'hit':
            typed(e, length=int, sha256=str, sequence=int)
        elif case == 'miss':
            typed(e, sequence=int, fids=list)
            if e.get('cause', 'budget_refusal') != 'budget_refusal':
                return 'unknown'  # the one recorded miss cause
            if not e['ok']:  # r9: an ok=False miss only in the DC status-1 shape; ok=True not-hit is a candidate
                case = case if MISS.search(typed(e, error=str)['error']) else 'unknown'
            elif classify_get(dict(ok=True, results=typed(e, results=list)['results']), False) != 'miss':
                case = 'unknown'  # an ok=True miss needs the worker's non-hit results shape, never None/[True]/absent
        elif case == 'fault':
            typed(e, sequence=int, episode=str)
    except ValueError:
        return 'unknown'
    return case


def clock_bracket(c, s1, s2):
    """Decision 3 r6 section 2: the M02-epoch-minus-M01-mono offset interval from paired samples in two jobs. Holds only
    under the conditional claim: the offset moved monotonically, or within the bracket, between the samples."""
    bounds = []
    for row in (s1, s2):
        typed(row, boot_id=str, run_id=str, t1=float, t4=float, entry_epoch=float, exit_epoch=float, entry_mono=float, exit_mono=float)
        if row['boot_id'] != c['m02_boot_id'] or row['run_id'] != c['run_id']:
            raise ValueError('clock sample is foreign')
        residence = row['exit_mono'] - row['entry_mono']
        d = row['t4'] - row['t1'] - residence
        if not (row['t1'] < row['t4'] and row['entry_epoch'] <= row['exit_epoch'] and residence >= 0 and d > 0):
            raise ValueError('clock sample is unordered')
        if abs(row['exit_epoch'] - row['entry_epoch'] - residence) > CLOCK_ANOMALY_S:
            raise ValueError('M02 epoch and mono disagree during a sample')
        bounds += [row['entry_epoch'] - row['t1'] - d, row['entry_epoch'] - row['t1']]
    if not s1['t4'] < s2['t1']:
        raise ValueError('clock samples do not bracket the body')
    if abs(s2['entry_epoch'] - s1['entry_epoch'] - (s2['entry_mono'] - s1['entry_mono'])) > CLOCK_ANOMALY_S:
        raise ValueError('M02 epoch and mono disagree between samples')
    lo, hi = min(bounds), max(bounds)
    if hi - lo > CLOCK_CEILING_S:
        raise ValueError('clock bracket over the ceiling')
    return lo, hi


def log_instants(stamp, begin, end):
    """A year-less glog stamp: the distinct instants, under the samples' years, inside the samples' span."""
    out = set()
    for moment in (begin, end):
        year = datetime.datetime.fromtimestamp(moment, datetime.timezone.utc).year
        t = datetime.datetime.strptime('%d%s' % (year, stamp), '%Y%m%d %H:%M:%S.%f').replace(tzinfo=datetime.timezone.utc).timestamp()
        if begin <= t <= end:
            out.add(t)
    return sorted(out)


def rotation_boundaries(events, begin, end):
    """Per VID, M02 epoch S of its one rotation success line (master_cache_volume_rotation.go:279, logged after the
    delete): the witnessed, VID-bound boundary of decision 3's revision, a conservative observed post-rotation point,
    not causal proof. A VID with any ambiguous, out-of-order or reused history, no birth first or not exactly one
    success last is left out, so its misses refuse. Unwritable lines are read only for that history: rotation of an
    already-full volume logs none, and normal ACKs follow the capacity line."""
    by_line, refused = {}, set()
    for e in events:
        typed(e, kind=str, vid=int, stamp=str, gen=int, line=str, sha256=str)
        seen = by_line.setdefault(e['sha256'], e)  # identity is the full raw line; `line` is presentation only
        if (seen['kind'], seen['vid'], seen['stamp'], seen['gen']) != (e['kind'], e['vid'], e['stamp'], e['gen']):
            refused |= {seen['vid'], e['vid']}  # never trust the first of a conflicting pair
    per = {}
    for e in by_line.values():
        instants = log_instants(e['stamp'], begin, end)
        if len(instants) != 1 or e['kind'] not in ('birth', 'unwritable', 'start', 'success'):
            refused.add(e['vid'])
            continue
        per.setdefault(e['vid'], []).append((instants[0], e['gen'], e['kind']))
    starts = {}
    for vid, rows in per.items():
        rows.sort()
        kinds = [k for _, _, k in rows]
        if vid in refused or kinds.count('success') != 1 or kinds[-1] != 'success':
            continue  # an event after the success, including a birth (VID reuse), lands here too
        at = [t for t, _, _ in rows]
        gens = [g for _, g, _ in rows]
        if len(set(at)) != len(at) or gens != sorted(gens):
            continue
        if kinds.count('birth') != 1 or kinds[0] != 'birth':
            continue
        starts[vid] = rows[-1][0]
    return starts


def start_boundaries(events, begin, end, bounds):
    """Per VID with a success boundary S, M02 epoch B of its FIRST retiring line ("retiring before seal|unpublish:
    cache Volume N": mono #972 logs it once per attempt, immediately before that attempt's first potentially
    reader-visible step, i.e. before the seal on a fresh rotation and before unpublish on a resumed one) strictly
    between the VID's birth and S. The line marks placement before the first potential effect, not proof it happened.
    The seal's read fence is monotonic (a durable retirement intent is never cleared automatically), so the first
    attempt's line bounds the earliest instant readers may lose the Volume, and a resumed rotation's later line can
    only be later. An attempt whose seal or unpublish failed and left the Volume serving is refused only when C9
    OBSERVES it (a later same-VID hit); without a later read such a miss is excused, inside the accepted
    last-exact-read..S blind spot (decision 3's stated limit), never claimed as proof the fence held. Only #972's
    text yields a start event (lab_profile.RETIRING); #969's form keeps B = S. Any retiring line outside (birth, S), or none,
    keeps B = S, the stricter success boundary. History refusals are rotation_boundaries': this only refines a VID
    that already passed them."""
    out = {}
    for vid, s in bounds.items():
        rows = {e['sha256']: e for e in events if e['vid'] == vid}.values()
        births = [t for e in rows if e['kind'] == 'birth' for t in log_instants(e['stamp'], begin, end)]
        starts = sorted(t for e in rows if e['kind'] == 'start' for t in log_instants(e['stamp'], begin, end))
        inside = [t for t in starts if births and births[0] < t < s]
        out[vid] = inside[0] if inside and len(inside) == len(starts) else s
    return out


def miss_excused(e, version, ack, good, bounds, lo, hi, dat, hits, starts=None, vid_last_hit=None):
    """Decision 3 revision (e45019f9 + codex01 8c56cca4), per miss: qualified observed ordering under the stated limit,
    in M01 mono; not a retirement or durability proof and not parity. The stated limit, accepted by Ping: a real loss
    between the version's last exact read and S is excused. `hits` are exact GETs of this same version."""
    for vid in {int(fid.split(',')[0]) for fid in version.fids}:
        if vid not in bounds or vid in dat:  # C2 (ledger), C8
            return False
        early, late = bounds[vid] - hi, bounds[vid] - lo
        # C3 against S; C4 against B (the retiring line, before unpublish), defaulting to S. Submission, so a GET
        # straddling B still refuses.
        late_b = (starts or {}).get(vid, bounds[vid]) - lo
        if not ack['completed'] < early or not e['submitted'] > late_b:
            return False
        if (vid_last_hit or {}).get(vid, -math.inf) > e['completed']:  # C9: the VID served a read after this miss
            return False
        if not any(h['submitted'] > ack['completed'] and h['completed'] < early for h in hits):  # C5
            return False
        after = [p for p in good if p['req'] > late]  # successful polls only; a failed reply is no evidence
        if any(vid in p['writables'] or vid in p['present'] for p in after):  # C6
            return False
        gone = next((p for p in after if vid not in p['present']), None)
        if not gone or gone['done'] > e['completed'] + 25:  # C7: requested after late(S)
            return False
    return True


def stats_name(identity):
    return 'native-stats-%d-%d.jsonl' % (typed(identity, pid=int)['pid'], typed(identity, starttime=int)['starttime'])


def assess_routes(root, workers, kills, refused_only=frozenset()):
    """Exactly one native-stats series per worker generation (4 originals + each replacement), none foreign,
    and each one's last observed loader counters in the accepted DC or HTTP identity. kv_load only: a DC
    process of a worker in `refused_only` (no hit in any of its generations) may show no route when its
    every read was a K8 refusal miss."""
    expected = {stats_name(w): w['transport'] for w in workers.values()}
    expected.update({stats_name(k['replacement']): workers[str(k['worker'])]['transport'] for k in kills})
    owner = {stats_name(w): i for i, w in workers.items()}
    owner.update({stats_name(k['replacement']): str(k['worker']) for k in kills})
    present = {p.name for p in root.iterdir() if p.name.startswith('native-stats-')}
    bad = sorted(present ^ set(expected))
    for name in sorted(present & set(expected)):
        observed = [typed(r, stats=dict)['stats'] for r in lines(root, name) if typed(r, status=str)['status'] == 'OBSERVED']
        if not observed:
            bad.append(name)
            continue
        last = typed(typed(observed[-1], loader=dict)['loader'], transport=str, **{k: int for k in COUNTERS})
        dc, rc, copied = (last[k] for k in COUNTERS)
        if expected[name] == 'dc':
            # A process whose every read was a K8 refusal miss has no route to show: zero DC reads and
            # read_calls equal to its refusal-miss counter. Any process with a read that was not refused
            # still needs its DC route.
            refused, calls = k8_refusal_misses(observed[-1]), last.get('read_calls')
            all_refused = owner[name] in refused_only and dc == 0 and refused is not None and not isinstance(calls, bool)                 and isinstance(calls, int) and calls > 0 and calls == refused
            ok = last['transport'] == 'rdma' and last.get('http_fallback_allowed') is False and (dc > 0 or all_refused)                 and rc == 0 and copied == 0
        else:
            ok = last['transport'] == 'http' and dc == 0 and rc == 0 and copied > 0
        if not ok or min(dc, rc, copied) < 0:
            bad.append(name)
    return bad


def kv_qualified(declared, faults, begin):
    """The plan's faults joined exactly to the evidence: one record per declared episode and nothing else,
    each a successful restart (REPLACED, as the direct path's Restart ends, or the approved runner's
    REPLACEMENT_STARTED; no error) of a live M02 victim, SIGKILL applied, a new
    M02 replacement, dispatched within 15 s of the declared time, and dispatch <= PreKillMono <= READY
    within 60 s. Anything short of that for any fault: none qualifies, so no window opens."""
    num = lambda v: isinstance(v, (int, float)) and not isinstance(v, bool) and math.isfinite(v)
    def ok(f, d):
        r = f['receipt']
        kill, old, new = r.get('Kill') or {}, (r.get('Kill') or {}).get('Identity') or {}, r.get('Replacement') or {}
        return f['role'] == d['role'] and r.get('State') in ('REPLACED', 'REPLACEMENT_STARTED') and not r.get('Error') \
            and old.get('Node') == 'm02' and kill.get('AliveBefore') is True and kill.get('Signal') == 'SIGKILL' \
            and isinstance(kill.get('AppliedAt'), str) and not kill['AppliedAt'].startswith('0001') and new.get('Node') == 'm02' \
            and num(new.get('PID')) and new['PID'] > 0 and new['PID'] != old.get('PID') and num(kill.get('PreKillMono')) \
            and abs(f['dispatch_mono'] - begin - d['at_s']) <= 15 \
            and f['dispatch_mono'] <= kill['PreKillMono'] <= f['ready_mono'] <= f['dispatch_mono'] + 60
    by = {}
    for f in faults:
        by.setdefault(f['episode'], []).append(f)
    good = [by[d['episode']][0] for d in declared if len(by.get(d['episode'], ())) == 1 and ok(by[d['episode']][0], d)]
    return good if len(good) == len(declared) == len(faults) else []


def kv_load_rows(pat, declared_faults, acks, ops, faults):
    """kv_load's own rows. write_once: every key's first version ACKed exactly once at the declared size,
    and nothing read (dispatched or submitted) before the last of those ACKs. load_as_declared, in the
    timed phases after that barrier: each client's operations within 10% of rate x the unpaused time;
    each phase's write share as declared (none in a read phase, 1 - read_ratio within 0.05 in a mixed
    one); the top 1% of keys drawing their declared share of the reads within 0.05; and exactly the
    plan's faults, each READY after its dispatch."""
    first = [a for a in acks if a['seed'].startswith('kv-load-') and a['seed'].endswith('-0')]
    index = {a['key']: int(a['seed'].split('-')[2]) for a in first}
    barrier = max((a['completed'] for a in first), default=None)
    early = [e['op'] for e in ops if e['action'] == 'get' and (barrier is None or min(e['dispatched'], e['submitted']) < barrier)]
    once = len(first) == pat['keys'] and sorted(index.values()) == list(range(pat['keys'])) and \
        all(a['length'] == pat['object_bytes'] for a in acks)
    write_once = (once and not early, '%d first versions for %d keys, %d GETs before the barrier' % (len(first), pat['keys'], len(early)))
    span = sum(p['duration_s'] for p in pat['phases'][1:])
    start = barrier or 0.0
    pause = lambda a, b: sum(max(0.0, min(f['ready_mono'], b) - max(f['dispatch_mono'], a)) for f in faults)
    paused = pause(start, start + span)
    timed = [e for e in ops if start <= e['dispatched'] < start + span]
    reads = [index[e['key']] for e in timed if e['action'] == 'get' and e['key'] in index]
    top = max(1, pat['keys'] // 100)
    w = [1.0 if pat['distribution'] == 'uniform' else (k + 1) ** -pat['zipf_s'] for k in range(pat['keys'])]
    share, expected = sum(i < top for i in reads) / max(len(reads), 1), sum(w[:top]) / sum(w)
    per = [sum(e['worker'] == i for e in timed) for i in range(pat['clients'])]
    mix, rates, t0, r = [], True, start, pat['rate_per_client']
    for p in pat['phases'][1:]:  # every client at its rate in every phase, not balanced across them
        d, in_p = p['duration_s'], [e for e in timed if t0 <= e['dispatched'] < t0 + p['duration_s']]
        rates = rates and all(0.9 * r * (d - pause(t0, t0 + d)) <= sum(e['worker'] == i for e in in_p) <= 1.1 * r * d
                              for i in range(pat['clients']))
        writes = sum(e['action'] == 'set' for e in in_p) / max(len(in_p), 1)
        mix.append(writes == 0 if p['kind'] == 'read' else abs(writes - (1 - p['read_ratio'])) <= 0.05)
        t0 += p['duration_s']
    named = len(faults) == len(declared_faults)  # the qualified faults: one per declared, none unqualified
    ok = rates and all(mix) and abs(share - expected) <= 0.05 and named
    return write_once, (ok, 'ops per client %s in %.0f s (%.0f paused), want %.1f/s; phase mixes %s; top-1%% share %.3f, '
                        'declared %.3f; faults %s as declared %s' % (per, span, paused, pat['rate_per_client'], mix, share,
                                                                    expected, [f['episode'] for f in faults], named))


def assess(c):
    root, crit = Path(c['root']), {}
    def mark(name, ok, reason=''):
        crit[name] = dict(outcome='PASS' if ok else 'FAIL', reason=reason)
    run = dict(run_id=c['run_id'], boot_id=c['boot_id'])
    if boot_id() != c['boot_id']:
        raise ValueError('assessor is not on the admitted M01 boot')
    begin = typed(read_bound(root / WITNESS, dict(boot_id=c['boot_id'])) or {}, mono=float)['mono']
    acks = [typed(a, key=str, writer=int, seed=str, submitted=float, completed=float, sequence=int, length=int,
                  digest=str, fids=list) for a in lines(root, 'ack-journal.jsonl')]
    events = [typed(e) for e in lines(root, 'op-events.jsonl', op_event_bounds(c.get('pattern')))]
    ops = [typed(e, op=str, action=str, key=str, worker=int, dispatched=float, submitted=float, completed=float, ok=bool)
           for e in events if 'action' in e]
    marks = [typed(e, event=str) for e in events if 'action' not in e]
    if any(e['action'] not in ('set', 'get') for e in ops) or any(m['event'] not in ('client_kill', 'body_end', 'quiescent') for m in marks):
        raise ValueError('unknown op-event kind')
    polls = [typed(p, req=float, done=float, ok=bool) for p in lines(root, 'topology-polls.jsonl')]
    good = [typed(p, writables=list, present=list) for p in polls if p['ok']]
    faults = [typed(f, episode=str, role=str, dispatch_mono=float, ready_mono=float, receipt=dict) for f in obj(root, 'faults.json')]
    m02 = typed(obj(root, 'm02-collect.json'), status=str, collection=str, rotated=list, dat_vids=list)
    if m02['status'] != 'SOAK_COLLECTED' or m02['collection'] != c['collection']:
        raise ValueError('M02 collect missing or foreign')
    workers, pat = obj(root, 'workers.json'), c.get('pattern')  # kv_load: its own clients, all DC
    ids = [str(i) for i in range(pat['clients'] if pat else 4)]
    if not isinstance(workers, dict) or sorted(workers) != sorted(ids) or [typed(workers[i], pid=int, starttime=int, transport=str)
                                                                          ['transport'] for i in ids] != (['dc'] * len(ids) if pat else ['dc', 'dc', 'dc', 'http']):
        raise ValueError('workers.json is not the accepted client set')

    # Oracle replay: every SET re-derived from its seed, every GET outcome exactly one recognised case.
    oracle, by_seq, wrong, misses, unknown, acked, hits, latest_raw = Oracle(), {}, [], [], [], {}, {}, {}
    gets = [e for e in ops if e['action'] == 'get']
    for kind, _, r in sorted([('ack', a['completed'], a) for a in acks] + [('get', e['completed'], e) for e in gets], key=lambda x: x[1]):
        if kind == 'ack':
            v = oracle.begin_write(r['key'], payload(r['seed'], r['length']))
            v = oracle.acknowledge(r['key'], v.sequence, r['length'], r['fids'])
            if v.digest != r['digest']:
                wrong.append(('ack', r['key']))
            by_seq[(r['key'], r['sequence'])] = (v, r)
            latest_raw[r['key']] = r['sequence']  # the worker's own sequence; the replay's v.sequence is an ACK-order ordinal
            acked.setdefault(r['key'], []).append((r['completed'], v))
            continue
        case, latest = get_case(r), oracle.latest.get(r['key'])
        if case == 'hit' and (latest is None or not latest.matches(r['length'], r['sha256'])):
            wrong.append(('get', r['op']))
        elif case == 'hit' and r['sequence'] == latest_raw.get(r['key']):
            # C5's read-back evidence: exact bytes of the version the GET itself recorded, by the worker's raw
            # sequence, resolved through the ACK journal, and that version is the latest. A hit recorded against
            # another, an unacknowledged or an unknown version, even with the same bytes, credits nothing.
            hits.setdefault((r['key'], r['sequence']), []).append(r)
        elif case == 'miss':
            # J1: the latest ACK completed before THIS miss was submitted, not one landing while it was in flight.
            misses.append((r, next((v for done, v in reversed(acked.get(r['key'], [])) if done < r['submitted']), None)))
        elif case == 'unknown':
            unknown.append(r['op'])
    mark('wrong_reads', not wrong, '%d wrong' % len(wrong))
    mark('get_outcomes', not unknown, '%d GETs, unrecognised %s' % (len(gets), unknown[:10]))
    sets = [e for e in ops if e['action'] == 'set']
    failed = [e['op'] for e in sets if not e['ok']]
    journal = sorted((a['key'], a['writer']) for a in acks)  # one ACK row per successful SET event
    mark('failed_sets', not failed and journal == sorted((e['key'], e['worker']) for e in sets if e['ok']),
         '%d failed, %d acks for %d SETs' % (len(failed), len(acks), len(sets)))

    # Terminal: the quiescent row last, after duration + quiescence, and every dispatched op settled exactly once.
    kills = [typed(k, worker=int, started=float, killed=float, finished=float, witness=dict, replacement=dict)
             for k in marks if k['event'] == 'client_kill']
    quiet = [typed(q, mono=float, dispatched=int) for q in marks if q['event'] == 'quiescent']
    settled = sorted([e['op'] for e in ops] + [str(k['witness'].get('operation_id')) for k in kills])
    ok = len(quiet) == 1 and events[-1] is quiet[0] and quiet[0]['mono'] >= begin + c['duration_s'] + c['quiescence_s'] \
        and settled == sorted(str(i) for i in range(1, quiet[0]['dispatched'] + 1)) and all(e['completed'] <= quiet[0]['mono'] for e in ops)
    mark('terminal', ok, '%d quiescent rows, %d ops settled' % (len(quiet), len(settled)))

    # Fault errors: an acked pending GET (exact op and dispatch), bound ack, submitted before P, completed in [P, P+60].
    P = {f['episode']: f for f in faults}
    def bound_ack(f):
        try:  # a foreign run, boot, episode or role credits nothing
            return read_bound(root / ('fault-ack-%s.json' % f['episode']), dict(run, episode=f['episode'], role=f['role']))
        except ValueError:
            return None
    bad = []
    for e in (e for e in gets if get_case(e) == 'fault'):
        f = P.get(e['episode'])
        ack = f and bound_ack(f)
        pending = ack and {typed(g, op=str, submitted=float)['op']: g['submitted'] for g in typed(ack, observed_mono=float, pending_gets=list)['pending_gets']}
        p = f and f['dispatch_mono']
        ok = (f and pending and typed(f['receipt'].get('Kill') or {}, PreKillMono=float)['PreKillMono'] > 0
              and pending.get(e['op']) == e['dispatched'] and e['dispatched'] <= e['submitted'] <= p
              and ack['observed_mono'] <= p and p <= e['completed'] <= p + 60 and TRANSIENT.search(e.get('error') or ''))
        if not ok:
            bad.append(e['op'])
    mark('fault_errors', not bad, 'uncredited %s' % bad[:10])

    # Misses: the accepted per-miss join (decision 3 revision), never a global retirement set.
    rotated = {typed(r, vid=int)['vid'] for r in m02['rotated']}
    dat = set(m02['dat_vids'])
    try:
        s1, s2 = obj(root, 'soak-clock-s1.json'), obj(root, 'soak-clock-s2.json')
        lo, hi = clock_bracket(c, s1, s2)
        bounds = rotation_boundaries(typed(m02, events=list)['events'], s1['entry_epoch'], s2['exit_epoch'])
        bounds = {v: t for v, t in bounds.items() if v in rotated}
        starts = start_boundaries(m02['events'], s1['entry_epoch'], s2['exit_epoch'], bounds)
        clock_error = ''
    except (ValueError, OSError) as error:  # no usable clock evidence: every miss stays unexcused
        lo, hi, bounds, starts, clock_error = 0, 0, {}, {}, '; clock: %s' % str(error)[:120]
    vid_last_hit = {}  # per VID, the latest completion of any exact hit on a version it holds (C9)
    for g in gets:
        v = get_case(g) == 'hit' and by_seq.get((g['key'], g['sequence']), (None, None))[0]
        for vid in {int(f.split(',')[0]) for f in (v.fids if v else ())}:
            vid_last_hit[vid] = max(vid_last_hit.get(vid, -math.inf), g['completed'])
    def excused(e, latest):
        v, a = by_seq.get((e['key'], e['sequence']), (None, None))
        return bool(v) and latest == v and list(e['fids']) == list(v.fids) and \
            miss_excused(e, v, a, good, bounds, lo, hi, dat, hits.get((e['key'], e['sequence']), []), starts, vid_last_hit)
    unexcused = [e for e, latest in misses if not excused(e, latest)]
    if pat:  # kv_load: every miss outside the qualified fault windows fails (no soak retirement excusal); inside, reported
        qualified = kv_qualified(c['faults'], faults, begin)
        inside = {e['op'] for e, _ in misses if any(f['dispatch_mono'] <= e['submitted'] <= f['ready_mono'] + 60 for f in qualified)}
        unexcused = [e for e, _ in misses if e['op'] not in inside]
        clock_error += '; %d inside fault windows, reported' % len(inside)
    # A K8 budget-refusal miss is a cache outcome by design, excused only when its own event names the
    # cause (the worker's counter delta across that GET); nothing else is inferred.
    if pat:  # kv_load only: the ordinary soak keeps its semantics
        refused = [e for e, _ in misses if e.get('cause') == 'budget_refusal']
        unexcused = [e for e in unexcused if e.get('cause') != 'budget_refusal']
        clock_error += '; %d budget-refusal misses excused' % len(refused)
    mark('misses', not unexcused, '%d misses, unexcused %s%s' % (len(misses), [e['op'] for e in unexcused][:10], clock_error))

    # Coverage, body, client kill, rotation, master fault effect.
    writers, keys = {}, {}
    for a in acks:
        writers[a['writer']] = writers.get(a['writer'], 0) + 1
        keys.setdefault(a['key'], set()).add(a['writer'])
    if pat:
        (ok, why), declared = kv_load_rows(pat, c['faults'], acks, ops, qualified)
        mark('write_once', ok, why)
    else:
        mark('coverage', sorted(writers) == [0, 1, 2, 3] and any(len(w) == 4 for w in keys.values()), 'writers %s' % sorted(writers))
    ends = [typed(e, mono=float)['mono'] for e in marks if e['event'] == 'body_end']
    late = [e['op'] for e in ops if ends and e['dispatched'] > ends[0]]
    mark('body', len(ends) == 1 and abs(ends[0] - begin - c['duration_s']) <= 1 and not late, 'ends %s, late %d' % (ends, len(late)))
    k = kills[0] if len(kills) == 1 else None
    w = k and k['witness']
    original = k and workers.get(str(k['worker']))
    ok = bool(k) and (original or {}).get('transport') == 'dc' and k['started'] - begin >= c['client_kill_at_s'] and 0 <= k['killed'] - k['started'] <= 60 \
        and 0 <= k['finished'] - k['killed'] <= 30 and w.get('event') == 'submitted' and w.get('action') == 'get' \
        and w.get('pid') == original['pid'] and k['replacement'].get('pid') != original['pid']
    mark('client_kill', ok, '%d kills' % len(kills))
    if pat:
        mark('load_as_declared', *declared)
    else:
        final = set(good[-1]['present']) if good else None
        proven = sorted(v for v in rotated if final is not None and v not in final and v not in dat)
        mark('rotation', len(proven) >= 10, '%d proven' % len(proven))
        mf = next((f for f in faults if f['role'] == 'master'), None)
        before = max((max(p['present'], default=0) for p in good if mf and p['req'] < mf['dispatch_mono']), default=0)
        after = max((max(p['present'], default=0) for p in good if mf and p['req'] > mf['ready_mono']), default=0)
        mark('max_vid_advanced', mf is not None and after > before, 'before %d after %d' % (before, after))
    # kv_load: only a worker whose EVERY GET, in all its generations, is a recorded budget-refusal miss may
    # show no route; a hit, a plain miss or a fault error (excused in a window or not) keeps the requirement.
    refused = {}
    for e in gets:
        refused.setdefault(str(e['worker']), []).append(get_case(e) == 'miss' and e.get('cause') == 'budget_refusal')
    refused_only = frozenset(w for w, all_refused in refused.items() if all(all_refused)) if pat else frozenset()
    route_bad = assess_routes(root, workers, kills, refused_only)
    mark('routes', not route_bad, 'bad %s' % route_bad)

    # K7 (a FINDING, never a FAIL): the first non-HTTP SET after each fault's successful READY.
    k7 = []
    for f in faults:
        first = min((a for a in acks if a['submitted'] >= f['ready_mono'] and workers[str(a['writer'])]['transport'] != 'http'), key=lambda a: a['submitted'], default=None)
        if first:
            lat = first['completed'] - first['submitted']
            k7.append(dict(episode=f['episode'], role=f['role'], dt_after_ready_s=first['submitted'] - f['ready_mono'],
                           latency_s=lat, at_rc_timeout=lat >= 4.9))
    status = 'PASS' if all(v['outcome'] == 'PASS' for v in crit.values()) else 'FAIL'
    return dict(status=status, criteria=crit, k7=k7, resources='NOT_OBSERVED', misses_qualification=QUALIFICATION)


# Emitted with every verdict, PASS or FAIL: what an excused miss does and doesn't mean.
QUALIFICATION = dict(
    method='qualified observed ordering under the stated limit (decision 3 revision); not a retirement or durability '
           'proof; no parity claim',
    clock='conditional on the paired clock samples S1/S2: ordering is judged across their bracket, assuming a '
          'constant offset between the samples; an out-and-back offset excursion between them is not excluded; '
          'without usable samples no miss is excused',
    limit='accepted by Ping: a real loss between a version\'s last exact read and its volume\'s rotation success line '
          'is excused')


def binding(c):
    return dict(schema=SCHEMA, run_id=c.get('run_id'), boot_id=c.get('boot_id'), root=c.get('root'))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', required=True)
    config = json.loads(parser.parse_args().config)
    try:
        if config.get('schema') != SCHEMA:
            raise ValueError('assessor config schema differs')
        row = assess(config)
    except Exception as error:  # any unreadable or malformed evidence fails closed
        row = dict(status='FAIL', error=repr(error)[:500], resources='NOT_OBSERVED', misses_qualification=QUALIFICATION)
    row['binding'] = binding(config)
    print(json.dumps(row, sort_keys=True), flush=True)


if __name__ == '__main__':
    main()
