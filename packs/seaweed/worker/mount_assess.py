#!/usr/bin/python3
"""Mount soak verdict (Stage 1 plan r4). Pure over the evidence dict; `load` reads the run root.

Product rows are FAIL only on a proven violation in a complete journal. ENVIRONMENT, INVALID and an unproven cleanup
are never a product verdict. Acks are sticky: once given, an ack stands until the ledger's authorized supersession.
"""
import argparse, hashlib, json, sys
from pathlib import Path

PRODUCT = ('no_lost_ack', 'no_lost_update', 'no_silent_corruption', 'writeback_outage', 'sync_control_honest',
           'mounts_alive', 'bounded_ops')
OP_LIMIT_S, HEAL_GRACE_S, ERR_GRACE_S, RECOVER_S = 60, 120, 60, 120
FLOORS = dict(overlapping_chmods=1000, cohort_per_episode=20)
ERROR_ERRNOS = ('EIO', 'ESTALE')
# The operation certainly did not happen; anything else leaves its outcome unknown.
DEFINITE_FAILURES = ('EACCES', 'EPERM', 'ENOTEMPTY', 'EINVAL', 'EROFS')
PHASE_W1_FLOOR = 3


def payload(seed, n):
    out, i = bytearray(), 0
    while len(out) < n:
        out += hashlib.sha256(('%s/%d' % (seed, i)).encode()).digest()
        i += 1
    return bytes(out[:n])


def is_prefix(seed, length, size, sha):
    return 0 <= size <= length and hashlib.sha256(payload(seed, size)).hexdigest() == sha


class Gen:
    def __init__(self, r):
        self.id, self.sha, self.size, self.seed = r['gen'], r['sha'], r['size'], r['seed']
        self.acked = bool(r['full']) and r.get('fsync') == 0 and r.get('rename', 0) == 0

    def matches(self, sha, size):
        return (sha, size) == (self.sha, self.size) or (not self.acked and is_prefix(self.seed, self.size, size, sha))


class Occurrence:
    """A generation at one name for one interval. A later rename or delete closes the interval but never erases it, so
    a read judged inside its window stays judged."""

    def __init__(self, gen, ack_end, epoch):
        self.gen, self.epoch, self.windows = gen, epoch, [[ack_end, float('inf')]]

    def is_open(self):
        return self.windows[-1][1] == float('inf')

    def covers(self, r):
        return any(a <= r['start'] and r['end'] <= b for a, b in self.windows)


def definitely_left_the_name_alone(r):
    """True when a mutation certainly did not change this name. Its own in-flight interval stays unjudged."""
    if r['kind'] in ('delete', 'rename'):
        return r['rc'] != 0 and r.get('errno') in DEFINITE_FAILURES
    if r['kind'] == 'replace':  # written to a temporary name; only the rename touches this one
        return not r['full'] or r.get('fsync') != 0 or (r.get('rename', 0) != 0 and r.get('errno') in DEFINITE_FAILURES)
    return False


def ledger(events):
    """W1 replay -> survivors (path -> occurrence), history (path -> [occurrence]), epochs, alt, uncertain.
    A definite failure keeps the ack's obligation; only an unknown outcome is uncertain, which is INVALID, never PASS."""
    survivors, history, epoch, alt, uncertain = {}, {}, {}, {}, set()

    def place(path, gen, end):
        o = Occurrence(gen, end, epoch.get(path, 0))
        history.setdefault(path, []).append(o)
        return o
    for r in sorted((e for e in events if e['t'] == 'mut'), key=lambda e: e['start']):
        kind, path = r['kind'], r['path']
        was_open = [o for o in history.get(path, ()) if o.is_open()]
        for o in was_open:
            o.windows[-1][1] = r['start']
        if definitely_left_the_name_alone(r):  # the bytes are unchanged: judged again once the failed op is over
            for o in was_open:
                o.windows.append([r['end'], float('inf')])
        if kind in ('create', 'replace'):
            g = Gen(r)
            o = place(path, g, r['end'])
            if g.acked:
                survivors[path] = o
        elif kind == 'rename':
            o = survivors.get(path)
            if o and r['rc'] == 0:
                del survivors[path]
                epoch[path] = epoch.get(path, 0) + 1
                survivors[r['to']] = place(r['to'], o.gen, r['end'])
            elif o and r.get('errno') not in DEFINITE_FAILURES:
                alt.setdefault(o.gen.id, set()).add(r['to'])
                uncertain.add(o.gen.id)
        elif kind == 'delete':
            o = survivors.get(path)
            if r['rc'] == 0:
                survivors.pop(path, None)
                epoch[path] = epoch.get(path, 0) + 1
            elif o and r.get('errno') not in DEFINITE_FAILURES:
                uncertain.add(o.gen.id)
    return survivors, history, epoch, alt, uncertain


def judge_w1(events, fail):
    """-> ids of acked generations whose absence is explained only by a mutation of unknown outcome."""
    survivors, history, epoch, alt, uncertain = ledger(events)
    finals, unresolved = {r['path']: r for r in events if r['t'] == 'final'}, []
    for path, o in survivors.items():
        here = [finals.get(p) for p in [path, *alt.get(o.gen.id, ())]]
        if any(f and f['rc'] == 0 and (f['sha'], f['size']) == (o.gen.sha, o.gen.size) for f in here):
            continue
        if o.gen.id in uncertain and not any(f and f['rc'] == 0 for f in here):
            unresolved.append(o.gen.id)
        else:
            fail('no_lost_ack', 'survivor %s %s absent or not exact' % (path, o.gen.id))
    for path, f in finals.items():
        if f['rc'] == 0 and path not in survivors:
            live = [o for o in history.get(path, ()) if o.epoch == epoch.get(path, 0)]
            if not any(o.gen.matches(f['sha'], f['size']) for o in live) and not any(path in paths for paths in alt.values()):
                fail('no_silent_corruption', 'final %s matches no generation' % path)
    for r in (e for e in events if e['t'] == 'read'):
        occs = history.get(r['path'], [])
        current = [o for o in occs if o.gen.acked and o.covers(r)]
        if not current:
            continue  # outside every immutable window: not judged
        o = current[-1]
        earlier = [x for x in occs[:occs.index(o)] if x.epoch == o.epoch]
        if r['rc'] == 0:
            if not any(x.gen.matches(r['sha'], r['size']) for x in [o, *earlier]):
                fail('no_silent_corruption', 'read %s matches no allowed generation' % r['op'])
        elif r.get('errno') == 'ENOENT' and earlier:
            fail('no_silent_corruption', 'read %s ENOENT with an earlier generation' % r['op'])
    return unresolved


def is_valid_prefix(seqs):
    return seqs == list(range(len(seqs)))


def judge_w2(events, fail):
    acked, mode_values, xattr_values = {}, {}, {}
    for r in events:
        if r['t'] == 'app' and r['full'] and r.get('fsync') == 0:
            acked.setdefault(r['file'], set()).add(r['seq'])
        elif r['t'] == 'attr':
            mode_values.setdefault(r['file'], set()).add(r['mode'])
            xattr_values.setdefault(r['file'], set()).add(r['xattr'])
    for r in (e for e in events if e['t'] == 'wread'):
        if not r['valid'] or not is_valid_prefix(r['seqs']):
            fail('no_silent_corruption', 'read of %s is not a gap-free record prefix' % r['file'])
        if r.get('mode') is not None and r['mode'] not in mode_values.get(r['file'], ()) and r['mode'] != r.get('initial_mode'):
            fail('no_silent_corruption', '%s shows a mode never set' % r['file'])
    finals = {r['file']: r for r in events if r['t'] == 'barrier'}
    for f, seqs in acked.items():
        b = finals.get(f)
        if not b:
            continue  # the settled check makes this INVALID
        if not b['valid'] or not is_valid_prefix(b['seqs']) or not seqs <= set(b['seqs']):
            fail('no_lost_update', '%s final records miss an acked append' % f)
        elif not (b['sentinel_acked'] and b['seen_mode'] == b['sentinel_mode'] and b['seen_xattr'] == b['sentinel_xattr']):
            fail('no_lost_update', '%s barrier sentinel not shown' % f)


def judge_w3(events, cohorts, fail):
    outcomes = {(r['ep'], r['path']): r for r in events if r['t'] == 'w3final'}
    for ep, c in cohorts.items():
        for f in c['files']:
            o = outcomes.get((ep, f['path']))
            if not o or not o['chunks'] or (o['sha'], o['size']) != (f['sha'], f['size']) or o['after_heal_s'] > RECOVER_S:
                fail('writeback_outage', '%s %s not intact within %d s of heal' % (ep, f['path'], RECOVER_S))
    sync = {r['path']: r for r in events if r['t'] == 'sync_final'}
    for r in (e for e in events if e['t'] == 'w3' and e['mount'] == 'sync'):
        if r['full'] and r.get('fsync') == 0:
            s = sync.get(r['path'])
            if not s or s['rc'] != 0 or (s['sha'], s['size']) != (r['sha'], r['size']):
                fail('sync_control_honest', 'acked sync file %s absent or not exact' % r['path'])


def judge_liveness(events, windows, fail):
    inside = lambda t, grace: any(a <= t <= h + grace for a, h in windows)
    for r in events:
        if r['t'] == 'mount_exit' and r['after_start']:
            fail('mounts_alive', 'mount %s exited' % r['mount'])
        if 'op' in r and 'start' in r and 'end' in r:  # operations only: a phase row spans minutes by design
            if r.get('errno') in ERROR_ERRNOS and not inside(r['end'], ERR_GRACE_S):
                fail('mounts_alive', 'op %s %s outside a fault window' % (r.get('op'), r['errno']))
            dur = r['end'] - r['start']
            if windows and any(a <= r['end'] <= h + HEAL_GRACE_S for a, h in windows):
                pass
            elif dur > OP_LIMIT_S:
                fail('bounded_ops', 'op %s took %.1f s' % (r.get('op'), dur))
            if any(r['start'] < h and h + HEAL_GRACE_S < r['end'] for a, h in windows):  # started before that heal
                fail('bounded_ops', 'op %s still running %d s after heal' % (r.get('op'), HEAL_GRACE_S))


def overlapping_chmods(events):
    apps = {}
    for r in (e for e in events if e['t'] == 'app'):
        apps.setdefault(r['file'], []).append((r['start'], r['end']))
    return sum(any(a < r['end'] and r['start'] < b for a, b in apps.get(r['file'], ()))
               for r in events if r['t'] == 'attr')


def gates(d, events, unresolved=()):
    """-> (status or None, reasons). Nothing here is a product verdict."""
    if not d['env'].get('fuse_ok') or not d['env'].get('mounts_started'):
        return 'ENVIRONMENT', ['FUSE or mount start failed before the first op']
    why = []
    if not d['journal_complete']:
        why.append('journal missing, unparseable or unsettled')
    if unresolved:
        why.append('acked generations absent after a mutation of unknown outcome: %s' % sorted(unresolved)[:5])
    for ep, f in d['faults'].items():
        missing = [k for k in ('rule_confirmed', 'counter_rose', 'filer_ok', 'rule_gone', 'argv_direct') if not f.get(k)]
        if missing:
            why.append('fault %s not witnessed: %s' % (ep, missing))
    if not d['faults']:
        why.append('no fault episode')
    enospc = sum(1 for r in events if r.get('errno') == 'ENOSPC')
    if enospc:  # a full test cluster proves nothing about the product
        why.append('capacity exhausted: %d ENOSPC ops' % enospc)
    for ep, c in d['cohorts'].items():
        if len(c['files']) < FLOORS['cohort_per_episode']:
            why.append('cohort %s below floor: %d' % (ep, len(c['files'])))
        if not c.get('frozen_before_heal'):
            why.append('cohort %s not frozen before heal' % ep)
    if set(d['cohorts']) != set(d['faults']):
        why.append('cohort/fault episodes differ')
    n = overlapping_chmods(events)
    if n < FLOORS['overlapping_chmods']:
        why.append('coverage: %d overlapping chmods' % n)
    wrote = {r['mount'] for r in events if r['t'] in ('mut', 'app')}
    if not set(d['env'].get('mounts', ())) <= wrote:
        why.append('coverage: not every mount wrote')
    phases = [r for r in events if r['t'] == 'phase']
    if len(phases) != 3:
        why.append('coverage: %d load phases journaled, want 3' % len(phases))
    for p in phases:
        n = sum(1 for r in events if r['t'] == 'mut' and r['kind'] in ('create', 'replace') and r['full'] and r.get('fsync') == 0
                and p['start'] <= r['end'] <= p['end'])
        if n < PHASE_W1_FLOOR:
            why.append('coverage: phase %s has %d acked W1 writes' % (p['name'], n))
    finals = {r['file'] for r in events if r['t'] == 'barrier'}
    if {r['file'] for r in events if r['t'] == 'app'} - finals:
        why.append('a W2 file has no final barrier')
    return ('INVALID' if why else None), why


def assess(d):
    events, rows = d['events'], {k: dict(outcome='PASS', reason=[]) for k in PRODUCT}

    def fail(row, reason):
        rows[row]['outcome'] = 'FAIL'
        rows[row]['reason'] = (rows[row]['reason'] + [reason])[:10]
    unresolved = judge_w1(events, fail)
    judge_w2(events, fail)
    judge_w3(events, d['cohorts'], fail)
    judge_liveness(events, [(f['apply'], f['heal']) for f in d['faults'].values() if 'apply' in f and 'heal' in f], fail)
    gate, why = gates(d, events, unresolved)
    failed = [k for k in PRODUCT if rows[k]['outcome'] == 'FAIL']
    if gate == 'ENVIRONMENT':
        status = gate
    elif gate == 'INVALID':
        status = gate
    elif failed:
        status = 'FAIL'
    elif d['cleanup'] != 'PROVEN':
        status = 'CLEANUP_UNKNOWN'
    else:
        status = 'PASS'
    # A violation proven on a complete journal is reported even when a coverage floor is short.
    return dict(schema='seaweed-mount-verdict/1', status=status, cleanup=d['cleanup'], rows=rows, gate_reasons=why,
                violations_proven=failed if d['journal_complete'] else [])


def strict(raw):
    def pairs(items):
        if len({k for k, _ in items}) != len(items):
            raise ValueError('duplicate key')
        return dict(items)
    return json.loads(raw, object_pairs_hook=pairs, parse_constant=lambda c: (_ for _ in ()).throw(ValueError(c)))


def load(root):
    root, events, complete = Path(root), [], True
    try:
        for line in (root / 'mount-journal.jsonl').read_text().splitlines():
            events.append(strict(line))
        ends = [e for e in events if e['t'] == 'end']
        complete = len(ends) == 1 and ends[0]['dispatched'] == sum(1 for e in events if 'op' in e)
    except (OSError, ValueError):
        complete = False
    read = lambda name, default: strict((root / name).read_text()) if (root / name).exists() else default
    return dict(events=events, journal_complete=complete, env=read('mount-env.json', {}), faults=read('mount-faults.json', {}),
                cohorts=read('mount-cohorts.json', {}), cleanup=read('mount-cleanup.json', 'UNKNOWN'))


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--root', required=True)
    verdict = assess(load(parser.parse_args().root))
    print(json.dumps(verdict, sort_keys=True))
    # A verdict was produced: FAIL is a result for the caller to publish, not an assessor error.
