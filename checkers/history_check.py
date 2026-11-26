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
  DUR durability with flush: a write is durable once acknowledged with its
      fsync (a write record without "flush": false), or once a later flush
      of its volume is acknowledged; a flush with outcome unknown makes
      nothing durable. After a process kill (verb kill, not refused) a block
      may read only its last durable value or the value of a write invoked
      after it up to the kill; anything else FAILS (an older value, content
      no write produced, ZERO when a durable write exists). A lost
      acknowledged unflushed write is a PASS, counted ("acknowledged, not
      flushed, lost: n"). Without a kill, C1's rule judged the same way.
      No kill and no unflushed write: FAIL (not exercised)

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


def flushed(args):
    """A write record without a flush key, or with "flush": true, was issued
    with the fsync; "flush": false was not. Anything else is not a record the
    runner writes."""
    f = args.get('flush', True)
    if f is not True and f is not False:
        raise TypeError('write flush %r' % (f,))
    return f


def killed(inv, comp):
    """A process kill counts unless it was refused (an unknown kill may have
    killed)."""
    return inv['verb'] == 'kill' and (comp is None or comp['outcome'] != 'refused')


@unknown_on_overlap
def dur(recs):
    """Durability with flush. Per volume and block:
      durable   the values the last durable write may have left: a flushed
                write acknowledged (ok), or the writes since the previous
                durable state once a flush of the same volume is acknowledged
                (from the last acknowledged write on; with none acknowledged,
                added to the previous durable values). A flush with outcome
                unknown or refused makes nothing durable.
      volatile  every write completed (ok or unknown) since the durable state.
      may hold  what a read may return, C1's shadow: an ok write its value
                only, an unknown write adds its value for good.
    A kill (verb kill, outcome not refused, applied at its invoke; it is not
    mapped to volumes, so it applies to every volume) sets each block's may
    hold, and its durable values, to durable | volatile: the last durable
    value or any write invoked after it up to the kill. A read outside its
    block's may hold FAILS; after a kill the finding says which: ZERO although
    a durable write exists, content no write produced, or an older value than
    the last durable one. On the first read of a block after a kill, every
    acknowledged unflushed write after the value read is counted lost (a PASS,
    reported). Without a kill this is C1's rule, judged the same way here: and
    every written block is read after its last write. No kill and no
    unflushed write: FAIL, not exercised."""
    ops_ = operations(recs)
    comp_of = {inv['seq']: comp for inv, comp in ops_}
    nkills = sum(1 for inv, comp in ops_ if killed(inv, comp))
    nunflushed = sum(1 for inv, _ in ops_ if inv['verb'] == 'write' and not flushed(inv.get('args') or {}))
    if not nkills and not nunflushed:
        return verdict(['no kill and no unflushed write in the history: DUR not exercised'])
    _, _, images = replay(recs)
    base = {}    # volume -> {lba: set}, a restore target's initial content
    st = {}      # (volume, lba) -> block state
    inv_of = {}
    f, lost, unread_after_kill, judged_after_kill = [], 0, 0, 0
    last_write, last_read = {}, {}

    def block(vol, lba):
        b = st.get((vol, lba))
        if b is None:
            init = set(base.get(vol, {}).get(lba, {ZERO}))
            b = st[(vol, lba)] = {'durable': init, 'volatile': [], 'cur': set(init), 'pending': set(),
                                  'produced': set(init), 'kill': None}
        return b

    def drop_kill(b):
        # A block whose kill was never resolved by a read: its acknowledged
        # unflushed writes are neither lost nor kept as far as the history shows.
        nonlocal unread_after_kill
        if b['kill'] is not None and any(ack for _, ack in b['kill'][2]):
            unread_after_kill += 1
        b['kill'] = None

    for i, r in enumerate(recs):
        if r['kind'] == 'invoke':
            inv_of[r['seq']] = r
            if killed(r, comp_of.get(r['seq'])):
                for b in st.values():
                    drop_kill(b)
                    allowed = b['durable'] | {s for s, _ in b['volatile']}
                    b['kill'] = (r['seq'], set(b['durable']), list(b['volatile']))
                    b['cur'], b['pending'], b['durable'], b['volatile'] = set(allowed), set(), set(allowed), []
            continue
        if r['kind'] != 'complete':
            continue
        inv = inv_of[r['of']]
        verb, out, vol, args = inv['verb'], r['outcome'], inv.get('volume'), inv.get('args') or {}
        if verb == 'write' and out in ('ok', 'unknown'):
            sha, fl = args['sha256'], flushed(args)
            b = block(vol, args['lba'])
            drop_kill(b)
            b['produced'].add(sha)
            if out == 'ok':
                b['cur'] = {sha}
            else:
                b['cur'].add(sha)
                b['pending'].add(sha)
            if fl and out == 'ok':
                b['durable'], b['volatile'] = {sha}, []
            else:
                b['volatile'].append((sha, out == 'ok'))   # ok here is acknowledged and NOT flushed
            last_write[(vol, args['lba'])] = i
        elif verb == 'flush' and out == 'ok':
            for (v, _), b in st.items():
                if v != vol or not b['volatile']:
                    continue
                vals = [s for s, _ in b['volatile']]
                acked = [k for k, (_, ack) in enumerate(b['volatile']) if ack]
                b['durable'] = set(vals[acked[-1]:]) if acked else b['durable'] | set(vals)
                b['volatile'] = []
        elif verb == 'restore' and out == 'ok':
            base[vol] = images.get(r['seq']) or {}
            for key in [k for k in st if k[0] == vol]:
                del st[key]
        elif verb == 'read' and out == 'ok':
            lba = args['lba']
            b = block(vol, lba)
            got = (r.get('result') or {}).get('sha256')
            allowed = b['cur'] | b['pending']
            k = b['kill']
            if got not in allowed:
                if k is None:
                    f.append('read %s lba %d seq %d returned %s, not one of %s' % (vol, lba, inv['seq'], got, sorted(allowed)))
                else:
                    if got == ZERO:
                        why = 'ZERO although a durable write exists'
                    elif got not in b['produced']:
                        why = 'content no write produced'
                    else:
                        why = 'an older value than the last durable one'
                    f.append('after the kill at seq %d, read %s lba %d seq %d returned %s: %s (allowed %s)' % (k[0], vol, lba, inv['seq'], got, why, sorted(allowed)))
            elif k is not None:
                vlist = k[2]
                at = max((n for n, (s, _) in enumerate(vlist) if s == got), default=-1)
                lost += sum(1 for _, ack in vlist[at + 1:] if ack)
            if k is not None:
                judged_after_kill += 1
                b['kill'] = None
            last_read[(vol, lba)] = i
    for b in st.values():
        drop_kill(b)
    if nkills:
        if not judged_after_kill:
            f.append('%d kill(s), no block read after any of them: the durability rule judged nothing' % nkills)
    else:
        # C1's rule without a kill: every written block is read after its last write.
        unread = sorted(key for key, w in last_write.items() if last_read.get(key, -1) < w)
        if unread:
            f.append('written, never read afterwards: %d (%s)' % (len(unread), ', '.join('%s lba %d' % key for key in unread[:8])))
    notes = ['acknowledged, not flushed, lost: %d' % lost]
    if unread_after_kill:
        notes.append('acknowledged, not flushed, not read after the kill: %d block(s)' % unread_after_kill)
    return {'verdict': 'FAIL' if f else 'PASS', 'findings': f + notes}


CHECKERS = {'C0': c0, 'C1': c1, 'C2': c2, 'C3': c3, 'C5': c5, 'C7': c7, 'DUR': dur}


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
