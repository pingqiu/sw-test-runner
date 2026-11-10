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
from soak_oracle import OP_LIMIT_S, Oracle, PutFaultWindow
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


def op_event_bounds(pat, duration_s=1800):
    """(rows, bytes) op-events.jsonl may hold. The soak: its fixed caps per 30 minutes of load, times
    ceil(duration_s / 1800) and never below them (#1002: a 30-min soak wrote 56k rows, 22 MB, so 2 h and
    8 h outgrow the 30-min caps). kv_load: what its declared pattern can write (each key's write and its
    verification, every operation of every timed phase at the declared rate, a margin per client and for
    the marks), at OP_ROW bytes a row, never below the soak's caps. A full kv_load writes more than 64 MiB."""
    if not pat:
        scale = max(1, -(-duration_s // 1800))
        return ROWS * scale, FILE * scale
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


def vid_outcome(e, vid, ack, good, bounds, lo, hi, dat, hits, starts=None, vid_last_hit=None):
    """One chunk VID as this miss's retirement witness, every guard on that VID alone (never mixed with a sibling's):
    'excused', 'undecidable' or 'unexcused'. Decision 3 revision (e45019f9 + codex01 8c56cca4): qualified observed
    ordering under the stated limit, in M01 mono; not a retirement or durability proof and not parity. The stated
    limit, accepted by Ping: a real loss between the version's last exact read and S is excused. `hits` are exact
    GETs of this same version. Rule A (Ping, 2026-11-04): a GET submitted inside the clock bracket around B, i.e. in
    (early(B), late(B)], cannot be ordered against B either way; it is 'undecidable', which does not fail the row,
    only when every other check holds, C9 included, AND the VID has its #972 retiring line (B < S). A GET provably
    before early(B), or inside the bracket without a retiring line, is unexcused."""
    if vid not in bounds or vid in dat:  # C2 (ledger), C8
        return 'unexcused'
    early, late = bounds[vid] - hi, bounds[vid] - lo
    b = (starts or {}).get(vid, bounds[vid])  # B: the first #972 retiring line, defaulting to S
    if not ack['completed'] < early:  # C3 against S
        return 'unexcused'
    if (vid_last_hit or {}).get(vid, -math.inf) > e['completed']:  # C9: the VID served a read after this miss
        return 'unexcused'  # (any later exact hit of this version marks this VID too, so it also refuses here)
    if not any(h['submitted'] > ack['completed'] and h['completed'] < early for h in hits):  # C5
        return 'unexcused'
    after = [p for p in good if p['req'] > late]  # successful polls only; a failed reply is no evidence
    if any(vid in p['writables'] or vid in p['present'] for p in after):  # C6
        return 'unexcused'
    gone = next((p for p in after if vid not in p['present']), None)
    if not gone or gone['done'] > e['completed'] + 25:  # C7: requested after late(S)
        return 'unexcused'
    if e['submitted'] > b - lo:  # C4: submitted after late(B); submission, so a straddling GET does not count
        return 'excused'
    if e['submitted'] > b - hi and b < bounds[vid]:  # rule A: inside the bracket around a real retiring line
        return 'undecidable'
    return 'unexcused'  # provably before early(B), or inside the bracket without a retiring line


def miss_judgement(e, version, ack, good, bounds, lo, hi, dat, hits, starts=None, vid_last_hit=None):
    """Per miss, existential over its version's chunk VIDs (Ping 2026-11-06, runner #119): a whole-value miss is
    explained when ANY one chunk's VID is a fully qualified retirement witness on its own ('excused'); else, if any is
    'undecidable' under rule A, the miss is undecidable; else it is unexcused. Cache-availability semantics, not
    product-loss clearance: the full-value read does not identify the missing chunk, so a concurrent loss of another
    chunk is invisible (accepted by Ping with the existing latent-loss limit). A single-VID version is judged exactly
    as before. Returns (outcome, witnesses): the VIDs that carry the outcome, in VID order; none when unexcused."""
    per = {vid: vid_outcome(e, vid, ack, good, bounds, lo, hi, dat, hits, starts, vid_last_hit)
           for vid in sorted({int(fid.split(',')[0]) for fid in version.fids})}
    for state in ('excused', 'undecidable'):
        witnesses = [vid for vid, o in per.items() if o == state]
        if witnesses:
            return state, witnesses
    return 'unexcused', []  # also a version with no FID at all: nothing can witness it


def miss_outcome(e, version, ack, good, bounds, lo, hi, dat, hits, starts=None, vid_last_hit=None):
    """'excused', 'undecidable' or 'unexcused': miss_judgement's outcome."""
    return miss_judgement(e, version, ack, good, bounds, lo, hi, dat, hits, starts, vid_last_hit)[0]


def miss_excused(e, version, ack, good, bounds, lo, hi, dat, hits, starts=None, vid_last_hit=None):
    """True only for an ordered, excused miss (miss_outcome 'excused'); an undecidable one is not excused."""
    return miss_outcome(e, version, ack, good, bounds, lo, hi, dat, hits, starts, vid_last_hit) == 'excused'


def stats_name(identity):
    return 'native-stats-%d-%d.jsonl' % (typed(identity, pid=int)['pid'], typed(identity, starttime=int)['starttime'])


def assess_routes(root, workers, kills, refused_only=frozenset()):
    """Exactly one native-stats series per worker generation (4 originals + each replacement), none foreign,
    and each one's last observed loader counters in the accepted DC, RC or HTTP identity. kv_load only: a DC
    process (series name) in `refused_only` (every GET of its own generation a recorded refusal miss) may
    show no route when its every read was a K8 refusal miss."""
    expected = {stats_name(w): w['transport'] for w in workers.values()}
    expected.update({stats_name(k['replacement']): workers[str(k['worker'])]['transport'] for k in kills})
    present = {p.name for p in root.iterdir() if p.name.startswith('native-stats-')}
    bad = sorted(present ^ set(expected))
    for name in sorted(present & set(expected)):
        observed = [typed(r, stats=dict)['stats'] for r in lines(root, name) if typed(r, status=str)['status'] == 'OBSERVED']
        if not observed:
            bad.append(name)
            continue
        last = typed(typed(observed[-1], loader=dict)['loader'], transport=str, **{k: int for k in COUNTERS})
        dc, rc, copied = (last[k] for k in COUNTERS)
        if expected[name] in ('dc', 'rc'):
            # An RDMA process shows its own route (DC, or RC for #999) and no other. A process whose every
            # read was a K8 refusal miss has no route to show: zero reads on its route and read_calls equal
            # to its refusal-miss counter. Any process with a read that was not refused still needs its route.
            own, other = (dc, rc) if expected[name] == 'dc' else (rc, dc)
            refused, calls = k8_refusal_misses(observed[-1]), last.get('read_calls')
            all_refused = name in refused_only and own == 0 and refused is not None and not isinstance(calls, bool)                 and isinstance(calls, int) and calls > 0 and calls == refused
            ok = last['transport'] == 'rdma' and last.get('http_fallback_allowed') is False and (own > 0 or all_refused)                 and other == 0 and copied == 0
        else:
            ok = last['transport'] == 'http' and dc == 0 and rc == 0 and copied > 0
        if not ok or min(dc, rc, copied) < 0:
            bad.append(name)
    return bad


def refusal_only_processes(workers, kills, gets):
    """kv_load: the native-stats series whose OWN generation's GETs are all recorded budget-refusal misses
    (the route sample is per process, so is its exemption). A GET is attributed to a worker's generation by
    that worker's kill rows: before `killed` the earlier generation, from `finished` the replacement. A GET
    dispatched inside a kill (killed <= t < finished), or of an unknown worker, belongs to no one generation
    and denies the exemption to every generation of its worker (fail closed). A hit, a plain miss or a fault
    error keeps its generation's route requirement."""
    names = {i: [stats_name(w)] for i, w in workers.items()}
    splits = {i: [] for i in workers}
    for k in sorted(kills, key=lambda k: k['killed']):
        names[str(k['worker'])].append(stats_name(k['replacement']))
        splits[str(k['worker'])].append((k['killed'], k['finished']))
    outcomes, denied = {}, set()
    for e in gets:
        w, t = str(e['worker']), e['dispatched']
        if w not in names or any(killed <= t < finished for killed, finished in splits[w]):
            denied.add(w)
            continue
        generation = sum(1 for _, finished in splits[w] if t >= finished)
        outcomes.setdefault(names[w][generation], []).append(get_case(e) == 'miss' and e.get('cause') == 'budget_refusal')
    return frozenset(n for w, ns in names.items() if w not in denied for n in ns if outcomes.get(n) and all(outcomes[n]))


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
    route = c.get('read_transport', 'dc')  # the soak's RDMA route for clients 0-2 (#999); absent is DC
    if route not in ('dc', 'rc') or (route == 'rc' and c.get('pattern')):
        raise ValueError('read_transport must be dc or rc, and rc is the soak only')
    def mark(name, ok, reason=''):
        crit[name] = dict(outcome='PASS' if ok else 'FAIL', reason=reason)
    run = dict(run_id=c['run_id'], boot_id=c['boot_id'])
    if boot_id() != c['boot_id']:
        raise ValueError('assessor is not on the admitted M01 boot')
    begin = typed(read_bound(root / WITNESS, dict(boot_id=c['boot_id'])) or {}, mono=float)['mono']
    acks = [typed(a, key=str, writer=int, seed=str, submitted=float, completed=float, sequence=int, length=int,
                  digest=str, fids=list) for a in lines(root, 'ack-journal.jsonl')]
    events = [typed(e) for e in lines(root, 'op-events.jsonl', op_event_bounds(c.get('pattern'), c['duration_s']))]
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
                                                                          ['transport'] for i in ids] != (['dc'] * len(ids) if pat else [route] * 3 + ['http']):
        raise ValueError('workers.json is not the accepted client set')
    if any(str(e['worker']) not in workers for e in ops):  # every op belongs to one admitted client
        raise ValueError('op-event of a worker outside workers.json')

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
    def judgement(e, latest):
        v, a = by_seq.get((e['key'], e['sequence']), (None, None))
        if not (v and latest == v and list(e['fids']) == list(v.fids)):
            return 'unexcused', []
        return miss_judgement(e, v, a, good, bounds, lo, hi, dat, hits.get((e['key'], e['sequence']), []), starts, vid_last_hit)
    judged = [(e, *judgement(e, latest)) for e, latest in misses]
    unexcused = [e for e, o, _ in judged if o == 'unexcused']
    undecidable = []  # rule A: reported per op and per WITNESS VID, separately from excused and unexcused
    for e, o, witnesses in judged:
        if o == 'undecidable':
            for vid in witnesses:  # only the VIDs that carry rule A; an unrotated sibling has no B and is not reported
                b = starts.get(vid, bounds[vid])
                undecidable.append(dict(op=e['op'], vid=vid, submit_minus_early_b_s=round(e['submitted'] - (b - hi), 6),
                                        submit_minus_late_b_s=round(e['submitted'] - (b - lo), 6)))
    if pat:  # kv_load: every miss outside the qualified fault windows fails (no soak retirement excusal); inside, reported
        qualified = kv_qualified(c['faults'], faults, begin)
        inside = {e['op'] for e, _ in misses if any(f['dispatch_mono'] <= e['submitted'] <= f['ready_mono'] + 60 for f in qualified)}
        unexcused = [e for e, _ in misses if e['op'] not in inside]
        undecidable = []  # kv_load judges misses by fault windows, not by rotation ordering
        clock_error += '; %d inside fault windows, reported' % len(inside)
    # A K8 budget-refusal miss is a cache outcome by design, excused only when its own event names the
    # cause (the worker's counter delta across that GET); nothing else is inferred.
    if pat:  # kv_load only: the ordinary soak keeps its semantics
        refused = [e for e, _ in misses if e.get('cause') == 'budget_refusal']
        unexcused = [e for e in unexcused if e.get('cause') != 'budget_refusal']
        clock_error += '; %d budget-refusal misses excused' % len(refused)
    if undecidable:
        clock_error += '; %d undecidable inside the clock bracket around B (rule A) %s' % (
            len({u['op'] for u in undecidable}), sorted({u['op'] for u in undecidable}, key=int)[:10])
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
    ok = bool(k) and (original or {}).get('transport') == ('dc' if pat else route) and k['started'] - begin >= c['client_kill_at_s'] and 0 <= k['killed'] - k['started'] <= 60 \
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
    refused_only = refusal_only_processes(workers, kills, gets) if pat else frozenset()
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
    return dict(status=status, criteria=crit, k7=k7, resources='NOT_OBSERVED', misses_qualification=QUALIFICATION,
                misses_undecidable=undecidable)


# ---- Stage 3: one RDMA/resource fault row (R0, F1-F7). PASS needs every criterion PASS; a missing witness is
# NOT_COVERED and never PASS; any FAIL fails the row. Each criterion recomputes from the run's own evidence.

ROW_CRITERIA = ('bytes', 'write_contract', 'audit', 'failure_mode', 'recovery', 'resources', 'causal')  # Go's rowCriteria
# Carried on every row verdict, PASS or not (Go's rowLayoutScope): a row result never reads as covering more.
LAYOUT_SCOPE = 'single volume process, on M02: multi-volume layouts are not covered'
ROW_KINDS = {'control': 'R0', 'volume_kill_io': 'F1', 'client_kill_put': 'F2', 'link_flap': 'F3', 'conn_kill': 'F4',
             'mr_pressure': 'F5', 'disk_full': 'F6', 'fd_exhaust': 'F7'}
RECOVERY_S = {'volume_kill_io': 60, 'client_kill_put': 60}  # frozen by the plan; every other row 30 s
SET_EXTRA_S = 9  # a PUT's terminal includes the ACK metadata read (8 s urlopen bound) after its native completion
RESOURCES = ('fds', 'qp', 'mr', 'vm_pin_kB')
PAGE = 4096


def rank(outcomes):
    """FAIL over NOT_COVERED over PASS."""
    return 'FAIL' if 'FAIL' in outcomes else 'NOT_COVERED' if 'NOT_COVERED' in outcomes else 'PASS'


def series(samples, key, ident, lo=-math.inf, hi=math.inf):
    """(mono, value) of one metric over the samples of one volume identity, inside [lo, hi] (M02 clock)."""
    return [(s['mono'], s['metrics'][key]) for s in samples if s.get('identity') == ident and lo <= s['mono'] <= hi
            and isinstance(s.get('metrics'), dict) and key in s['metrics']]


def counter_rose(samples, key, ident, start, end, after=None):
    """True when the counter's value at some sample inside (start, end] exceeds its last value at or before start;
    None when the metric was not observed on both sides (NOT_COVERED)."""
    before = series(samples, key, ident, hi=start)
    inside = series(samples, key, ident, lo=after if after is not None else start, hi=end)
    inside = [(t, v) for t, v in inside if t > start]
    if not before or not inside:
        return None
    return max(v for _, v in inside) > before[-1][1]


def assess_row(c):
    root, row = Path(c['root']), c['fault_row']
    kind, episode = row['kind'], row['episode']
    if kind not in ROW_KINDS or row['recovery_s'] != RECOVERY_S.get(kind, 30):
        raise ValueError('fault row kind unknown or its frozen recovery bound moved')
    if boot_id() != c['boot_id']:
        raise ValueError('assessor is not on the admitted M01 boot')
    crit, claims = {}, {}

    def mark(name, outcome, reason=''):
        crit[name] = dict(outcome=outcome, reason=reason[:400])
    begin = typed(read_bound(root / WITNESS, dict(boot_id=c['boot_id'])) or {}, mono=float)['mono']
    acks = [typed(a, key=str, writer=int, seed=str, submitted=float, completed=float, sequence=int, length=int, digest=str, fids=list)
            for a in lines(root, 'ack-journal.jsonl')]
    events = [typed(e) for e in lines(root, 'op-events.jsonl', op_event_bounds(c.get('pattern'), c['duration_s']))]
    ops = [typed(e, op=str, action=str, key=str, worker=int, dispatched=float, submitted=float, completed=float, ok=bool)
           for e in events if 'action' in e]
    marks = [typed(e, event=str) for e in events if 'action' not in e]
    if any(m['event'] not in ('client_kill', 'body_end', 'quiescent', 'fault_window', 'audit_done') for m in marks):
        raise ValueError('unknown op-event kind')
    ledger = [typed(r, op=str, key=str, state=str, seed=str, length=int, digest=str, sequence=int, worker=int,
                    dispatched=float, submitted=float, completed=float, worker_identity=dict) for r in lines(root, 'write-ledger.jsonl')]
    audits = [typed(r, key=str, reads=list) for r in lines(root, 'audit.jsonl')]
    faults = [typed(f, episode=str, role=str, dispatch_mono=float, ready_mono=float, receipt=dict) for f in obj(root, 'faults.json')]
    samples = [typed(s, mono=float, epoch=float, phase=str) for s in lines(root, 'resource-samples.jsonl')]
    workers = obj(root, 'workers.json')
    polls = [typed(p, req=float, done=float, ok=bool) for p in lines(root, 'topology-polls.jsonl')]
    good = [typed(p, writables=list, present=list) for p in polls if p['ok']]
    m02 = typed(obj(root, 'm02-collect.json'), status=str, collection=str, rotated=list, dat_vids=list)
    if m02['status'] != 'SOAK_COLLECTED' or m02['collection'] != c['collection']:
        raise ValueError('M02 collect missing or foreign')
    if len(faults) != 1 or faults[0]['episode'] != episode:
        raise ValueError('faults.json is not exactly this row')
    f = faults[0]
    kills = [k for k in marks if k['event'] == 'client_kill']
    try:
        lo, hi = clock_bracket(c, obj(root, 'soak-clock-s1.json'), obj(root, 'soak-clock-s2.json'))
    except (ValueError, OSError):
        lo = hi = None  # no M01-M02 join: every witness that needs one is NOT_COVERED
    injection = {r.get('action'): r for r in f.get('injection') or [] if isinstance(r, dict)}

    # 1. Bytes: every ACK re-derived from its seed; every hit the latest ACK when it completed (oracle replay).
    wrong, oracle = [], Oracle()
    gets = [e for e in ops if e['action'] == 'get']
    for k, _, r in sorted([('ack', a['completed'], a) for a in acks] + [('get', e['completed'], e) for e in gets], key=lambda x: x[1]):
        if k == 'ack':
            v = oracle.begin_write(r['key'], payload(r['seed'], r['length']))
            v = oracle.acknowledge(r['key'], v.sequence, r['length'], r['fids'])
            if v.digest != r['digest']:
                wrong.append('ack ' + r['key'][-8:])
        elif get_case(r) == 'hit' and not (r['key'] in oracle.latest and oracle.latest[r['key']].matches(r['length'], r['sha256'])):
            wrong.append('get ' + r['op'])
    wrong += ['ledger ' + r['op'] for r in ledger if r['digest'] != hashlib_sha(payload(r['seed'], r['length']))]
    mark('bytes', 'FAIL' if wrong else 'PASS', '%d wrong %s' % (len(wrong), wrong[:5]))

    # 2. Write contract: one terminal state per PUT, ACKED exactly the journal, UNKNOWN never retried or ACKed,
    # and every UNKNOWN attributed through the named PUT window (F1, F2 only).
    bad, by_op = [], {}
    for r in ledger:
        if r['state'] not in ('ACKED', 'REFUSED', 'UNKNOWN') or r['op'] in by_op:
            bad.append('state ' + r['op'])
        by_op[r['op']] = r
    set_ops = {e['op'] for e in ops if e['action'] == 'set'}
    kill_ops = {str((k.get('witness') or {}).get('operation_id')) for k in kills if k.get('kind') == 'client_kill_put'}
    if set(by_op) != set_ops | kill_ops:
        bad.append('ledger is not exactly the PUTs')
    if sorted((r['key'], r['sequence']) for r in ledger if r['state'] == 'ACKED') != sorted((a['key'], a['sequence']) for a in acks):
        bad.append('ACKED is not the ACK journal')
    window, victim = None, receipt_identities(f)[0]
    if kind == 'volume_kill_io' and victim and isinstance((f['receipt'].get('Kill') or {}).get('PreKillMono'), (int, float)):
        # The row's layout has exactly one volume process (Go refuses any other), so every PUT targets it.
        window = PutFaultWindow(f['receipt']['Kill']['PreKillMono'], f['ready_mono'], victim)
    elif kind == 'client_kill_put' and len(kills) == 1:
        v = kills[0].get('victim') or {}
        window = PutFaultWindow(kills[0]['killed'], kills[0]['finished'], (v.get('pid'), v.get('starttime')))
    unknown = [r for r in ledger if r['state'] == 'UNKNOWN']
    for r in unknown:
        target = victim if kind == 'volume_kill_io' else (r['worker_identity'].get('pid'), r['worker_identity'].get('starttime'))
        ok = window is not None and r.get('attributed') == episode and window.attributes(target, r['submitted'], r['completed'])
        if kind == 'client_kill_put':
            ok = ok and r['op'] in kill_ops
        later = [x['op'] for x in ledger if x['key'] == r['key'] and x['dispatched'] > r['dispatched']] + \
            [e['op'] for e in ops if e['action'] == 'set' and e['key'] == r['key'] and e['dispatched'] > r['dispatched']]
        if later:
            bad.append('UNKNOWN %s retried by %s' % (r['op'], later[:3]))
        if any(a['key'] == r['key'] and a['sequence'] == r['sequence'] for a in acks):
            bad.append('UNKNOWN %s turned into an ACK' % r['op'])
        if not ok:
            bad.append('UNKNOWN %s outside the attributed window' % r['op'])
    attributed = sum(1 for r in unknown if r.get('attributed') == episode)
    mark('write_contract', 'FAIL' if bad else 'PASS', '%d PUTs, %d attributed UNKNOWN; %s' % (len(ledger), attributed, bad[:5]))

    # 3. Audit: a fresh client read every ledger key after the heal; each key returned what its ledger state allows.
    retired = set()
    final = set(good[-1]['present']) if good else None
    rot = Oracle()
    for vid in {typed(x, vid=int)['vid'] for x in m02['rotated']}:
        if final is not None and vid not in final and vid not in m02['dat_vids']:
            rot.retirement(vid, True, True, True)  # the oracle's existing rule: master success, topology and .dat absent
    retired = rot.retired
    fids = {(a['key'], a['sequence']): a['fids'] for a in acks}
    keys = {}
    for r in sorted(ledger, key=lambda r: r['sequence']):
        if r['state'] == 'REFUSED':
            continue  # a definite refusal before any mutation: not a ledger key of its own
        k = keys.setdefault(r['key'], dict(survivor=None, unknown=[]))
        if r['state'] == 'ACKED':
            k['survivor'] = r
        elif r['state'] == 'UNKNOWN':
            k['unknown'].append(r)
    done = [m for m in marks if m['event'] == 'audit_done']
    fresh = done and done[-1].get('client') not in [dict(pid=w.get('pid'), starttime=w.get('starttime')) for w in workers.values()] + \
        [k.get('replacement') and dict(pid=k['replacement'].get('pid'), starttime=k['replacement'].get('starttime')) for k in kills]
    audited = {a['key']: a['reads'] for a in audits}
    failed = []
    for key, k in keys.items():
        reads = audited.get(key)
        if not reads:
            continue
        allowed = ([k['survivor']] if k['survivor'] else []) + k['unknown']
        vids = {str(x).split(',')[0] for x in fids.get((key, k['survivor']['sequence']), [])} if k['survivor'] else set()
        miss_ok = bool(not k['survivor'] and k['unknown'] or (vids and vids <= {str(v) for v in retired}))

        def good(x):
            """One audit read on its own: the exact bytes of an allowed generation, or an allowed not-found."""
            case = typed(x, case=str)['case']
            if case == 'hit':
                return any(x.get('sha256') == v['digest'] and x.get('length') == v['length'] for v in allowed)
            return case == 'miss' and miss_ok
        # Every read counts: one wrong-bytes, partial or error read fails the key, whatever a later read returns.
        if not all(good(x) for x in reads):
            failed.append(key[-12:])
    covered = bool(done) and done[-1].get('complete') is True and set(audited) == set(keys) and bool(fresh)
    mark('audit', 'FAIL' if failed else 'PASS' if covered else 'NOT_COVERED',
         '%d ledger keys, %d audited, fresh %s, failed %s' % (len(keys), len(audited), bool(fresh), failed[:5]))

    # 4. Failure mode: every op terminal within the op limit, every op settled exactly once, and no crash of a
    # process the row did not target (M01 workers; every watched M02 unit; the volume, F1 aside).
    late = [e['op'] for e in ops if e['completed'] - e['submitted'] > OP_LIMIT_S + (SET_EXTRA_S if e['action'] == 'set' else 1)]
    quiet = [q for q in marks if q['event'] == 'quiescent']
    settled = sorted([e['op'] for e in ops] + sorted(kill_ops))
    terminal = len(quiet) == 1 and events[-1] is quiet[0] and settled == sorted(str(i) for i in range(1, quiet[0]['dispatched'] + 1))
    crash = []
    if kills and kind != 'client_kill_put' or len(kills) > 1:
        crash.append('client killed outside F2')
    idents = [s.get('identity') for s in samples]
    distinct = [(x['pid'], x['starttime']) for i, x in enumerate(idents) if x and x not in idents[:i]]
    if kind == 'volume_kill_io' and receipt_identities(f)[0]:
        if distinct != list(receipt_identities(f)):
            crash.append('volume identities %s are not the receipt victim then replacement' % distinct)
    elif None in idents or len(distinct) != 1:
        crash.append('volume identities %d (or a gap), want exactly 1' % len(distinct))
    for label in {k for s in samples for k in (s.get('watch') or {})}:
        if len({json.dumps(s['watch'].get(label), sort_keys=True) for s in samples}) != 1:
            crash.append('watched %s changed' % label)
    outcome = 'FAIL' if late or not terminal or crash else 'NOT_COVERED' if not samples else 'PASS'
    mark('failure_mode', outcome, 'late %s, terminal %s, %s' % (late[:5], terminal, crash[:4]))

    # 5. Recovery: an RDMA GET submitted after the heal and served within the frozen bound.
    heal = kills[0]['finished'] if kind == 'client_kill_put' and kills else f['ready_mono']
    rdma = {str(i) for i, w in workers.items() if isinstance(w, dict) and w.get('transport') in ('dc', 'rc')}
    after = [e for e in gets if str(e['worker']) in rdma and heal <= e['submitted'] <= heal + row['recovery_s']]
    served = [e for e in after if get_case(e) == 'hit' and e['completed'] <= heal + row['recovery_s']]
    mark('recovery', 'PASS' if served else 'FAIL' if after else 'NOT_COVERED',
         '%d RDMA GETs in the %d s after the heal, %d served' % (len(after), row['recovery_s'], len(served)))

    # 6. Resources: the pin bound at every sample, no whole .dat pin, and each view's quiescence class.
    pinned = [s['metrics']['pinned'] for s in samples if isinstance(s.get('metrics'), dict) and 'pinned' in s['metrics']]
    over = [p for p in pinned if p > row['pin_bound_bytes']]
    whole = [s['seq'] for s in samples if (s.get('rdma') or {}).get('mr_lens') and any(
        d > 0 and -(-d // PAGE) * PAGE in s['rdma']['mr_lens'] for d in s.get('dat_sizes') or [])]
    # The baseline is a pre-load sample provably taken before the load started: its M02 time below the earliest M02
    # time the load-start instant can map to. Without the clock join, or with no such sample, nothing is baselined.
    before_load = [s for s in samples if s['phase'] == 'pre_load' and lo is not None and s['epoch'] < begin + lo]
    classes = resource_classes(samples, row, before_load)
    outcomes = ['FAIL' if over or whole else 'PASS', 'NOT_COVERED' if not pinned else 'PASS'] + \
        ['FAIL' if v == 'leak' else 'NOT_COVERED' if v == 'NOT_COVERED' else 'PASS' for v in classes.values()]
    mark('resources', rank(outcomes), 'pin max %s bound %d, whole .dat %s, classes %s' % (
        max(pinned, default=None), row['pin_bound_bytes'], whole[:3], classes))

    # 7. Causal witness: the row's host event and its effect, joined on PID/starttime and window. R0 runs every
    # host-event check and must find none.
    events_ = host_events(kind, row, f, samples, injection, kills, ledger, lo, hi)
    if kind == 'control':
        found = [k for k, v in events_.items() if v]
        mark('causal', 'FAIL' if found else 'PASS', 'R0 host events found: %s' % found)
    else:
        outcome, why = effect(kind, row, f, samples, injection, kills, ledger, ops, workers, root, lo, hi, events_)
        mark('causal', outcome, why)
    if kind == 'conn_kill':
        claims['qp'] = 'NOT_COVERED'  # TCP scope only: no QP-state claim without a real RDMA effect
    status = rank([v['outcome'] for v in crit.values()])
    return dict(status=status, row=ROW_KINDS[kind], criteria=crit, claims=claims, unknown_attributed=attributed,
                resources='ACCOUNTED', resource_classes=classes, layout_scope=LAYOUT_SCOPE)


def receipt_identities(f):
    """(victim, replacement) as (pid, starttime) from the restart receipt; None for an absent one."""
    r = f['receipt']
    one = lambda i: (i['PID'], i['StartTime']) if isinstance(i, dict) and i.get('PID') and i.get('StartTime') else None
    return one((r.get('Kill') or {}).get('Identity')), one(r.get('Replacement'))


def hashlib_sha(data):
    import hashlib
    return hashlib.sha256(data).hexdigest()


def resource_classes(samples, row, pre):
    """Each view's quiescence class: released, retained (the product's own custody, within budget, not growing),
    leak, or NOT_COVERED (no pre-load witness, unread, or the pre-load and final samples are not the same
    pid/starttime). `pre` are the witnessed pre-load samples only."""
    quiet = [s for s in samples if s['phase'] == 'quiescence']
    out = {'data_fds': data_fd_class(quiet[-1] if quiet else None)}

    def non_data_fds(s):
        """fds that are not volume data/index files: the leak rule's own view (ruling, option a)."""
        fds, data = s.get('fds'), s.get('data_files')
        return None if fds is None or data is None or s.get('data_fds') != len(data) else fds - len(data)
    for view in RESOURCES:
        value = (lambda s: (s.get('rdma') or {}).get(view)) if view in ('qp', 'mr') else non_data_fds if view == 'fds' \
            else (lambda s: s.get(view))
        if not pre or not quiet or pre[-1].get('identity') is None or pre[-1].get('identity') != quiet[-1].get('identity') \
                or value(pre[-1]) is None or value(quiet[-1]) is None:
            out[view] = 'NOT_COVERED'
            continue
        excess = value(quiet[-1]) - value(pre[-1])
        if excess <= 0:
            out[view] = 'released'
            continue
        m = [s.get('metrics') or {} for s in quiet[-3:]]
        held_bytes = [x.get('retained_bytes') for x in m]
        held_sources = [x.get('retained_sources') for x in m]
        steady = len(m) == 3 and None not in held_bytes + held_sources and held_bytes == sorted(held_bytes, reverse=True) \
            and held_sources == sorted(held_sources, reverse=True)
        cover = {'vm_pin_kB': held_bytes[-1] / 1024 if steady else 0, 'mr': held_sources[-1] if steady else 0,
                 'fds': held_sources[-1] if steady else 0, 'qp': 0}[view]  # a QP is never custody
        within = steady and 0 < held_bytes[-1] <= row['retained_budget_bytes'] and excess <= cover
        out[view] = 'retained' if within else 'leak'
    return out


def data_fd_class(s):
    """Data fds at the end of quiescence, bounded by the volume server's own open-volume list (its /status):
    each listed volume may hold one fd per data/index file; any other data fd (an unlisted volume, or a second
    fd on one file) is surplus and a leak. No product list, or no file names: NOT_COVERED, never accounted."""
    if s is None or s.get('identity') is None or not isinstance(s.get('volumes'), list) or not isinstance(s.get('data_files'), list):
        return 'NOT_COVERED'
    listed, seen, surplus = set(s['volumes']), set(), 0
    for name in s['data_files']:
        stem, _, ext = name.rpartition('.')
        if stem not in listed or (stem, ext) in seen:
            surplus += 1
        seen.add((stem, ext))
    return 'leak' if surplus else 'accounted'


def m02_window(t1, t2, lo, hi):
    """The M02 wall-clock interval certainly containing M01 mono [t1, t2], or None without a clock join."""
    return None if lo is None else (t1 + lo, t2 + hi)


def by_epoch(samples, span):
    return [s for s in samples if span and span[0] <= s['epoch'] <= span[1]]


def host_events(kind, row, f, samples, injection, kills, ledger, lo, hi):
    """Every row's host event, each True only when observed; R0 must show none. M02 events come from the
    sampler (and the injector's own before/after), never from the controller's intent."""
    apply_, restore = injection.get('apply') or {}, injection.get('restore') or {}
    before, after = apply_.get('before') or {}, apply_.get('after') or {}
    idents = [s.get('identity') for s in samples if s.get('identity')]
    lim = [s.get('limits') for s in samples if s.get('limits')]
    port = [s.get('port') for s in samples if s.get('port')]
    downed = [p[1] for p in port]
    conns_before, conns_after = before.get('conns'), after.get('conns')
    return dict(
        kill=len({(i['pid'], i['starttime']) for i in idents}) > 1,
        client_kill=bool(kills),
        port_down=any('ACTIVE' not in p[0] for p in port) and bool(downed) and max(downed) > min(downed),
        conns_gone=bool(conns_before) and conns_after is not None and not set(conns_before) & set(conns_after),
        memlock_lowered=len({json.dumps(x.get('memlock')) for x in lim}) > 1,
        nofile_lowered=len({json.dumps(x.get('nofile')) for x in lim}) > 1,
        disk_low=any(s.get('free') is not None and s['free'] < row.get('free_below_bytes', 0) for s in samples),
        fds_at_limit=any(s.get('fds') is not None and (s.get('limits') or {}).get('nofile', [-1])[0] not in (-1, None)
                         and s['fds'] >= s['limits']['nofile'][0] for s in samples))


def effect(kind, row, f, samples, injection, kills, ledger, ops, workers, root, lo, hi, ev):
    """(outcome, reason) of the row's causal witness: PASS only with its host event AND its effect in the window;
    a missing one is NOT_COVERED."""
    apply_, restore = injection.get('apply') or {}, injection.get('restore') or {}
    start = (apply_.get('before') or {}).get('mono')
    end = (restore.get('after') or {}).get('mono')
    span = (start, end + row['recovery_s']) if start is not None and end is not None else None  # M02 mono
    ident = (apply_.get('identity') or {}) if apply_ else None
    ident = ident and dict(pid=ident.get('pid'), starttime=ident.get('starttime'))
    m01 = (f['dispatch_mono'], f['ready_mono'] + row['recovery_s'])
    nc = lambda why: ('NOT_COVERED', why)
    if kind == 'volume_kill_io':
        kw = f['receipt'].get('Kill') or {}
        pre, post = kw.get('PreKillMono'), kw.get('PostKillMono')
        if not ev['kill'] or not isinstance(pre, (int, float)) or not isinstance(post, (int, float)) or not pre < post:
            return nc('no witnessed victim kill with its pre/post-kill clock')
        arms = {a: sorted(e['op'] for e in ops if e['action'] == a and e['submitted'] <= pre and e['completed'] >= post)
                for a in ('set', 'get')}
        missing = [a for a, v in arms.items() if not v]
        return (nc('arm(s) %s with no op pending at the kill' % missing) if missing else
                ('PASS', 'pending at the kill: PUT %s, GET %s' % (arms['set'][:3], arms['get'][:3])))
    if kind == 'client_kill_put':
        k = kills[0] if len(kills) == 1 else {}
        op = str((k.get('witness') or {}).get('operation_id'))
        r = next((x for x in ledger if x['op'] == op), None)
        if not (k.get('kind') == 'client_kill_put' and r and r['state'] == 'UNKNOWN' and k.get('victim') != k.get('replacement')):
            return nc('no worker killed with a PUT pending')
        span = m02_window(k['killed'], k['finished'] + row['recovery_s'], lo, hi)
        vol = next((s['identity'] for s in by_epoch(samples, span) if s.get('identity')), None)
        before = [s for s in samples if span and s['epoch'] < span[0] and s.get('identity') == vol]
        rose = counter_rose(samples, 'released', vol, before[-1]['mono'], max((s['mono'] for s in by_epoch(samples, span)), default=-1)) \
            if before else None
        return nc('release count not observed across the kill') if rose is None else \
            ('PASS', 'PUT %s UNKNOWN, release count rose' % op) if rose else ('NOT_COVERED', 'release count did not move')
    if span is None or not ident or not ident.get('pid'):
        return nc('no injector witness')
    if kind == 'link_flap':
        b, a = (apply_.get('before') or {}).get('port'), (apply_.get('after') or {}).get('port')
        if not (ev['port_down'] and b and a and 'ACTIVE' not in a[0]):
            return nc('port never DOWN with a link_downed delta')
        rdma = {i for i, w in workers.items() if isinstance(w, dict) and w.get('transport') in ('dc', 'rc')}
        hit = [e['op'] for e in ops if str(e['worker']) in rdma and m01[0] <= e['completed'] <= m01[1]
               and (e.get('fault_error') or e.get('state') in ('REFUSED', 'UNKNOWN'))]
        fb = fallback_rose(root, workers, row, m01)
        return ('PASS', 'RDMA ops ended in clean error %s / fallback %s' % (hit[:3], fb)) if hit or fb else \
            nc('no RDMA op ended in fallback or a clean error in the window')
    if kind == 'conn_kill':
        gone = ev['conns_gone']
        new = set((restore.get('after') or {}).get('conns') or []) - set((apply_.get('before') or {}).get('conns') or [])
        if not (gone and new):
            return nc('setup connections not shown gone and replaced')
        rose = counter_rose(samples, 'released', ident, start, span[1])
        return nc('release count not observed') if rose is None else ('PASS', 'release count rose (TCP scope)') if rose \
            else nc('release count did not move')
    if kind == 'mr_pressure':
        changed = ((apply_.get('after') or {}).get('limits') or {}).get('memlock', [None])[0] == (apply_.get('plan') or {}).get('memlock')
        if not (ev['memlock_lowered'] and changed):
            return nc('memlock not lowered in /proc/pid/limits')
        t = (apply_.get('after') or {}).get('mono')
        tried = [counter_rose(samples, k, ident, t, span[1]) for k in ('registration_refused', 'registration_fallback')]
        return ('PASS', 'a registration after the change was refused or fell back') if True in tried else \
            nc('no new registration attempted after the change')
    if kind == 'disk_full':
        if not (ev['disk_low'] and ((apply_.get('after') or {}).get('free') or math.inf) < row['free_below_bytes']):
            return nc('statfs never below the threshold on the row mount')
        hit = [r['op'] for r in ledger if r['state'] in ('REFUSED', 'UNKNOWN') and m01[0] <= r['completed'] <= m01[1]]
        return ('PASS', 'PUTs ended %s' % hit[:3]) if hit else nc('no PUT ended REFUSED or UNKNOWN in the window')
    if kind == 'fd_exhaust':
        if not (ev['nofile_lowered'] and ev['fds_at_limit']):
            return nc('nofile not lowered, or fd count never at it')
        rose = counter_rose(samples, 'accept_errors', ident, (apply_.get('after') or {}).get('mono', start), span[1])
        return ('PASS', 'accept/open errors rose on that pid') if rose else nc('no failed accept or open observed')
    return nc('unknown row')


def fallback_rose(root, workers, row, window):
    """A client fallback counter (the profile's names) rose across the window on an RDMA worker's own series."""
    names = row.get('client_fallback_counters') or []
    for w in workers.values():
        if not isinstance(w, dict) or w.get('transport') not in ('dc', 'rc'):
            continue
        rows = [r for r in (read_evidence(root / stats_name(w), FILE, LINE, ROWS) or []) if r.get('status') == 'OBSERVED']
        pre = [r for r in rows if r['mono'] <= window[0]]
        post = [r for r in rows if r['mono'] >= window[1]]
        for n in names:
            get = lambda r: (r.get('stats') or {}).get('loader', {}).get(n)
            if pre and post and isinstance(get(pre[-1]), int) and isinstance(get(post[0]), int) and get(post[0]) > get(pre[-1]):
                return True
    return False


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
        row = assess_row(config) if config.get('fault_row') else assess(config)
    except Exception as error:  # any unreadable or malformed evidence fails closed
        row = dict(status='FAIL', error=repr(error)[:500], resources='NOT_OBSERVED', misses_qualification=QUALIFICATION)
        if config.get('fault_row'):
            row.update(resources='ACCOUNTED', layout_scope=LAYOUT_SCOPE)
            del row['misses_qualification']
    row['binding'] = binding(config)
    print(json.dumps(row, sort_keys=True), flush=True)


if __name__ == '__main__':
    main()
