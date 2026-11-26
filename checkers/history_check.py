"""Checkers of a run's history file (runner spec S4), judging the history alone.

python3 -I history_check.py HISTORY.jsonl [--json OUT]

Each checker returns PASS, FAIL or UNKNOWN with its findings. A history that
does not parse, or breaks the writer's own rules, is UNKNOWN for every
checker: nothing is judged from a file that cannot be read whole. The exit
code is 0 only when every checker passes.

  C0  every invoke has its complete; no outcome is unknown; a refusal only
      where its own step declared it (args.expect = refused), never by verb;
      a declared refusal answered ok is a finding (half of the spec's C6);
      every fault applied (outcome ok or unknown) is restored: a fault_restore
      of the same fault_key completed ok later in the file, else 'fault left
      applied'; a fault operation whose outcome is unknown is a RUNNER
      failure (the environment is not known), never a product result
  C1  every acknowledged write is read back: each read returns a value the
      shadow allows; and no written block goes unread after its last write
      (a count that must be 0; a later delete does not excuse it)
  C2  a restore target's FIRST read of each block is a content the source's
      shadow allowed between the snapshot's invoke and complete
  C3  independence after a restore, both ways: no write to the source made
      after the snapshot shows in the target, and no write to the target shows
      in the source
  C5  a refused operation changed nothing: every completed operation with
      outcome refused (any verb) is judged between two observations of its
      volume (records of kind and verb observe; the generator's gen_* records
      are not observations). BEFORE is the latest observation of the volume
      before the refusal's invoke, with no operation touching the volume
      completing ok or unknown in between; AFTER is the first observation of
      the volume after the refusal's complete, with no operation touching the
      volume invoked in between. An operation touches its volume, a restore
      also its source, and a fault operation every volume (the history does
      not map volumes to VMs). Compared: result.head.gen (any write to HEAD
      moves the generation) and result.files as a sorted list. Both must be
      recorded in both observations (HEAD a record with an integer gen, files
      a list): an observation with show_error, an unreadable HEAD or a
      listing_error proves nothing either way. FAIL if a judged refusal shows
      a different generation or listing; a refusal without both observations
      is not judged (a note). UNKNOWN if there were refusals and none could be
      judged; PASS if one was judged and none changed anything; FAIL (not
      exercised) if there was no refusal at all, like the other checkers
  C7  after a delete with outcome ok, the volume's next observation shows HEAD
      in state deleted (the tombstone), and its directory holds exactly HEAD
      and VOLUME: any pool, seg, root or RETENTION object is a FAIL, and so is
      a missing HEAD (the tombstone keeps a later volume of the same name out
      of this directory). In v1 a create of that name afterwards is refused
      (FailedPrecondition: the master keeps the name's deletion reservation).
  C6  bounds and "none stays unknown". Latency: an operation is judged only
      against a bound an operator page states (BOUNDS, with its page and
      line); a verb without one is NOT JUDGED and listed ("no stated bound"),
      and so is an operation open while a process was down (the table holds
      no per-fault bound). Unknowns: a resolution point is a `healthy`
      observation with healthy true and no operation in flight, or the end
      record. Every operation whose outcome is unknown (or that never
      completed) is due at the first point after it, and must be RESOLVED by
      an observation after that point (after its completion when the point is
      the end): a write by an ok read of the same block returning its value
      (took effect) or a value the block allowed before it (did not); a
      snapshot by the volume's root/ objects against the observation before
      it (a new root: took effect; the same roots: did not); a delete, move
      or restore by the status (HEAD) and the listing. FAIL on a third state,
      on an unknown no clean observation places (none after the point, or the
      volume or block changed in between), on a run whose last healthy
      record is not healthy, and on a kill with no healthy point after it.
      A read changes nothing and needs no placing; an unknown of another
      verb has no rule and makes the verdict UNKNOWN. FAIL (not exercised)
      if the history holds neither an unknown nor a healthy record.

A fault is an operation of module Runner (verbs fault_apply and fault_restore,
args fault_key, resource, vm_id), never a record kind of its own. A fault is
ACTIVE from its apply's INVOKE to its restore's COMPLETE, the widest interval:
fault_windows() is that definition and every checker bound to faults uses it.
After a fault operation whose outcome is unknown, C1 C2 C3 answer UNKNOWN: the
history does not map volumes to resources, so no volume is judged.

The shadow (spec S3): per volume and block, the set of values the block may
hold. A new volume holds zeros. An acknowledged write makes its value the
only one; a write whose outcome is unknown adds its value, and that value
stays possible beside every later one (the runner cannot know that the write
can no longer land). A restore target starts as its snapshot's sets.
"""
import argparse
import hashlib
import json
import sys

ZERO = hashlib.sha256(bytes(4096)).hexdigest()
KINDS = {'invoke', 'complete', 'observe', 'end'}   # a fault is an operation of module Runner, not a kind
# The master's answer to a create of a deleted name (v1: the name's deletion
# reservation). C7 requires this reason, not any FailedPrecondition.
RESERVED_NAME = 'is being deleted or was deleted'
OUTCOMES = {'ok', 'refused', 'unknown'}
TOMBSTONE = {'HEAD', 'VOLUME'}   # a deleted volume's directory keeps exactly these


class Malformed(ValueError):
    pass


def read_history(lines):
    """Records in file order; Malformed for anything the writer never writes."""
    recs, open_inv, done = [], {}, set()
    last_mono = -1
    for n, raw in enumerate(lines, 1):
        raw = raw.strip()
        if not raw:
            continue
        try:
            r = json.loads(raw)
        except ValueError as e:
            raise Malformed('line %d: %s' % (n, e))
        if not isinstance(r, dict) or r.get('kind') not in KINDS:
            raise Malformed('line %d: no kind' % n)
        if r.get('seq') != len(recs) + 1:
            raise Malformed('line %d: seq %r, want %d' % (n, r.get('seq'), len(recs) + 1))
        mono = r.get('mono_ns')
        if not isinstance(mono, int) or mono < last_mono:
            raise Malformed('line %d: clock %r after %d' % (n, mono, last_mono))
        last_mono = mono
        if recs and recs[-1]['kind'] == 'end':
            raise Malformed('line %d: a record after the end record' % n)
        k = r['kind']
        if k == 'invoke':
            if not r.get('op') or not r.get('verb'):
                raise Malformed('line %d: invocation without op or verb' % n)
            open_inv[r['seq']] = r
        elif k == 'complete':
            of = r.get('of')
            if of in done or of not in open_inv:
                raise Malformed('line %d: completion of %r, not an open invocation' % (n, of))
            if r.get('outcome') not in OUTCOMES:
                raise Malformed('line %d: outcome %r' % (n, r.get('outcome')))
            inv = open_inv.pop(of)
            if (r.get('op'), r.get('verb'), r.get('volume')) != (inv.get('op'), inv.get('verb'), inv.get('volume')):
                raise Malformed('line %d: completion does not match its invocation' % n)
            done.add(of)
        recs.append(r)
    return recs


def operations(recs):
    """(invoke, complete or None) per invocation, in invocation order."""
    comp = {r['of']: r for r in recs if r['kind'] == 'complete'}
    return [(r, comp.get(r['seq'])) for r in recs if r['kind'] == 'invoke']


FAULT_VERBS = ('fault_apply', 'fault_restore')


def fault_windows(recs):
    """The faults of the history: (fault_key, resource, start, end), start the
    seq of the apply's INVOKE, end the seq of the restore's COMPLETE with
    outcome ok, or None while the fault was never proven restored. The fault
    is active over [start, end]: the widest interval, so a checker never
    takes a moment the fault may have covered for one it did not."""
    out, open_ = [], {}
    for inv, comp in operations(recs):
        if inv['verb'] not in FAULT_VERBS:
            continue
        key = (inv.get('args') or {}).get('fault_key')
        if inv['verb'] == 'fault_apply':
            if comp is None or comp['outcome'] != 'refused':
                open_[key] = len(out)
                out.append([key, (inv.get('args') or {}).get('resource'), inv['seq'], None])
        elif key in open_ and comp is not None and comp['outcome'] == 'ok':
            out[open_.pop(key)][3] = comp['seq']
    return [tuple(w) for w in out]


def unknown_environment(recs):
    """The seq of the first fault operation whose outcome is unknown (or that
    never completed), or None."""
    for inv, comp in operations(recs):
        if inv['verb'] in FAULT_VERBS and (comp is None or comp['outcome'] == 'unknown'):
            return inv['seq']
    return None


def verdict(findings, unknown=False):
    if findings:
        return {'verdict': 'FAIL', 'findings': findings}
    return {'verdict': 'UNKNOWN' if unknown else 'PASS', 'findings': []}


def c0(recs):
    """A refusal is expected only where its own invocation declares it
    (args.expect == "refused", one step of the scenario), never by verb."""
    f = []
    if not operations(recs):
        return verdict(['no operation in the history: nothing was exercised'])
    for inv, comp in operations(recs):
        name = '%s %s seq %d' % (inv['verb'], inv.get('volume', ''), inv['seq'])
        if inv['verb'] in FAULT_VERBS:
            if comp is None or comp['outcome'] != 'ok':
                f.append('RUNNER failure, not a product result: %s %s seq %d %s: the environment is not known: %s' % (
                    inv['verb'], (inv.get('args') or {}).get('fault_key', ''), inv['seq'],
                    'never completed' if comp is None else 'outcome ' + comp['outcome'],
                    '' if comp is None else (comp.get('result') or {}).get('detail', '')))
            continue
        expected = (inv.get('args') or {}).get('expect') == 'refused'
        if comp is None:
            f.append('no completion: ' + name)
        elif comp['outcome'] == 'unknown':
            f.append('outcome unknown: %s: %s' % (name, (comp.get('result') or {}).get('detail', '')))
        elif comp['outcome'] == 'refused' and not expected:
            f.append('refusal not expected: %s: %s' % (name, (comp.get('result') or {}).get('detail', '')))
        elif comp['outcome'] == 'ok' and expected:
            f.append('declared refusal answered ok: ' + name)
    for key, resource, start, end in fault_windows(recs):
        if end is None:
            f.append('fault left applied: %s on %s (applied at seq %d, no fault_restore completed ok after it)' % (key, resource, start))
    return verdict(f)


class Shadow:
    """What each block of each volume may hold, built in completion order."""

    def __init__(self):
        self.allowed = {}   # (volume, lba) -> set of sha256
        self.pending = {}   # (volume, lba) -> values of unknown writes (never shrink)
        self.base = {}      # volume -> {lba: set} its initial content (a restore's snapshot)

    def may_hold(self, vol, lba):
        s = self.allowed.get((vol, lba))
        if s is None:
            s = set(self.base.get(vol, {}).get(lba, {ZERO}))
        return s | self.pending.get((vol, lba), set())

    def write(self, vol, lba, sha, outcome):
        if outcome == 'ok':
            self.allowed[(vol, lba)] = {sha}
        elif outcome == 'unknown':
            self.allowed[(vol, lba)] = self.may_hold(vol, lba) | {sha}
            self.pending.setdefault((vol, lba), set()).add(sha)

    def lbas(self, vol):
        return {lba for (v, lba) in list(self.allowed) + list(self.pending) if v == vol} | set(self.base.get(vol, {}))


def overlaps(recs):
    """Volumes on which two operations were open at once. The data checkers
    apply operations at their completion and freeze a snapshot as its
    invoke set plus the writes completed during it: right only when the
    operations on a volume never overlap (M0 issues them one at a time). On
    an overlap they answer UNKNOWN rather than judge. A restore touches its
    source and its target."""
    open_on, found = {}, set()
    for r in recs:
        if r['kind'] == 'invoke':
            vols = {r.get('volume')}
            if r['verb'] == 'restore':
                vols.add((r.get('args') or {}).get('source'))
            vols.discard(None)
            for v in vols:
                if open_on.get(v):
                    found.add(v)
                open_on.setdefault(v, set()).add(r['seq'])
        elif r['kind'] == 'complete':
            for s in open_on.values():
                s.discard(r['of'])
    return found


def unknown_on_overlap(fn):
    def wrapped(recs):
        u = unknown_environment(recs)
        if u is not None:
            return {'verdict': 'UNKNOWN', 'findings': ['the environment is not known from seq %d (a fault operation without a known outcome): this checker does not judge' % u]}
        o = overlaps(recs)
        if o:
            return {'verdict': 'UNKNOWN', 'findings': ['operations overlap on %s: this checker judges only one-at-a-time histories' % ', '.join(sorted(o))]}
        return fn(recs)
    wrapped.__name__, wrapped.__doc__ = fn.__name__, fn.__doc__
    return wrapped


def replay(recs):
    """Walk the history once and hand each event to the checkers that need it.

    Writes and reads are applied at their completion (M0 issues them one at a
    time); a snapshot freezes, per block, the values allowed at its invoke
    plus those of any write that completed during it.
    """
    sh = Shadow()
    events = []
    inv_of = {r['seq']: r for r in recs if r['kind'] == 'invoke'}
    snap_open = {}       # invocation seq -> (volume, {lba: set}) of an open snapshot
    frozen = {}          # (volume, snapshot) -> {lba: set}, the latest of that name
    images = {}          # restore completion seq -> the snapshot image it restored (None: none existed)
    for r in recs:
        if r['kind'] == 'invoke' and r['verb'] == 'snapshot':
            vol = r['volume']
            snap_open[r['seq']] = (vol, {lba: sh.may_hold(vol, lba) for lba in sh.lbas(vol)})
            continue
        if r['kind'] != 'complete':
            if r['kind'] == 'observe':
                events.append(('observe', r, None))
            continue
        inv = inv_of[r['of']]
        if inv['verb'] in FAULT_VERBS:   # an operation on a VM, not on a volume
            events.append(('fault', inv, r))
            continue
        vol, verb, out = inv['volume'], inv['verb'], r['outcome']
        args, res = inv.get('args') or {}, r.get('result') or {}
        if verb == 'write':
            lba = args['lba']
            # A write completed while a snapshot of ITS volume was open may be
            # in that snapshot; a refused write changed nothing.
            if out in ('ok', 'unknown'):
                for svol, s in snap_open.values():
                    if svol == vol:
                        s.setdefault(lba, sh.may_hold(vol, lba))
                        s[lba] = s[lba] | {args['sha256']}
            sh.write(vol, lba, args['sha256'], out)
            events.append(('write', inv, r))
        elif verb == 'read':
            if out == 'ok':
                events.append(('read', inv, r, sh.may_hold(vol, args['lba'])))
        elif verb == 'snapshot':
            _, taken = snap_open.pop(r['of'])
            if out == 'ok':
                frozen[(vol, args['name'])] = taken
            events.append(('snapshot', inv, r))
        elif verb == 'restore':
            if out == 'ok':
                # The image is the snapshot AS IT STOOD when the restore completed:
                # a snapshot of that name taken later is not this restore's.
                snap = frozen.get((args['source'], args['snapshot']))
                images[r['seq']] = None if snap is None else {k: set(v) for k, v in snap.items()}
                if snap is not None:
                    sh.base[vol] = images[r['seq']]
            events.append(('restore', inv, r))
        else:
            events.append((verb, inv, r))
    sh.frozen = frozen   # every snapshot as last taken, by (volume, name)
    return sh, events, images


@unknown_on_overlap
def c1(recs):
    sh, events, _ = replay(recs)
    f = []
    last_write = {}   # (vol, lba) -> index of the last write event
    last_read = {}    # (vol, lba) -> index of the last ok read event
    for i, ev in enumerate(events):
        kind, inv = ev[0], ev[1]
        if kind == 'write' and ev[2]['outcome'] in ('ok', 'unknown'):
            last_write[(inv['volume'], inv['args']['lba'])] = i
        elif kind == 'read':
            key = (inv['volume'], inv['args']['lba'])
            got = (ev[2].get('result') or {}).get('sha256')
            if got not in ev[3]:
                f.append('read %s lba %d seq %d returned %s, not one of %s' % (key[0], key[1], inv['seq'], got, sorted(ev[3])))
            last_read[key] = i
    # Every written block is read after its last write: a volume deleted at
    # the end must be read before its delete, so a delete excuses nothing.
    if not last_write:
        f.append('no write in the history: C1 judges nothing')
    # No read with writes present already fails below (written, never read).
    unread = sorted(k for k, w in last_write.items() if last_read.get(k, -1) < w)
    if unread:
        f.append('written, never read afterwards: %d (%s)' % (len(unread), ', '.join('%s lba %d' % k for k in unread[:8])))
    return verdict(f)


def restores(recs):
    """(target, source, snapshot, restore completion seq) of each ok restore."""
    out = []
    for inv, comp in operations(recs):
        if inv['verb'] == 'restore' and comp and comp['outcome'] == 'ok':
            a = inv.get('args') or {}
            out.append((inv['volume'], a.get('source'), a.get('snapshot'), comp['seq']))
    return out


@unknown_on_overlap
def c2(recs):
    _, events, images = replay(recs)
    f = []
    if not restores(recs):
        return verdict(['no restore in the history: C2 judges nothing'])
    for target, source, snap, at in restores(recs):
        image = images.get(at)
        if image is None:
            f.append('restore of %s from %s/%s: no snapshot of that name had completed before the restore' % (target, source, snap))
            continue
        # The restored content of a block is seen only by a read of the
        # target with no earlier write of the target at that block; a block
        # written first is not checked here (C1 covers it).
        first, written = {}, set()
        for ev in events:
            if ev[1].get('volume') != target or ev[0] not in ('read', 'write') or ev[2]['seq'] < at:
                continue
            lba = ev[1]['args']['lba']
            if ev[0] == 'write' and ev[2]['outcome'] in ('ok', 'unknown'):
                written.add(lba)
            elif ev[0] == 'read' and lba not in written:
                first.setdefault(lba, ev)
        if not first:
            f.append('%s: no restored block was read (each was written first, or none was read): C2 judges nothing' % target)
            continue
        for lba, ev in sorted(first.items()):
            got = (ev[2].get('result') or {}).get('sha256')
            allowed = image.get(lba, {ZERO})
            if got not in allowed:
                f.append('%s lba %d: first read %s, not a content of %s/%s (%s)' % (target, lba, got, source, snap, sorted(allowed)))
    return verdict(f)


@unknown_on_overlap
def c3(recs):
    """Independence after a restore, both ways. A direction is exercised only
    by the LATEST write on one side at a block being acknowledged (ok) and of
    a content the other side may not already hold there, followed by an ok read
    of the SAME block on the other side, with no write by the reading side to
    that block after it (the reader's own write masks it); an overwritten value
    counts for nothing. A write that cannot tell propagation from
    isolation (the other side may hold the same content anyway), an unknown or
    refused write, a read before the write, or a read of another block
    exercises nothing. A direction not exercised is a FAIL: C3 judged nothing."""
    _, events, images = replay(recs)
    f = []
    if not restores(recs):
        return verdict(['no restore in the history: C3 judges nothing'])
    for target, source, snap, at in restores(recs):
        image = images.get(at) or {}
        written = {source: {}, target: {}}   # volume -> lba -> {sha: completion seq} after the restore (ok or unknown)
        latest = {source: {}, target: {}}    # volume -> lba -> (value if acknowledged else None, completion seq) of its LATEST write
        exercised = {source: False, target: False}   # keyed by the WRITTEN side
        for ev in events:
            if ev[0] not in ('read', 'write') or ev[2]['seq'] <= at or ev[1].get('volume') not in written:
                continue
            vol, lba = ev[1]['volume'], ev[1]['args']['lba']
            other = target if vol == source else source
            if ev[0] == 'write' and ev[2]['outcome'] in ('ok', 'unknown'):
                written[vol].setdefault(lba, {})[ev[1]['args']['sha256']] = ev[2]['seq']
                # Only the latest write counts: an earlier value was overwritten;
                # an unknown latest write leaves the content ambiguous.
                latest[vol][lba] = (ev[1]['args']['sha256'] if ev[2]['outcome'] == 'ok' else None, ev[2]['seq'])
            elif ev[0] == 'read':
                got = (ev[2].get('result') or {}).get('sha256')
                # ev[3]: what the reading volume may hold here by its own history.
                # The witness is UNMASKED only if it completed after the reader's own
                # last write at this block: a later own write overwrites anything
                # the witness could have propagated.
                w_sha, w_seq = latest[other].get(lba, (None, 0))
                own_seq = latest[vol].get(lba, (None, 0))[1]
                if w_sha is not None and w_seq > own_seq and w_sha not in ev[3]:
                    exercised[other] = True   # acknowledged, unmasked and distinguishable here
                foreign = set(written[other].get(lba, {})) - (image.get(lba, set()) if vol == target else set())
                if got in foreign:
                    if vol == target:
                        f.append('%s lba %d seq %d holds a write to %s made after the snapshot' % (target, lba, ev[1]['seq'], source))
                    else:
                        f.append('%s lba %d seq %d holds a write to the target %s' % (source, lba, ev[1]['seq'], target))
        # The directions are told apart by content: a hash written to both
        # volumes at the same block makes them indistinguishable.
        shared = sorted('lba %d' % lba for lba in set(written[source]) & set(written[target]) if set(written[source][lba]) & set(written[target][lba]))
        if shared:
            return {'verdict': 'UNKNOWN', 'findings': f + ['%s and %s were written the same content at %s: the directions cannot be told apart' % (source, target, ', '.join(shared[:4]))]}
        if not exercised[source]:
            f.append('%s -> %s not exercised: no write to the source read back on the target at the same block' % (source, target))
        if not exercised[target]:
            f.append('%s -> %s not exercised: no write to the target read back on the source at the same block' % (target, source))
    return verdict(f)

def c7(recs):
    f = []
    deleted = {}    # volume -> completion seq of its ok delete
    creates = []    # (volume, invocation seq, completion) of every create
    for inv, comp in operations(recs):
        if inv['verb'] == 'delete' and comp and comp['outcome'] == 'ok':
            deleted[inv['volume']] = comp['seq']
        elif inv['verb'] == 'create':
            creates.append((inv['volume'], inv['seq'], comp))
    if not deleted:
        return verdict(['no delete with outcome ok in the history: C7 judges nothing'])
    seen = set()
    for r in recs:
        vol = r.get('volume')
        if r['kind'] != 'observe' or vol not in deleted or vol in seen or r['seq'] < deleted[vol]:
            continue
        seen.add(vol)
        res = r.get('result') or {}
        head = res.get('head')
        if not isinstance(head, dict) or head.get('state') != 'deleted':
            f.append('%s after delete: ops chunk show answered %r, not HEAD in state deleted' % (vol, head or res.get('show_error')))
        files = res.get('files')
        if files is None:
            f.append('%s after delete: no listing (%s)' % (vol, res.get('listing_error', 'none recorded')))
            continue
        extra = sorted(set(files) - TOMBSTONE)
        if extra:
            f.append('%s after delete: objects left: %s' % (vol, ', '.join(extra[:8])))
        missing = sorted(TOMBSTONE - set(files))
        if missing:
            f.append('%s after delete: %s missing from its directory (the tombstone keeps the name out of it)' % (vol, ', '.join(missing)))
    # In v1 the master keeps a deleted name's reservation: a create of that
    # name after its ok delete is REFUSED (FailedPrecondition). If a build lets
    # it through, this goes red so that someone decides it.
    recreated = set()
    for vol, at, comp in creates:
        if vol not in deleted or at < deleted[vol]:
            continue
        recreated.add(vol)
        detail = (comp.get('result') or {}).get('detail', '') if comp else ''
        if comp is None or comp['outcome'] != 'refused' or 'FailedPrecondition' not in detail or RESERVED_NAME not in detail:
            f.append('%s created again after its delete: %s, not refused for the name''s reservation (FailedPrecondition, %r): %s'
                     % (vol, comp['outcome'] if comp else 'no completion', RESERVED_NAME, detail or (comp.get('result') if comp else '')))
    if not recreated:
        f.append('no create of a deleted name was attempted: the reservation rule was not exercised')
    never = sorted(set(deleted) - seen)
    if never:
        f.append('deleted but never observed afterwards: %s' % ', '.join(never))
    return verdict(f)


def touches(inv, vol):
    """Whether an operation may change volume `vol` as far as the history
    tells: its own volume, a restore's source, and every volume for a fault
    operation (a VM fault is not mapped to volumes)."""
    if inv['verb'] in FAULT_VERBS:
        return True
    return vol in (inv.get('volume'), (inv.get('args') or {}).get('source') if inv['verb'] == 'restore' else None)


def observed_state(obs):
    """(HEAD generation, sorted file listing) of an observation, or the reason
    it records no comparable state."""
    res = obs.get('result') or {}
    head, files = res.get('head'), res.get('files')
    if not isinstance(head, dict) or not isinstance(head.get('gen'), int) or isinstance(head.get('gen'), bool):
        return None, 'no HEAD generation (%r)' % (head if head is not None else res.get('show_error'))
    if not isinstance(files, list):
        return None, 'no file listing (%s)' % res.get('listing_error', 'none recorded')
    return (head['gen'], sorted(files)), None


@unknown_on_overlap
def c5(recs):
    """A refused operation changed nothing. Judged only when the operations
    never overlap (unknown_on_overlap): an operation open on the volume during
    the refusal could move HEAD between the two observations, and the
    observations could no longer be laid to the refusal alone."""
    ops_ = operations(recs)
    refusals = [(inv, comp) for inv, comp in ops_ if comp is not None and comp['outcome'] == 'refused']
    if not refusals:
        return {'verdict': 'FAIL', 'findings': ['no refused operation in the history: C5 not exercised']}
    inv_of = {r['seq']: r for r in recs if r['kind'] == 'invoke'}   # a completion carries no args
    f, notes, judged = [], [], 0
    for inv, comp in refusals:
        vol = inv.get('volume')
        name = 'seq %d %s %s' % (inv['seq'], inv['verb'], vol or '(no volume)')
        if not vol:
            notes.append('not judged: %s: an operation on no volume' % name)
            continue
        before = after = None
        for r in recs:   # the latest observation before the invoke; an ok or unknown completion on the volume voids it
            if r['seq'] >= inv['seq']:
                break
            if r['kind'] == 'observe' and r.get('verb') == 'observe' and r.get('volume') == vol:
                before = r
            elif before is not None and r['kind'] == 'complete' and r['outcome'] in ('ok', 'unknown') and touches(inv_of[r['of']], vol):
                before = None
        for r in recs:   # the first observation after the complete, unless an operation on the volume is invoked first
            if r['seq'] <= comp['seq']:
                continue
            if r['kind'] == 'invoke' and touches(r, vol):
                break
            if r['kind'] == 'observe' and r.get('verb') == 'observe' and r.get('volume') == vol:
                after = r
                break
        if before is None or after is None:
            notes.append('not judged: %s: no valid observation %s it' % (name, 'before' if before is None else 'after'))
            continue
        (b, bwhy), (a, awhy) = observed_state(before), observed_state(after)
        if b is None or a is None:
            notes.append('not judged: %s: observation seq %d records %s' % (name, (before if b is None else after)['seq'], bwhy or awhy))
            continue
        judged += 1
        if b[0] != a[0]:
            f.append('%s refused but HEAD generation moved %d -> %d (observations seq %d, %d)' % (name, b[0], a[0], before['seq'], after['seq']))
        if b[1] != a[1]:
            gained, lost = sorted(set(a[1]) - set(b[1])), sorted(set(b[1]) - set(a[1]))
            f.append('%s refused but its file listing changed: gained %s, lost %s (observations seq %d, %d)'
                     % (name, ', '.join(gained[:8]) or 'none', ', '.join(lost[:8]) or 'none', before['seq'], after['seq']))
    if f:
        return {'verdict': 'FAIL', 'findings': f + notes}
    if not judged:
        return {'verdict': 'UNKNOWN', 'findings': notes + ['%d refusal(s), none judged' % len(refusals)]}
    return {'verdict': 'PASS', 'findings': notes}


# C6 latency bounds: verb -> (seconds, the operator page and line stating it).
# Only a bound a page states goes here, never an invented number. The pages
# state recovery bounds of a restarted holder (the re-grant barrier and the
# start wait, docs/volume-lifecycle.md "After a crash") but no bound on the
# duration of any operation, so the table is empty and every verb is listed
# as not judged.
BOUNDS = {}
RUNNER_VERBS = FAULT_VERBS + ('kill', 'restart')   # the environment's operations, not the product's
PLACED_BY_OBSERVATION = ('write', 'snapshot', 'delete', 'move', 'restore')
LIVE = {'HEAD', 'VOLUME'}   # a live volume's directory holds at least these


def healthy_points(recs):
    """Seqs of the healthy observations with healthy true and no operation in flight."""
    open_, out = set(), []
    for r in recs:
        if r['kind'] == 'invoke':
            open_.add(r['seq'])
        elif r['kind'] == 'complete':
            open_.discard(r['of'])
        elif r['kind'] == 'observe' and r.get('verb') == 'healthy' and (r.get('result') or {}).get('healthy') is True and not open_:
            out.append(r['seq'])
    return out


def _volume_changed(recs, vol, lo, hi, lba=None):
    """The first operation on `vol` invoked in (lo, hi) that may change what a
    placement reads: any verb but read, and for a block (lba given) a write to
    that block, a restore onto the volume or a delete. A refused one changed
    nothing."""
    for inv, comp in operations(recs):
        if not lo < inv['seq'] < hi or inv.get('volume') != vol or inv['verb'] == 'read':
            continue
        if comp is not None and comp['outcome'] == 'refused':
            continue
        if lba is not None and not ((inv['verb'] == 'write' and (inv.get('args') or {}).get('lba') == lba) or inv['verb'] in ('restore', 'delete')):
            continue
        if lba is None and inv['verb'] == 'write':
            continue   # a write moves no root, holder or state
        return inv
    return None


def _place_write(recs, inv, done, after):
    vol, lba, sha = inv['volume'], inv['args']['lba'], inv['args']['sha256']
    prior = replay([r for r in recs if r['seq'] < inv['seq']])[0].may_hold(vol, lba)
    for i2, c2 in operations(recs):
        if i2['seq'] <= done or i2.get('volume') != vol:
            continue
        if i2['verb'] == 'read' and i2['args']['lba'] == lba and i2['seq'] > after and c2 is not None and c2['outcome'] == 'ok':
            ch = _volume_changed(recs, vol, done, i2['seq'], lba)
            if ch is not None:
                return None, 'seq %d %s changed the block before the read seq %d' % (ch['seq'], ch['verb'], i2['seq'])
            got = (c2.get('result') or {}).get('sha256')
            if got == sha:
                return 'took effect', 'read seq %d' % i2['seq']
            if got in prior:
                return 'did not', 'read seq %d' % i2['seq']
            return 'third', 'read seq %d returned %s: neither its value %s nor a prior one %s' % (i2['seq'], got, sha, sorted(prior))
    return None, 'no ok read of %s lba %d after seq %d' % (vol, lba, after)


def _observation(recs, vol, after):
    for r in recs:
        if r['seq'] > after and r['kind'] == 'observe' and r.get('verb') == 'observe' and r.get('volume') == vol:
            return r
    return None


def _baseline(recs, vol, before):
    """The latest observation of `vol` before seq `before` with nothing that may
    change it in between (C5's rule, writes aside)."""
    best = None
    for r in recs:
        if r['seq'] >= before:
            break
        if r['kind'] == 'observe' and r.get('verb') == 'observe' and r.get('volume') == vol:
            best = r
    if best is None or _volume_changed(recs, vol, best['seq'], before) is not None:
        return None
    return best


def _place_by_status(inv, obs, base):
    """Placement of a snapshot, delete, move or restore from the observation
    after it (and, for a snapshot, the one before it)."""
    verb, args = inv['verb'], inv.get('args') or {}
    res = obs.get('result') or {}
    head, files = res.get('head'), res.get('files')
    err = (res.get('show_error') or {}).get('code') if isinstance(res.get('show_error'), dict) else None
    if verb == 'snapshot':
        bfiles = (base.get('result') or {}).get('files')
        if not isinstance(files, list) or not isinstance(bfiles, list):
            return None, 'no listing before and after (observations seq %d, %d)' % (base['seq'], obs['seq'])
        before = {f for f in bfiles if f.startswith('root/')}
        now = {f for f in files if f.startswith('root/')}
        if before - now:
            return 'third', 'root objects lost: %s' % ', '.join(sorted(before - now)[:4])
        return ('took effect' if now - before else 'did not'), 'roots %d -> %d' % (len(before), len(now))
    if verb == 'restore':
        if isinstance(head, dict):
            if not isinstance(files, list):
                return None, 'no listing (%s)' % res.get('listing_error', 'none recorded')
            missing = sorted(LIVE - set(files))
            return ('third', 'HEAD answers but %s missing' % ', '.join(missing)) if missing else ('took effect', 'HEAD and VOLUME present')
        if err == 'volume_not_found':
            if res.get('listing_error'):
                return None, 'no listing (%s)' % res['listing_error']
            return ('third', 'no HEAD but objects present: %s' % ', '.join(sorted(files)[:4])) if files else ('did not', 'no such volume')
        return None, 'no status (%r)' % (head if head is not None else res.get('show_error'))
    if not isinstance(head, dict):
        if verb == 'delete' and err == 'volume_not_found':
            return 'third', 'the volume is absent instead of its tombstone'
        return None, 'no status (%r)' % (head if head is not None else res.get('show_error'))
    if not isinstance(files, list):
        return None, 'no listing (%s)' % res.get('listing_error', 'none recorded')
    state = head.get('state')
    if verb == 'delete':
        if state == 'deleted':
            if set(files) == TOMBSTONE:
                return 'took effect', 'HEAD deleted, tombstone only'
            return 'third', 'HEAD deleted but the directory holds %s' % ', '.join(sorted(set(files) ^ TOMBSTONE)[:4])
        if state in ('deleting', 'purging'):
            return 'third', 'HEAD in state %s' % state
    if LIVE - set(files):
        return 'third', 'HEAD in state %r but %s missing' % (state, ', '.join(sorted(LIVE - set(files))))
    if verb == 'delete':
        return 'did not', 'HEAD in state %r' % state
    # move: the holder's server and the epoch tell the two outcomes apart
    server, epoch = str(head.get('holder', '')).split('/')[0], head.get('epoch')
    if server == args.get('to') and isinstance(epoch, int) and epoch > args.get('epoch'):
        return 'took effect', 'holder %s at epoch %d' % (head.get('holder'), epoch)
    if server == args.get('from') and epoch == args.get('epoch'):
        return 'did not', 'holder %s at epoch %d' % (head.get('holder'), epoch)
    return 'third', 'holder %r at epoch %r: neither %s at a new epoch nor %s at %r' % (head.get('holder'), epoch, args.get('to'), args.get('from'), args.get('epoch'))


@unknown_on_overlap
def c6(recs):
    """Bounds and "none stays unknown" (see the module docstring)."""
    f, notes, unruled = [], [], []
    ops_ = operations(recs)
    healthy = [r for r in recs if r['kind'] == 'observe' and r.get('verb') == 'healthy']
    unknowns = [(inv, comp) for inv, comp in ops_ if inv['verb'] not in RUNNER_VERBS and inv['verb'] != 'read'
                and (comp is None or comp['outcome'] == 'unknown')]
    if not unknowns and not healthy:
        return {'verdict': 'FAIL', 'findings': ['no unknown outcome and no healthy point in the history: C6 not exercised']}
    # Latency, only against a stated bound and never under a fault.
    down = []   # (kill invoke seq, seq of the first healthy point after it or None)
    points = healthy_points(recs)
    for inv, comp in ops_:
        if inv['verb'] == 'kill':
            down.append((inv['seq'], next((p for p in points if p > inv['seq']), None)))
    unbounded = set()
    for inv, comp in ops_:
        if inv['verb'] in RUNNER_VERBS or comp is None or comp['outcome'] == 'unknown':
            continue
        if inv['verb'] not in BOUNDS:
            unbounded.add(inv['verb'])
            continue
        if any(k < comp['seq'] and (p is None or p > inv['seq']) for k, p in down):
            notes.append('latency not judged: seq %d %s ran while a process was down (no per-fault bound)' % (inv['seq'], inv['verb']))
            continue
        secs, where = BOUNDS[inv['verb']]
        took = (comp['mono_ns'] - inv['mono_ns']) / 1e9
        if took > secs:
            f.append('seq %d %s %s took %.3f s, above the %g s bound (%s)' % (inv['seq'], inv['verb'], inv.get('volume', ''), took, secs, where))
    if unbounded:
        notes.append('no stated bound: %s (latency not judged)' % ', '.join(sorted(unbounded)))
    # Health: every kill is followed by a healthy point; the run ends healthy.
    for k, p in down:
        if p is None:
            f.append('kill seq %d: no healthy point after it (never shown healthy again)' % k)
    if healthy and (healthy[-1].get('result') or {}).get('healthy') is not True:
        res = healthy[-1].get('result') or {}
        f.append('the run ended unhealthy: last healthy record seq %d: down %s, not serving %s' % (healthy[-1]['seq'], res.get('down'), res.get('not_serving')))
    # None stays unknown.
    for inv, comp in unknowns:
        name = 'seq %d %s %s' % (inv['seq'], inv['verb'], inv.get('volume', ''))
        if inv['verb'] not in PLACED_BY_OBSERVATION:
            unruled.append(name)
            continue
        done = inv['seq'] if comp is None else comp['seq']
        due = next((p for p in points if p > done), None)
        after = done if due is None else due
        at = 'the end' if due is None else 'healthy point seq %d' % due
        if inv['verb'] == 'write':
            how, why = _place_write(recs, inv, done, after)
        else:
            obs = _observation(recs, inv['volume'], after)
            base = _baseline(recs, inv['volume'], inv['seq']) if inv['verb'] == 'snapshot' else None
            if obs is None:
                how, why = None, 'no observation of %s after %s' % (inv['volume'], at)
            elif inv['verb'] == 'snapshot' and base is None:
                how, why = None, 'no clean observation of %s before it' % inv['volume']
            elif _volume_changed(recs, inv['volume'], done, obs['seq']) is not None:
                ch = _volume_changed(recs, inv['volume'], done, obs['seq'])
                how, why = None, 'seq %d %s changed %s before the observation seq %d' % (ch['seq'], ch['verb'], inv['volume'], obs['seq'])
            else:
                how, why = _place_by_status(inv, obs, base)
        if how is None:
            f.append('unknown not resolved: %s (due at %s): %s' % (name, at, why))
        elif how == 'third':
            f.append('unknown in a third state: %s (due at %s): %s' % (name, at, why))
    if f:
        return {'verdict': 'FAIL', 'findings': f + notes}
    if unruled:
        return {'verdict': 'UNKNOWN', 'findings': ['no resolution rule for an unknown: %s' % ', '.join(unruled)] + notes}
    return {'verdict': 'PASS', 'findings': notes}


CHECKERS = {'C0': c0, 'C1': c1, 'C2': c2, 'C3': c3, 'C5': c5, 'C6': c6, 'C7': c7}


def check(lines):
    try:
        recs = read_history(lines)
    except Malformed as e:
        return {name: {'verdict': 'UNKNOWN', 'findings': ['history unreadable: %s' % e]} for name in CHECKERS}
    # Positive evidence first: an empty history, or one that does not end with
    # the writer's end record (a crashed runner, a truncated copy), is not a
    # run; every checker fails it rather than pass on what it never saw.
    if not recs:
        return {name: {'verdict': 'FAIL', 'findings': ['empty history: no evidence']} for name in CHECKERS}
    if recs[-1]['kind'] != 'end':
        return {name: {'verdict': 'FAIL', 'findings': ['the history has no end record: incomplete evidence (seq %d is the last)' % recs[-1]['seq']]} for name in CHECKERS}
    out = {}
    for name, fn in CHECKERS.items():
        try:
            out[name] = fn(recs)
        except (KeyError, TypeError) as e:
            out[name] = {'verdict': 'UNKNOWN', 'findings': ['record without a field the checker needs: %r' % (e,)]}
    return out


def main(argv):
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument('history')
    p.add_argument('--json')
    a = p.parse_args(argv)
    with open(a.history, encoding='utf-8') as fh:
        result = check(fh.read().splitlines())
    text = json.dumps(result, indent=1, sort_keys=True)
    if a.json:
        with open(a.json, 'w', encoding='utf-8') as fh:
            fh.write(text + '\n')
    for name, v in sorted(result.items()):
        print('%s %s%s' % (name, v['verdict'], (': ' + '; '.join(v['findings'][:3])) if v['findings'] else ''))
    return 0 if all(v['verdict'] == 'PASS' for v in result.values()) else 1


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
