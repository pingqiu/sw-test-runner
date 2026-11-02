"""M01 soak verdict (step 3b): one pinned job that recomputes every accepted
criterion from the run's own evidence files, never from the load's summary.
Reuses soak_oracle.Oracle (write/ack replay), soak_native_worker.payload
(digests) and soak_load's file reader, classifier and poll format. Prints one
bounded verdict row; resources are NOT_OBSERVED by ruling."""
import argparse, json, math, sys
from pathlib import Path
from soak_native_worker import payload
from soak_oracle import Oracle
from soak_load import TRANSIENT, read_bound

ROWS = 200000  # per evidence file: over this the verdict fails closed


def bounded(path, limit=4 << 20):
    with path.open('rb') as f:
        body = f.read(limit + 1)
    if len(body) > limit:
        raise ValueError('evidence over bound: ' + path.name)
    return body


def rows(root, name):
    out = []
    with (root / name).open('rb') as f:
        for raw in f:
            out.append(json.loads(raw))
            if len(out) > ROWS:
                raise ValueError('evidence over bound: ' + name)
    return out


def assess(c):
    root, crit = Path(c['root']), {}
    def mark(name, ok, reason=''):
        crit[name] = dict(outcome='PASS' if ok else 'FAIL', reason=reason)
    begin = read_bound(root / 'soak-load-start.json', {})['mono']
    acks, events, polls = rows(root, 'ack-journal.jsonl'), rows(root, 'op-events.jsonl'), rows(root, 'topology-polls.jsonl')
    faults, m02 = c['faults'], json.loads(bounded(root / 'm02-collect.json'))
    if m02.get('status') != 'SOAK_COLLECTED' or m02.get('collection') != c['collection']:
        raise ValueError('M02 collect missing or foreign')

    # Oracle replay: every SET re-derived from its seed, every GET hit against the latest ACK.
    oracle, by_seq, wrong, misses = Oracle(), {}, [], []
    ops = [('ack', a['completed'], a) for a in acks] + [('get', e['completed'], e) for e in events if e.get('action') == 'get']
    for kind, _, r in sorted(ops, key=lambda x: x[1]):
        if kind == 'ack':
            v = oracle.begin_write(r['key'], payload(r['seed'], r['length']))
            v = oracle.acknowledge(r['key'], v.sequence, r['length'], r['fids'])
            if v.digest != r['digest']:
                wrong.append(('ack', r['key']))
            by_seq[(r['key'], r['sequence'])] = (v, r)
        elif r['ok']:
            latest = oracle.latest.get(r['key'])
            if latest is None or (r['length'], r['sha256']) != (latest.length, latest.digest):
                wrong.append(('get', r['op']))
        elif r.get('miss'):
            misses.append((r, oracle.latest.get(r['key'])))  # the latest ACK at the time of THIS miss
    mark('wrong_reads', not wrong, '%d wrong' % len(wrong))
    failed = [e for e in events if e.get('action') == 'set' and not e['ok']]
    mark('failed_sets', not failed, '%d failed' % len(failed))

    # Fault errors: credited only for an acked pending GET in [P, P+60] after a dispatched Kill.
    P = {f['episode']: f for f in faults}
    bad = []
    for e in (e for e in events if e.get('fault_error')):
        f, ack = P.get(e['episode']), read_bound(root / ('fault-ack-%s.json' % e['episode']), {})
        pending = {g['op']: g['submitted'] for g in ack['pending_gets']}
        ok = (f and f['receipt']['Kill']['PreKillMono'] > 0 and e['op'] in pending and pending[e['op']] <= ack['observed_mono'] <= f['dispatch_mono']
              and f['dispatch_mono'] <= e['completed'] <= f['dispatch_mono'] + 60 and TRANSIENT.search(e.get('error') or ''))
        if not ok:
            bad.append(e['op'])
    mark('fault_errors', not bad, 'uncredited %s' % bad[:10])

    # Misses: the accepted per-miss causal join (decision 3), never a global retirement set.
    good = [p for p in polls if p['ok']]
    final_present = set(good[-1]['present']) if good else None
    rotated, dat = {r['vid'] for r in m02['rotated']}, set(m02['dat_vids'])
    def excused(e, latest):
        v, a = by_seq.get((e['key'], e['sequence']), (None, None))
        if not v or latest != v:  # the miss read must be of the latest ACKed version
            return False
        for vid in {int(fid.split(',')[0]) for fid in v.fids}:
            w = [p for p in good if p['req'] >= a['completed'] and vid in p['writables']]
            if not w:
                return False
            after = [p for p in good if p['req'] > w[-1]['req'] and p['req'] <= e['submitted'] + 25]
            unw = next((p for p in after if vid in p['present'] and vid not in p['writables']), None)
            if not unw or unw['done'] > e['submitted']:
                return False
            tail = [p for p in polls if p['req'] >= w[-1]['req'] and p['req'] <= e['completed'] + 25]
            if any(not p['ok'] for p in tail) or any(vid in p['writables'] for p in tail if p['ok'] and p['req'] > unw['req']):
                return False
            gone = next((p for p in tail if p['ok'] and vid not in p['present']), None)
            stamps = [p['req'] for p in tail if p['ok']]
            if not gone or gone['done'] > e['completed'] + 25 or any(b - a2 > 6 for a2, b in zip(stamps, stamps[1:])):
                return False
            if vid not in rotated or vid in dat:
                return False
        return True
    unexcused = [e['op'] for e, latest in misses if not excused(e, latest)]
    mark('misses', not unexcused, '%d misses, unexcused %s' % (len(misses), unexcused[:10]))

    # Coverage, body, client kill, rotation, master fault effect.
    writers, keys = {}, {}
    for a in acks:
        writers[a['writer']] = writers.get(a['writer'], 0) + 1
        keys.setdefault(a['key'], set()).add(a['writer'])
    mark('coverage', len(writers) == 4 and any(len(w) == 4 for w in keys.values()), 'writers %s' % sorted(writers))
    end = next((e['mono'] for e in events if e.get('event') == 'body_end'), None)
    late = [e['op'] for e in events if 'submitted' in e and end and e['submitted'] > end]
    mark('body', end is not None and abs(end - begin - c['duration_s']) <= 1 and not late, 'end %s, late %d' % (end, len(late)))
    kills = [e for e in events if e.get('event') == 'client_kill']
    mark('client_kill', len(kills) == 1 and kills[0]['started'] - begin >= c['client_kill_at_s'] and kills[0]['finished'] - kills[0]['started'] <= 60
         and kills[0]['witness'].get('event') == 'submitted', '%d kills' % len(kills))
    proven = sorted(v for v in rotated if final_present is not None and v not in final_present and v not in dat)
    mark('rotation', len(proven) >= 10, '%d proven' % len(proven))
    mf = next((f for f in faults if f['role'] == 'master'), None)
    before = max((max(p['present'], default=0) for p in good if mf and p['req'] < mf['dispatch_mono']), default=0)
    after = max((max(p['present'], default=0) for p in good if mf and p['req'] > mf['ready_mono']), default=0)
    mark('max_vid_advanced', mf is None or after > before, 'before %d after %d' % (before, after))

    # Routes per worker from its own native stats (DC: rdma, no fallback, no copied reads; HTTP: copied only).
    workers = json.loads((root / 'workers.json').read_text())
    pids = {str(w['pid']): i for i, w in workers.items()}
    pids.update({str(k['replacement']['pid']): str(k['worker']) for k in kills})
    route_bad = []
    for path in sorted(root.glob('native-stats-*.jsonl')):
        i = pids.get(path.name.split('-')[2])
        s = [r['stats'].get('loader', {}) for r in rows(root, path.name) if r.get('status') == 'OBSERVED']
        if i is None or not s:
            route_bad.append(path.name)
            continue
        dc = workers[i]['transport'] == 'dc' if i in workers else True
        last = s[-1]
        if dc and (last.get('transport') != 'rdma' or last.get('http_fallback_allowed') is not False or last.get('copied_reads', 0) or not last.get('direct_dc_reads')):
            route_bad.append(path.name)
        if not dc and (last.get('transport') != 'http' or last.get('direct_dc_reads', 0) or not last.get('copied_reads')):
            route_bad.append(path.name)
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


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', required=True)
    config = json.loads(parser.parse_args().config)
    config['faults'] = json.loads((Path(config['root']) / 'faults.json').read_text())
    try:
        row = assess(config)
    except Exception as error:  # any unreadable or malformed evidence fails closed
        row = dict(status='FAIL', error=repr(error)[:500], resources='NOT_OBSERVED')
    print(json.dumps(row, sort_keys=True), flush=True)


if __name__ == '__main__':
    main()
