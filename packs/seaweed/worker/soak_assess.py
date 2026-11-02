"""M01 soak verdict (step 3b): one pinned job that recomputes every accepted
criterion from the run's own evidence files, never from the load's summary.
Reuses soak_oracle (write/ack replay, Version.matches), soak_native_worker.payload
(digests) and soak_load's strict reader, classifiers and poll format. Prints one
bounded verdict row bound to its run; resources are NOT_OBSERVED by ruling."""
import argparse, json, math, sys, types
from pathlib import Path

if 'soak_load' not in sys.modules:  # -I drops the script dir: the exact protected sibling, which pins its own
    _source = Path(__file__).resolve().with_name('soak_load.py')
    _module = sys.modules['soak_load'] = types.ModuleType('soak_load')
    _module.__file__ = str(_source)
    exec(compile(_source.read_bytes(), str(_source), 'exec'), _module.__dict__)
from soak_native_worker import payload
from soak_oracle import Oracle
from soak_load import MISS, TRANSIENT, read_bound, read_evidence
from soak_stats import WITNESS, boot_id

SCHEMA = 'seaweed-soak-verdict/1'
MANDATORY = ('wrong_reads', 'failed_sets', 'get_outcomes', 'terminal', 'fault_errors', 'misses', 'coverage', 'body',
             'client_kill', 'rotation', 'max_vid_advanced', 'routes')  # Go's soakCriteria, same set
FILE, LINE, ROWS, OBJECT = 64 << 20, 64 << 10, 200000, 4 << 20
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


def lines(root, name):
    rows = read_evidence(root / name, FILE, LINE, ROWS)
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
    case = {(True, False, False): 'hit', (False, True, False): 'miss', (False, False, True): 'fault'}.get(
        (e['ok'], e.get('miss') is True, e.get('fault_error') is True), 'unknown')
    try:
        if case == 'hit':
            typed(e, length=int, sha256=str, sequence=int)
        elif case == 'miss':
            typed(e, sequence=int, fids=list, error=str)
            case = case if MISS.search(e['error']) else 'unknown'
        elif case == 'fault':
            typed(e, sequence=int, episode=str)
    except ValueError:
        return 'unknown'
    return case


def miss_excused(e, version, ack, good, polls, rotated, dat):
    """Decision 3, per miss: the latest-ACK version, and for each of its VIDs an uninterrupted writable-exit
    candidate around the miss, no gap > 6 s, no VID reuse anywhere later, the M02 rotation line and no .dat."""
    for vid in {int(fid.split(',')[0]) for fid in version.fids}:
        w = [p for p in good if p['req'] >= ack['completed'] and vid in p['writables']]
        if not w:
            return False
        after = [p for p in good if w[-1]['req'] < p['req'] <= e['submitted'] + 25]
        unw = next((p for p in after if vid in p['present'] and vid not in p['writables']), None)
        if not unw or unw['done'] > e['submitted']:
            return False
        tail = [p for p in polls if w[-1]['req'] <= p['req'] <= e['completed'] + 25]
        if any(not p['ok'] for p in tail) or any(vid in p['writables'] for p in tail if p['ok'] and p['req'] > unw['req']):
            return False
        gone = next((p for p in tail if p['ok'] and vid not in p['present']), None)
        stamps = [p['req'] for p in tail if p['ok']]
        if not gone or gone['done'] > e['completed'] + 25 or any(b - a > 6 for a, b in zip(stamps, stamps[1:])):
            return False
        if any(vid in p['present'] for p in good if p['req'] > gone['req']):  # VID reuse: never excusable
            return False
        if vid not in rotated or vid in dat:
            return False
    return True


def stats_name(identity):
    return 'native-stats-%d-%d.jsonl' % (typed(identity, pid=int)['pid'], typed(identity, starttime=int)['starttime'])


def assess_routes(root, workers, kills):
    """Exactly one native-stats series per worker generation (4 originals + each replacement), none foreign,
    and each one's last observed loader counters in the accepted DC or HTTP identity."""
    expected = {stats_name(w): w['transport'] for w in workers.values()}
    expected.update({stats_name(k['replacement']): workers[str(k['worker'])]['transport'] for k in kills})
    present = {p.name for p in root.iterdir() if p.name.startswith('native-stats-')}
    bad = sorted(present ^ set(expected))
    for name in sorted(present & set(expected)):
        loaders = [typed(typed(r, stats=dict)['stats'], loader=dict)['loader'] for r in lines(root, name)
                   if typed(r, status=str)['status'] == 'OBSERVED']
        if not loaders:
            bad.append(name)
            continue
        last = typed(loaders[-1], transport=str, **{k: int for k in COUNTERS})
        dc, rc, copied = (last[k] for k in COUNTERS)
        if expected[name] == 'dc':
            ok = last['transport'] == 'rdma' and last.get('http_fallback_allowed') is False and dc > 0 and rc == 0 and copied == 0
        else:
            ok = last['transport'] == 'http' and dc == 0 and rc == 0 and copied > 0
        if not ok or min(dc, rc, copied) < 0:
            bad.append(name)
    return bad


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
    events = [typed(e) for e in lines(root, 'op-events.jsonl')]
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
    workers = obj(root, 'workers.json')
    if not isinstance(workers, dict) or sorted(workers) != ['0', '1', '2', '3'] or \
            [typed(workers[i], pid=int, starttime=int, transport=str)['transport'] for i in '0123'] != ['dc', 'dc', 'dc', 'http']:
        raise ValueError('workers.json is not the accepted 3 DC + 1 HTTP set')

    # Oracle replay: every SET re-derived from its seed, every GET outcome exactly one recognised case.
    oracle, by_seq, wrong, misses, unknown = Oracle(), {}, [], [], []
    gets = [e for e in ops if e['action'] == 'get']
    for kind, _, r in sorted([('ack', a['completed'], a) for a in acks] + [('get', e['completed'], e) for e in gets], key=lambda x: x[1]):
        if kind == 'ack':
            v = oracle.begin_write(r['key'], payload(r['seed'], r['length']))
            v = oracle.acknowledge(r['key'], v.sequence, r['length'], r['fids'])
            if v.digest != r['digest']:
                wrong.append(('ack', r['key']))
            by_seq[(r['key'], r['sequence'])] = (v, r)
            continue
        case, latest = get_case(r), oracle.latest.get(r['key'])
        if case == 'hit' and (latest is None or not latest.matches(r['length'], r['sha256'])):
            wrong.append(('get', r['op']))
        elif case == 'miss':
            misses.append((r, latest))  # the latest ACK at the time of THIS miss
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

    # Misses: the accepted per-miss causal join (decision 3), never a global retirement set.
    rotated = {typed(r, vid=int)['vid'] for r in m02['rotated']}
    dat = set(m02['dat_vids'])
    def excused(e, latest):
        v, a = by_seq.get((e['key'], e['sequence']), (None, None))
        return bool(v) and latest == v and miss_excused(e, v, a, good, polls, rotated, dat)
    unexcused = [e['op'] for e, latest in misses if not excused(e, latest)]
    mark('misses', not unexcused, '%d misses, unexcused %s' % (len(misses), unexcused[:10]))

    # Coverage, body, client kill, rotation, master fault effect.
    writers, keys = {}, {}
    for a in acks:
        writers[a['writer']] = writers.get(a['writer'], 0) + 1
        keys.setdefault(a['key'], set()).add(a['writer'])
    mark('coverage', sorted(writers) == [0, 1, 2, 3] and any(len(w) == 4 for w in keys.values()), 'writers %s' % sorted(writers))
    ends = [typed(e, mono=float)['mono'] for e in marks if e['event'] == 'body_end']
    late = [e['op'] for e in ops if ends and e['dispatched'] > ends[0]]
    mark('body', len(ends) == 1 and abs(ends[0] - begin - c['duration_s']) <= 1 and not late, 'ends %s, late %d' % (ends, len(late)))
    k = kills[0] if len(kills) == 1 else None
    w = k and k['witness']
    original = k and workers.get(str(k['worker']))
    ok = bool(k) and k['worker'] in (0, 1, 2) and k['started'] - begin >= c['client_kill_at_s'] and 0 <= k['killed'] - k['started'] <= 60 \
        and 0 <= k['finished'] - k['killed'] <= 30 and w.get('event') == 'submitted' and w.get('action') == 'get' \
        and w.get('pid') == original['pid'] and k['replacement'].get('pid') != original['pid']
    mark('client_kill', ok, '%d kills' % len(kills))
    final = set(good[-1]['present']) if good else None
    proven = sorted(v for v in rotated if final is not None and v not in final and v not in dat)
    mark('rotation', len(proven) >= 10, '%d proven' % len(proven))
    mf = next((f for f in faults if f['role'] == 'master'), None)
    before = max((max(p['present'], default=0) for p in good if mf and p['req'] < mf['dispatch_mono']), default=0)
    after = max((max(p['present'], default=0) for p in good if mf and p['req'] > mf['ready_mono']), default=0)
    mark('max_vid_advanced', mf is not None and after > before, 'before %d after %d' % (before, after))
    route_bad = assess_routes(root, workers, kills)
    mark('routes', not route_bad, 'bad %s' % route_bad[:6])

    # K7 (a FINDING, never a FAIL): the first non-HTTP SET after each fault's successful READY.
    k7 = []
    for f in faults:
        first = min((a for a in acks if a['submitted'] >= f['ready_mono'] and a['writer'] != 3), key=lambda a: a['submitted'], default=None)
        if first:
            lat = first['completed'] - first['submitted']
            k7.append(dict(episode=f['episode'], role=f['role'], dt_after_ready_s=first['submitted'] - f['ready_mono'],
                           latency_s=lat, at_rc_timeout=lat >= 4.9))
    status = 'PASS' if all(v['outcome'] == 'PASS' for v in crit.values()) else 'FAIL'
    return dict(status=status, criteria=crit, k7=k7, resources='NOT_OBSERVED')


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
        row = dict(status='FAIL', error=repr(error)[:500], resources='NOT_OBSERVED')
    row['binding'] = binding(config)
    print(json.dumps(row, sort_keys=True), flush=True)


if __name__ == '__main__':
    main()
