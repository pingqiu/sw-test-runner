"""Checkers of a run's history file (runner spec S4), judging the history alone.

python3 -I history_check.py HISTORY.jsonl [--json OUT] [--product DIR]

The checkers are generic: product facts (bounds, recovery terms, fault
targets, observation parts) come from DIR/checks.json, and the product's
reading code (observation parts, placement of an unknown, the shape a delete
leaves, a kill's scope) from the reader module it names. DIR defaults to the
block product (packs/v3block/product); the rules below name block's facts as
the example.

A rerun is never folded: each attempt is its own invoke and complete, its
args carrying the request and "attempt": n, and a request's outcome is its
LAST attempt's (superseded()). C0, C5, C6 and C7 judge a request by its last
attempt; an earlier refused attempt counts under no C5 limit (it is still
judged by the whole-state rule when a clean observation pair allows).

Each checker returns PASS, FAIL or UNKNOWN with its findings, and "judged":
the number of items it actually judged (C0 operations with a known outcome,
C1 reads compared, C2 restored blocks compared, C3 cross reads compared, C5
refusals judged, C6 unknowns resolved plus recoveries timed, C7 deleted
volumes observed, DUR reads judged after a kill (without a holder kill, the
reads judged by C1's rule), MO outages judged); --json carries it. A PASS
with judged 0 cannot happen: a checker that judged nothing says "not
exercised", and check() fails any verdict that would. A history that
does not parse, or breaks the writer's own rules, is UNKNOWN for every
checker: nothing is judged from a file that cannot be read whole. The exit
code is 0 only when every checker passes.

  C0  every invoke has its complete; no outcome is unknown; a refusal
      (refused or refused_effect) only
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
      not map volumes to VMs). Compared, the whole observed state: HEAD
      (generation, epoch, holder, state) or the show error's code, HEAD's
      snapshot names (result.snapshots), the control plane's workspace answer (exit, and the stable facts of its
      text: the lease expiry and the validity at query are dropped, and a
      text not in its "key: value" shape is not comparable), the volume's
      directories (result.dirs, so a name that never existed is observed as
      having none) and its file listing (result.files). A part recorded in
      neither observation is not compared and is named in a note, except a
      required part (REQUIRED_PARTS: HEAD and the workspace answer), whose
      absence leaves the refusal not judged; a part recorded in one only, an
      unreadable HEAD, or a listing error leaves the refusal not judged.
      BASELINE: two consecutive observations of a volume with no operation
      touching it invoked between them must show the same state; the product
      works in the background (checkpoint publishing, cleaning), and a
      difference there makes C5 UNKNOWN for the run, and so does a history
      with refusals but no such pair.
      Each refusal belongs to a LIMIT: the generator's gen_step names the
      limit a negative operation probes; other refusals are "ordinary <verb>".
      The findings report judged / refusals per limit. FAIL if a judged
      refusal shows a different state; UNKNOWN if a limit that occurred has
      no judged refusal; PASS only if every limit that occurred was judged at
      least once and none changed anything; FAIL (not exercised) if there was
      no refusal at all, like the other checkers. A negative operation
      answered ok is a FAIL ("negative not refused"); one answered unknown is
      listed in a note (unknown is not ok).
      REFUSED_EFFECT: a refusal with a definite, documented change, its
      complete carrying result.effect {part, to, cite}. The effect is a
      state that stays, judged on the same two observations as a refusal:
      after the operation effect.part HAS the documented state (the product
      reader's effect_state; before it, another value or already this one,
      as for a repeated refusal), and every other compared part is
      unchanged; anything else FAILS. A condition the observation cannot
      evaluate (a stderr not recorded) leaves it NOT JUDGED, never passed.
      Baseline and limits as for refused. A
      refused_effect without its effect breaks the writer's rule: the
      history is unreadable (UNKNOWN for every checker)
  C7  after a delete with outcome ok, the volume's next observation shows HEAD
      in state deleted (the tombstone), and its directory holds exactly HEAD
      and VOLUME: any pool, seg, root or RETENTION object is a FAIL, and so is
      a missing HEAD (the tombstone keeps a later volume of the same name out
      of this directory). In v1 a create of that name afterwards is refused
      (FailedPrecondition: the master keeps the name's deletion reservation).
  DUR durability with flush: a write is durable once acknowledged with its
      fsync (a write record without "flush": false), or once a later flush
      of its volume is acknowledged; a flush with outcome unknown makes
      nothing durable. An unknown write may take effect until its holder is
      known dead. A holder kill (args.volume) reaches that volume only, a
      master kill no volume, a kill naming no volume every volume (noted).
      After a kill that reaches its volume a block may read only its last
      durable value, the value of a write invoked after it, or of an unknown
      write whose holder was alive after the durable point; anything else
      FAILS (an older value no open unknown explains, content no write
      produced, ZERO when a durable write exists). The kill settles the
      unknowns: from the first read after it the value is stable, and a
      change with no write in between FAILS. Counted on PASS: "acknowledged,
      not flushed, lost: n" and "unknown write landed after a later durable
      write: n". Without a kill, C1's rule judged the same way.
      No kill and no unflushed write: FAIL (not exercised)

  C6  bounds and "none stays unknown". Latency: an operation is judged only
      against a bound an operator page states (BOUNDS, with its page and
      line); a verb without one is NOT JUDGED and listed ("no stated bound"),
      and so is an operation open while a process was down (the table holds
      no per-fault bound). Recovery: a holder (blockvolume) restart must be
      followed by a healthy observation with healthy true by the pages'
      deadline, on the history's clock: max(the holder kill's completion +
      the re-grant 65 s, the restart's completion) + the start wait 30 s
      (RECOVERY); a blockmaster restart, for which the pages
      state no bound, and a holder restart a master fault overlaps, are NOT
      JUDGED and listed. Unknowns: a resolution point is a `healthy`
      observation with healthy true and no operation in flight, or the end
      record. Every operation whose outcome is unknown (or that never
      completed) is due at the first point after it, and must be RESOLVED by
      an observation after that point (after its completion when the point is
      the end): a write by an ok read of the same block returning its value
      (took effect) or a value the block allowed before it (did not); a
      snapshot by HEAD's snapshot names against the observation before it
      (its name present: took effect; absent: did not; another name lost: a
      third state), or, only when neither observation records the names,
      by the volume's root/ objects (a new root: took effect; the same
      roots: did not), the note saying which rule judged it; a delete, move
      or restore by the status (HEAD) and the listing. FAIL on a third state,
      on an unknown no clean observation places (none after the point, or the
      volume or block changed in between), on a run whose last healthy
      record is not healthy, and on a kill with no healthy point after it.
      A volume's intended state (product LIFECYCLE): held after an ok
      attach; not held after an ok stop, move (a hand-off: nothing serves
      the volume until the next attach) or delete; unsettled after one of
      them answered unknown. An unknown attach, stop or move (product
      NEEDS_SETTLED; a delete has its own observation rule) is resolved by
      a `settled` observation of its volume (counted once) within the
      recovery deadline max(the latest kill reaching the volume before its
      end + regrant, its end) + start_wait, or its end + regrant +
      start_wait with no kill; none in time FAILS ("unsettled"). Another
      lifecycle operation of the volume invoked first, inside the deadline,
      supersedes it (noted; the verb's own rule then places it). A verb
      with an observation rule (delete, move) is still checked by it for a
      third state. Any `third_state` entry of a healthy record FAILS.
      A read changes nothing and needs no placing; an unknown of another
      verb has no rule and makes the verdict UNKNOWN. Every judged verdict
      carries the finding "unknowns resolved: <n>" (a run with healthy
      points and n = 0 PASSes; the night requires n > 0 in some run). FAIL
      (not exercised) if the history holds neither an unknown nor a healthy
      record.
  MO  a short master outage interrupts nothing (checks.json master_outage):
      the window runs from an ok kill of the master's completion to its next
      ok restart's completion on the history's clock; one at most
      max_seconds long is judged, a longer one (or one never restarted) is
      NOT JUDGED and listed. Inside it: every write invoked is answered ok;
      every single-look healthy record (result.look "single", one per look)
      shows no volume intended to be held (or with no lifecycle record) not
      serving, the others noted; and every holder identity such a look
      shows equals the last one recorded before the kill. A judged window
      with no write or no look is UNKNOWN ("short master outage not exercised
      in window"). Reported: "short master outages judged: <n>". FAIL (not
      exercised) when the history holds no ok kill of the master.

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
import importlib.util
import json
import os
import sys

ZERO = None       # sha256 of one block of zeros, the product's block_bytes: set by load_product
KINDS = {'invoke', 'complete', 'observe', 'end'}   # a fault is an operation of module Runner, not a kind
# refused_effect: the product refused definitely and made a definite,
# documented change; its complete carries result.effect {part, to, cite},
# copied by the runner from the outcome map. It is "not ok" everywhere, and
# C5 judges its change strictly.
OUTCOMES = {'ok', 'refused', 'refused_effect', 'unknown'}
REFUSALS = ('refused', 'refused_effect')

# The product description: a directory holding checks.json (see the runner's
# packs/<product>/product/). Its facts fill the tables below, and its reader
# module (checks.json reader.module) reads observations and places operations.
# The default is the block product, so a call without --product is unchanged.
DEFAULT_PRODUCT = os.path.join(os.path.dirname(os.path.abspath(__file__)), os.pardir, 'packs', 'v3block', 'product')
CHECKS = {}        # checks.json as read
PRODUCT = None     # the product's reader module
BOUNDS = {}        # C6 latency bounds: verb -> (seconds, cite); filled in place by load_product
RECOVERY = {}      # C6 recovery terms: term -> (seconds, cite); filled in place by load_product
STATE_PARTS = ()   # the observation parts compared, in checks.json's order
REQUIRED_PARTS = ()   # of those, the parts C5 requires in both observations of a judged refusal
MASTER = None      # the fault target whose role is master
MASTER_OUTAGE = None   # MO: (max seconds, cite) of an outage the pages say interrupts nothing, or None
HOLDER_PREFIX = None   # a process whose name starts with this is a holder
PLACED_BY_OBSERVATION = ('write',)   # extended by the product's PLACED_BY_STATUS


def _load_module(name, product_dir):
    """The reader module, by file: <product_dir>/<name>.py, else the file of
    that name beside this one. By file, not by import: the checkers run under
    python -I, which puts no script directory on the path."""
    for d in (product_dir, os.path.dirname(os.path.abspath(__file__))):
        path = os.path.join(d, name + '.py')
        if os.path.isfile(path):
            spec = importlib.util.spec_from_file_location(name, path)
            mod = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(mod)
            return mod
    raise FileNotFoundError('reader module %r: not in %s nor beside the checker' % (name, product_dir))


def load_product(product_dir):
    """Read checks.json from `product_dir` and load its reader module. The
    tables BOUNDS and RECOVERY are filled in place (a test may patch them)."""
    global CHECKS, PRODUCT, STATE_PARTS, REQUIRED_PARTS, MASTER, HOLDER_PREFIX, PLACED_BY_OBSERVATION, ZERO, MASTER_OUTAGE
    with open(os.path.join(product_dir, 'checks.json'), encoding='utf-8') as fh:
        checks = json.load(fh)
    mod = _load_module(checks['reader']['module'], product_dir)
    mod.bind(checks)
    parts = tuple(checks['observation']['compared'])
    mod.observed_state({'result': {}}, parts)   # a part the reader cannot read fails here, not mid-run
    required = tuple(checks['observation'].get('required', ()))
    if set(required) - set(parts):
        raise ValueError('required parts %s are not compared parts' % ', '.join(sorted(set(required) - set(parts))))
    CHECKS, PRODUCT, STATE_PARTS, REQUIRED_PARTS = checks, mod, parts, required
    ZERO = hashlib.sha256(bytes(checks['block_bytes']['bytes'])).hexdigest()
    BOUNDS.clear()
    BOUNDS.update({r['verb']: (r['seconds'], r['cite']) for r in checks.get('bounds', [])})
    RECOVERY.clear()
    RECOVERY.update({r['term']: (r['seconds'], r['cite']) for r in checks.get('recovery', [])})
    roles = {}
    for t in checks.get('fault_targets', []):
        roles.setdefault(t.get('role'), t)
    MASTER = roles['master']['name'] if 'master' in roles else None
    HOLDER_PREFIX = roles['holder'].get('prefix') if 'holder' in roles else None
    PLACED_BY_OBSERVATION = ('write',) + tuple(mod.PLACED_BY_STATUS)
    mo_row = checks.get('master_outage')
    MASTER_OUTAGE = (mo_row['max_seconds'], mo_row['cite']) if mo_row else None


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
        # The writer omits an empty volume (a kill, a restart, a healthy
        # record names none): it is the empty name, not a missing field.
        if r['kind'] != 'end':
            r.setdefault('volume', '')
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
            if r['outcome'] == 'refused_effect':
                # The runner copies the effect from the outcome map; without it
                # the record breaks the writer's own rule, and nothing is judged.
                eff = (r.get('result') or {}).get('effect')
                if not isinstance(eff, dict) or not isinstance(eff.get('part'), str) or 'to' not in eff or not eff.get('cite'):
                    raise Malformed('line %d: refused_effect without result.effect {part, to, cite}: %r' % (n, eff))
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


def superseded(recs):
    """Invoke seqs of attempts a later attempt of the same request replaced.
    A rerun is never folded: each attempt is its own invoke and complete,
    its args carrying the request (whatever arg the action names it by) and
    "attempt": n, and the request's outcome is its LAST attempt's. Attempts
    of one request share verb, volume and every arg but "attempt", and are
    numbered on from 1; an attempt 1 (or a number that does not follow)
    starts a new request, so a later operation with the same args is its
    own. An invoke with no attempt is a request of one attempt."""
    groups, open_ = [], {}   # open_: key -> index into groups of its request so far
    for r in recs:
        if r['kind'] != 'invoke':
            continue
        args = dict(r.get('args') or {})
        n = args.pop('attempt', None)
        if n is None:
            continue
        key = (r['verb'], r.get('volume'), json.dumps(args, sort_keys=True))
        g = open_.get(key)
        if n != 1 and g is not None and groups[g][-1][0] == n - 1:
            groups[g].append((n, r['seq']))
        else:
            open_[key] = len(groups)
            groups.append([(n, r['seq'])])
    return {seq for g in groups for _, seq in g[:-1]}


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


def verdict(findings, unknown=False, judged=0):
    """A checker's verdict object. `judged` is the number of items the
    checker actually judged (its own count, machine-readable in --json)."""
    if findings:
        return {'verdict': 'FAIL', 'findings': findings, 'judged': judged}
    return {'verdict': 'UNKNOWN' if unknown else 'PASS', 'findings': [], 'judged': judged}


def c0(recs):
    """A refusal is expected only where its own invocation declares it
    (args.expect == "refused", one step of the scenario), never by verb."""
    f, judged = [], 0   # judged: operations (requests, by their last attempt) with a known outcome checked
    if not operations(recs):
        return verdict(['no operation in the history: nothing was exercised'])
    earlier = superseded(recs)
    for inv, comp in operations(recs):
        name = '%s %s seq %d' % (inv['verb'], inv.get('volume', ''), inv['seq'])
        if inv['seq'] in earlier:
            # An attempt a later attempt of its request replaced: the
            # request's outcome is the last attempt's, judged there. The
            # attempt itself must still have completed.
            if comp is None:
                f.append('no completion: ' + name)
            continue
        if comp is not None and comp['outcome'] != 'unknown':
            judged += 1
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
        elif comp['outcome'] in REFUSALS and not expected:
            f.append('refusal not expected: %s: %s' % (name, (comp.get('result') or {}).get('detail', '')))
        elif comp['outcome'] == 'ok' and expected:
            f.append('declared refusal answered ok: ' + name)
    for key, resource, start, end in fault_windows(recs):
        if end is None:
            f.append('fault left applied: %s on %s (applied at seq %d, no fault_restore completed ok after it)' % (key, resource, start))
    return verdict(f, judged=judged)


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
            return {'verdict': 'UNKNOWN', 'judged': 0, 'findings': ['the environment is not known from seq %d (a fault operation without a known outcome): this checker does not judge' % u]}
        o = overlaps(recs)
        if o:
            return {'verdict': 'UNKNOWN', 'judged': 0, 'findings': ['operations overlap on %s: this checker judges only one-at-a-time histories' % ', '.join(sorted(o))]}
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
    f, judged = [], 0   # judged: reads compared
    last_write = {}   # (vol, lba) -> index of the last write event
    last_read = {}    # (vol, lba) -> index of the last ok read event
    for i, ev in enumerate(events):
        kind, inv = ev[0], ev[1]
        if kind == 'write' and ev[2]['outcome'] in ('ok', 'unknown'):
            last_write[(inv['volume'], inv['args']['lba'])] = i
        elif kind == 'read':
            judged += 1
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
    return verdict(f, judged=judged)


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
    f, judged = [], 0   # judged: restored blocks compared
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
            judged += 1
            got = (ev[2].get('result') or {}).get('sha256')
            allowed = image.get(lba, {ZERO})
            if got not in allowed:
                f.append('%s lba %d: first read %s, not a content of %s/%s (%s)' % (target, lba, got, source, snap, sorted(allowed)))
    return verdict(f, judged=judged)


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
    f, judged = [], 0   # judged: cross reads compared (a read of a block the other side wrote after the restore)
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
                if written[other].get(lba):
                    judged += 1
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
            return {'verdict': 'UNKNOWN', 'judged': judged, 'findings': f + ['%s and %s were written the same content at %s: the directions cannot be told apart' % (source, target, ', '.join(shared[:4]))]}
        if not exercised[source]:
            f.append('%s -> %s not exercised: no write to the source read back on the target at the same block' % (source, target))
        if not exercised[target]:
            f.append('%s -> %s not exercised: no write to the target read back on the source at the same block' % (target, source))
    return verdict(f, judged=judged)

def c7(recs):
    f = []
    earlier = superseded(recs)   # an ok is always a request's last attempt (nothing reruns an ok)
    deleted = {}    # volume -> completion seq of its ok delete
    creates = []    # (volume, invocation seq, completion) of every create
    for inv, comp in operations(recs):
        if inv['verb'] == 'delete' and comp and comp['outcome'] == 'ok':
            deleted[inv['volume']] = comp['seq']
        elif inv['verb'] == 'create' and inv['seq'] not in earlier:   # a request's answer is its last attempt's
            creates.append((inv['volume'], inv['seq'], comp))
    if not deleted:
        return verdict(['no delete with outcome ok in the history: C7 judges nothing'])
    seen = set()
    for r in recs:
        vol = r.get('volume')
        # An observation of the volume, not a generator record (gen_step and
        # the like are observe records that name a volume too).
        if r['kind'] != 'observe' or r.get('verb') != 'observe' or vol not in deleted or vol in seen or r['seq'] < deleted[vol]:
            continue
        seen.add(vol)
        # The product's shape of a deleted volume (its tombstone).
        f += PRODUCT.after_delete_findings(vol, r)
    # In v1 the master keeps a deleted name's reservation: a create of that
    # name after its ok delete is REFUSED; the product reader judges the answer.
    recreated = set()
    for vol, at, comp in creates:
        if vol not in deleted or at < deleted[vol]:
            continue
        recreated.add(vol)
        bad = PRODUCT.recreate_finding(vol, comp)
        if bad:
            f.append(bad)
    if not recreated:
        f.append('no create of a deleted name was attempted: the reservation rule was not exercised')
    never = sorted(set(deleted) - seen)
    if never:
        f.append('deleted but never observed afterwards: %s' % ', '.join(never))
    return verdict(f, judged=len(seen))   # judged: deleted volumes observed


def touches(inv, vol):
    """Whether an operation may change volume `vol` as far as the history
    tells: its own volume, a restore's source, and every volume for a fault
    operation (a VM fault is not mapped to volumes)."""
    if inv['verb'] in FAULT_VERBS:
        return True
    return vol in (inv.get('volume'), (inv.get('args') or {}).get('source') if inv['verb'] == 'restore' else None)


def observed_state(obs):
    """The comparable state of an observation: {part: value} for each part it
    records, and {part: why} for each part it records but cannot compare. The
    parts are checks.json's observation.compared; the product reader reads them."""
    return PRODUCT.observed_state(obs, STATE_PARTS)


def state_diff(a, b):
    """The parts in which two comparable states differ, and the parts not
    compared because neither records them, or (None, why) when they cannot be
    compared."""
    (sa, ba), (sb, bb) = a, b
    if ba or bb:
        return None, 'records %s' % '; '.join('%s: %s' % kv for kv in sorted({**ba, **bb}.items()))
    one = sorted(set(sa) ^ set(sb))
    if one:
        return None, 'records %s in one observation only' % ', '.join(one)
    # `a` is the earlier observation: the product reader decides whether the
    # later value is the same state (it may allow a step that time alone makes).
    return [p for p in STATE_PARTS if p in sa and not PRODUCT.same_part(p, sa[p], sb[p])], [p for p in STATE_PARTS if p not in sa]


def show_part(state, part):
    return PRODUCT.show_part(state, part)


def limits_of(recs):
    """{invoke seq: limit}: the limit the generator's latest gen_step names for
    the operations it proposed, else "ordinary <verb>"."""
    out, step = {}, None
    for r in recs:
        if r['kind'] == 'observe' and r.get('verb') == 'gen_step':
            step = (r.get('result') or {}).get('negative')
        elif r['kind'] == 'invoke':
            out[r['seq']] = step if step else 'ordinary %s' % r['verb']
    return out


def baseline(recs):
    """(pairs checked, findings): every two consecutive observations of a
    volume with no operation touching it invoked between them."""
    last, checked, f = {}, 0, []
    for r in recs:
        if r['kind'] == 'invoke':
            for vol in list(last):
                if touches(r, vol):
                    del last[vol]
        elif r['kind'] == 'observe' and r.get('verb') == 'observe' and r.get('volume'):
            vol = r['volume']
            if vol in last:
                diff, _ = state_diff(observed_state(last[vol]), observed_state(r))
                if diff is not None:
                    checked += 1
                    if diff:
                        f.append('baseline: %s changed with no operation on it (%s; observations seq %d, %d): the product works in the background here, C5 cannot lay a change to a refusal'
                                 % (vol, ', '.join(diff), last[vol]['seq'], r['seq']))
            last[vol] = r
    return checked, f


@unknown_on_overlap
def c5(recs):
    """A refused operation changed nothing; a refused_effect changed exactly
    its effect. Judged only when the operations never overlap
    (unknown_on_overlap): an operation open on the volume during the refusal
    could move HEAD between the two observations, and the observations could
    no longer be laid to the refusal alone."""
    ops_ = operations(recs)
    # A request's refusal is its LAST attempt's: an earlier attempt (a rerun
    # through, say, the lease horizon) is no refusal of the request's limit.
    earlier = superseded(recs)
    refusals = [(inv, comp) for inv, comp in ops_ if comp is not None and comp['outcome'] in REFUSALS]
    if not [1 for inv, _ in refusals if inv['seq'] not in earlier]:
        return {'verdict': 'FAIL', 'findings': ['no refused operation in the history: C5 not exercised']
                + (['%d refused attempt(s) a later attempt of the same request replaced: no refusal of any limit' % len(refusals)] if refusals else [])}
    inv_of = {r['seq']: r for r in recs if r['kind'] == 'invoke'}   # a completion carries no args
    limit = limits_of(recs)
    f, notes, gaps = [], [], set()
    per = {}   # limit -> [refusals, judged], last attempts only
    attempts_judged = 0   # earlier attempts judged by the whole-state rule, under no limit
    for inv, comp in ops_:
        if inv['seq'] in earlier:
            continue   # the request's answer is its last attempt's
        if comp is not None and comp['outcome'] not in REFUSALS and (inv.get('args') or {}).get('expect') == 'refused':
            # ok is a FAIL: the limit let the operation through; unknown is not
            # ok and stays a note.
            (f if comp['outcome'] == 'ok' else notes).append('negative not refused: seq %d %s %s (%s) answered %s'
                                                            % (inv['seq'], inv['verb'], inv.get('volume'), limit[inv['seq']], comp['outcome']))
    for inv, comp in refusals:
        vol = inv.get('volume')
        last = inv['seq'] not in earlier
        lim = limit[inv['seq']] if last else '%s, attempt %s replaced by a later attempt (counted under no limit)' % (
            limit[inv['seq']], (inv.get('args') or {}).get('attempt'))
        if last:
            per.setdefault(lim, [0, 0])[0] += 1
        name = 'seq %d %s %s' % (inv['seq'], inv['verb'], vol or '(no volume)')
        if not vol:
            notes.append('not judged: %s: an operation on no volume' % name)
            continue
        before = after = None
        for r in recs:   # the latest observation before the invoke; a completion on the volume that may have changed it voids it
            if r['seq'] >= inv['seq']:
                break
            if r['kind'] == 'observe' and r.get('verb') == 'observe' and r.get('volume') == vol:
                before = r
            elif before is not None and r['kind'] == 'complete' and r['outcome'] != 'refused' and touches(inv_of[r['of']], vol):
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
        b, a = observed_state(before), observed_state(after)
        diff, missing = state_diff(b, a)
        if diff is None:
            notes.append('not judged: %s: observations seq %d, %d: %s' % (name, before['seq'], after['seq'], missing))
            continue
        lacking = [p for p in REQUIRED_PARTS if p in missing]
        if lacking:
            notes.append('not judged: %s: observations seq %d, %d record no %s (required)' % (name, before['seq'], after['seq'], ', '.join(lacking)))
            continue
        effect = (comp.get('result') or {}).get('effect') if comp['outcome'] == 'refused_effect' else None
        if effect is not None and effect['part'] not in STATE_PARTS:
            notes.append('not judged: %s: its effect names %r, not a compared part (%s)' % (name, effect['part'], ', '.join(STATE_PARTS)))
            continue
        if effect is not None and effect['part'] in missing:
            notes.append('not judged: %s: observations seq %d, %d record no %s (its effect)' % (name, before['seq'], after['seq'], effect['part']))
            continue
        span = '(observations seq %d, %d)' % (before['seq'], after['seq'])
        found = ['%s (%s) %s but its %s changed: %s -> %s %s'
                 % (name, lim, comp['outcome'], part, show_part(b, part), show_part(a, part), span)
                 for part in diff if effect is None or part != effect['part']]
        if effect is not None:
            # The effect is a state that stays: after the operation its part
            # HAS the documented value (before: another value, or already
            # this one, as for a repeated refusal); every other part is
            # unchanged (above).
            bad, cannot = PRODUCT.effect_state(effect['part'], effect['to'], after)
            if bad:
                found.append('%s (%s) refused_effect but its %s is not in the documented state: %s (effect %r, %s) %s'
                             % (name, lim, effect['part'], '; '.join(bad), effect['to'], effect['cite'], span))
            elif cannot and not found:
                notes.append('not judged: %s: observation seq %d cannot show its effect: %s' % (name, after['seq'], '; '.join(cannot)))
                continue
        gaps.update(missing)
        if last:
            per[lim][1] += 1
        else:
            attempts_judged += 1
        f += found
    report = ['limit %s: %d of %d refusal(s) judged' % (lim, j, n) for lim, (n, j) in sorted(per.items())]
    if gaps:
        notes.append('not compared (recorded by no observation): %s' % ', '.join(p for p in STATE_PARTS if p in gaps))
    replaced = sum(1 for inv, _ in refusals if inv['seq'] in earlier)
    if replaced:
        notes.append('refused attempts a later attempt of the same request replaced: %d (%d judged by the whole-state rule), counted under no limit'
                     % (replaced, attempts_judged))
    judged = sum(j for _, j in per.values()) + attempts_judged   # judged: refusals judged (last attempts, and earlier ones a pair allowed)
    checked, bf = baseline(recs)
    if bf or not checked:   # a change at a refusal may be the background work: not laid to the refusal
        return {'verdict': 'UNKNOWN', 'judged': judged, 'findings': (bf or ['baseline not established: no two consecutive observations of a volume with nothing between them']) + f + report + notes}
    if f:
        return {'verdict': 'FAIL', 'judged': judged, 'findings': f + report + notes}
    unjudged = [lim for lim, (n, j) in sorted(per.items()) if not j]
    if unjudged:
        return {'verdict': 'UNKNOWN', 'judged': judged, 'findings': ['limit(s) with no judged refusal: %s' % ', '.join(unjudged)] + report + notes}
    return {'verdict': 'PASS', 'judged': judged, 'findings': report + notes}


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


def kill_scope(inv):
    """The volumes a kill reaches on the data path: a set (empty: none), or
    None for every volume. The product reader reads the kill's arguments
    (for block: a holder kill reaches its volume, a master kill none)."""
    return PRODUCT.kill_scope(inv)


@unknown_on_overlap
def dur(recs):
    """Durability with flush.
    Scope (M2): one holder at a time on the same store, one replica: a
    volume's data path is its holder and that holder's store, nothing else.
    Replicated volumes (a failover to another replica) are M3's and are not
    judged here; a PASS says so. Per volume and block:
      durable   the values the last durable write may have left: a flushed
                write acknowledged (ok), or the writes since the previous
                durable state once a flush of the same volume is acknowledged
                (from the last acknowledged write on; with none acknowledged,
                added to the previous durable values). A flush with outcome
                unknown or refused makes nothing durable.
      volatile  every write completed (ok or unknown) since the durable state.
      open      the values of unknown writes whose holder is not known dead:
                such a write may take effect at any time until then, also
                after a later durable write.
      may hold  what a read may return: C1's shadow (an ok write its value
                only, an unknown write adds its value), with open unknowns.
    A kill (verb kill, outcome not refused, applied at its invoke) reaches
    the volumes kill_scope() names. A holder kill (args.volume) makes that
    volume's holder known dead: it may lose that volume's unflushed writes
    and settles that volume's open unknowns, and no other volume's. A kill of
    the master reaches no holder: it loses nothing and settles nothing (the
    data path is the holders'), so a run whose only kills are the master's
    is judged as a run without a kill. A kill naming no volume is applied to
    every volume, and a note says so: there it allows each volume to lose
    unflushed writes the killed process may not explain, and settles every
    open unknown, so an unknown whose holder in fact survived and lands
    later is reported although the product may be right; every finding
    after such a kill says that it was applied to every volume.
    A holder restarted with no recorded kill (a healthy observation whose
    result.holders gives the volume another pid or start time than the
    previous healthy observation, with no kill or restart reaching that
    volume between them; for example its lease lost in a long master outage
    and the unit restarted it) is a kill of that volume at the later healthy
    record, reported "holder of <v> restarted with no recorded kill (healthy
    seq a -> b)". Reads between the restart and that record are judged as
    before a kill.
    At a kill each block may hold durable | volatile | open: the last durable
    value, any write invoked after it, or any unknown write whose holder was
    alive after the durable point. Then the unknowns are settled: the first
    read of the block after the kill fixes its value, and a later read that
    differs with no write (and no kill) in between FAILS. That first read
    also fixes what the next kill may leave: the durable set before the kill
    plus the value read, or the value read alone when it is one of that
    durable set (it says which durable value it was). So a value lost at one
    kill does not come back at the next, and a durable value read does not
    vanish at the next kill unless a later write explains it. A read outside its
    block's may hold FAILS; after a kill the finding says which: ZERO although
    a durable write exists, content no write produced, or an older value than
    the last durable one that no open unknown explains.
    Counted and reported on PASS: on the first read after a kill, every
    acknowledged unflushed write after the value read ("acknowledged, not
    flushed, lost: n"; not counted when an older unknown landed over them);
    and every read whose value only an unknown write invoked before the last
    durable write explains ("unknown write landed after a later durable
    write: n"). Without a kill this is C1's rule, judged the same way here,
    and every written block is read after its last write. No kill and no
    unflushed write: FAIL, not exercised."""
    ops_ = operations(recs)
    comp_of = {inv['seq']: comp for inv, comp in ops_}
    # Kills that reach a holder; a master kill reaches none.
    nkills = sum(1 for inv, comp in ops_ if killed(inv, comp) and kill_scope(inv) != set())
    unscoped = []   # seqs of kills applied to every volume
    nunflushed = sum(1 for inv, _ in ops_ if inv['verb'] == 'write' and not flushed(inv.get('args') or {}))
    if not nkills and not nunflushed:
        return verdict(['no kill of a holder and no unflushed write in the history: DUR not exercised'])
    _, _, images = replay(recs)
    base = {}    # volume -> {lba: set}, a restore target's initial content
    st = {}      # (volume, lba) -> block state
    inv_of = {}
    f, lost, landed, unread_after_kill, judged_after_kill, reads_judged = [], 0, 0, 0, 0, 0
    last_write, last_read = {}, {}

    def block(vol, lba):
        b = st.get((vol, lba))
        if b is None:
            init = set(base.get(vol, {}).get(lba, {ZERO}))
            # kill: (kill seq, durable, volatile, early unknowns, open unknowns) until the
            # first read after the kill; settled: the value that read fixed.
            b = st[(vol, lba)] = {'durable': init, 'volatile': [], 'cur': set(init), 'open': set(),
                                  'produced': set(init), 'kill': None, 'settled': None}
        return b

    kinds = {}   # kill seq -> what the findings say about it

    def apply_kill(seq, scope):
        # scope: a set of volumes, or None for every volume.
        for (kv, _), b in st.items():
            if scope is not None and kv not in scope:
                continue
            drop_kill(b)
            vals = {s for s, _ in b['volatile']}
            early = b['open'] - b['durable'] - vals   # unknowns invoked before the durable point
            allowed = b['durable'] | vals | b['open']
            b['kill'] = (seq, set(b['durable']), list(b['volatile']), early, set(b['open']))
            b['cur'], b['open'], b['durable'], b['volatile'] = set(allowed), set(), set(allowed), []
            b['settled'] = None

    # Holder identities from healthy records: volume -> (pid, start) and the
    # healthy seq it was seen at; covered: volumes a kill or restart reached
    # since (None: one that names no volume reached every volume).
    holders, holder_seq, covered = {}, {}, set()
    restarted = []

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
            if r['verb'] in ('kill', 'restart'):
                # Either one explains a new holder at the next healthy record
                # (a refused one too: the check is about explaining, not judging).
                reach = kill_scope(r)
                covered.update({None} if reach is None else reach)
            scope = kill_scope(r) if killed(r, comp_of.get(r['seq'])) else set()
            if scope is None:
                unscoped.append(r['seq'])
                kinds[r['seq']] = ' (a kill naming no volume, applied to every volume)'
            if scope != set():
                apply_kill(r['seq'], scope)
            continue
        if r['kind'] == 'observe' and r.get('verb') == 'healthy':
            # The product reader names each volume's holder identity in the
            # healthy record; a changed identity is a restarted holder.
            seen = PRODUCT.holder_identity(r)
            for hv, ident in sorted(seen.items()):
                if hv in holders and holders[hv] != ident and hv not in covered and None not in covered:
                    restarted.append('holder of %s restarted with no recorded kill (healthy seq %d -> %d)' % (hv, holder_seq[hv], r['seq']))
                    kinds[r['seq']] = ' (holder of %s restarted with no recorded kill)' % hv
                    apply_kill(r['seq'], {hv})
                holders[hv], holder_seq[hv] = ident, r['seq']
            # A kill or restart is "between" two healthy records of a volume
            # only until the volume's next one.
            covered.difference_update(seen)
            if seen:
                covered.discard(None)
            continue
        if r['kind'] != 'complete':
            continue
        inv = inv_of[r['of']]
        verb, out, vol, args = inv['verb'], r['outcome'], inv.get('volume'), inv.get('args') or {}
        if verb == 'write' and out in ('ok', 'unknown'):
            sha, fl = args['sha256'], flushed(args)
            b = block(vol, args['lba'])
            drop_kill(b)
            b['settled'] = None
            b['produced'].add(sha)
            if out == 'ok':
                b['cur'] = {sha}
            else:
                b['cur'].add(sha)
                b['open'].add(sha)
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
            allowed = b['cur'] | b['open']
            k, settled = b['kill'], b['settled']
            reads_judged += 1
            if got not in allowed:
                if settled is not None:
                    f.append('read %s lba %d seq %d returned %s, but the read at seq %d after the kill at seq %d%s returned %s, with no write in between: the value changed after the unknowns were settled'
                             % (vol, lba, inv['seq'], got, settled[1], settled[0],
                                kinds.get(settled[0], ''), settled[2]))
                elif k is None:
                    f.append('read %s lba %d seq %d returned %s, not one of %s' % (vol, lba, inv['seq'], got, sorted(allowed)))
                else:
                    if got == ZERO:
                        why = 'ZERO although a durable write exists'
                    elif got not in b['produced']:
                        why = 'content no write produced'
                    else:
                        why = 'an older value than the last durable one that no open unknown explains'
                    f.append('after the kill at seq %d%s, read %s lba %d seq %d returned %s: %s (allowed %s)' % (
                        k[0], kinds.get(k[0], ''), vol, lba, inv['seq'], got, why, sorted(allowed)))
            elif k is not None:
                _, kdur, vlist, early, _ = k
                if got in early and got not in kdur and got not in {s for s, _ in vlist}:
                    landed += 1
                else:
                    at = max((n for n, (s, _) in enumerate(vlist) if s == got), default=-1)
                    lost += sum(1 for _, ack in vlist[at + 1:] if ack)
            elif got not in b['cur'] and got not in b['durable'] and got not in {s for s, _ in b['volatile']}:
                landed += 1   # only an open unknown from before the durable point explains it
            if k is not None:
                judged_after_kill += 1
                b['kill'] = None
                # Settled: the holder is dead, every unknown took effect or
                # never will; the block holds the value read until a write.
                b['cur'] = {got}
                # The next kill may leave only the durable set before this
                # kill plus the value read: a write lost here, or an unknown
                # settled as not taken effect, does not come back. A value
                # read that is one of that durable set and of nothing newer
                # names the durable value: the others do not come back either.
                newer = {s for s, _ in k[2]}   # writes after the durable point
                b['durable'] = {got} if got in k[1] and got not in newer else k[1] | {got}
                b['settled'] = (k[0], inv['seq'], got)
            last_read[(vol, lba)] = i
    for b in st.values():
        drop_kill(b)
    nkills += len(restarted)   # a holder restarted with no recorded kill is a kill
    if nkills:
        if not judged_after_kill:
            f.append('%d kill(s), no block read after any of them: the durability rule judged nothing' % nkills)
    else:
        # C1's rule without a kill: every written block is read after its last write.
        unread = sorted(key for key, w in last_write.items() if last_read.get(key, -1) < w)
        if unread:
            f.append('written, never read afterwards: %d (%s)' % (len(unread), ', '.join('%s lba %d' % key for key in unread[:8])))
    notes = ['acknowledged, not flushed, lost: %d' % lost, 'unknown write landed after a later durable write: %d' % landed] + restarted
    notes.append("scope: same holder, same store, one replica (M2); replicated volumes are M3's and not judged here")
    if unscoped:
        notes.append('kill(s) naming no volume, applied to every volume: seq %s' % ', '.join(str(x) for x in unscoped))
    if unread_after_kill:
        notes.append('acknowledged, not flushed, not read after the kill: %d block(s)' % unread_after_kill)
    # judged: the reads judged after a kill; in a run without a holder kill,
    # the reads judged by C1's rule (its unflushed writes are what it tests).
    judged = judged_after_kill if nkills else reads_judged
    if not f and not judged:
        f.append('no read judged: DUR not exercised')
    return {'verdict': 'FAIL' if f else 'PASS', 'judged': judged, 'findings': f + notes}



# C6 latency bounds (BOUNDS) and recovery terms (RECOVERY) come from the
# product's checks.json, each with its cite: only a bound a page states, never
# an invented number. Recovery is a bound on REACHING a healthy point, not the
# latency of an operation: a holder is healthy again by
#   max(kill completion + regrant, restart completion) + start_wait.
# (For block: no stated operation bound, regrant 65 s, start wait 30 s, no
# restart bound for the master.)
RUNNER_VERBS = FAULT_VERBS + ('kill', 'restart')   # the environment's operations, not the product's


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


def recovery_findings(recs, ops_, healthy):
    """Recovery on the history's one clock, as the pages read: the holder is
    healthy again (the first healthy observation with healthy true after its
    restart) by max(its kill's completion + regrant, the restart's
    completion) + start_wait. A blockmaster restart has no stated bound and
    is listed; so is a holder restart whose recovery a blockmaster kill or
    restart overlaps (the pages then move the re-grant to after the master's
    restart), a restart without an ok completion, and one with no ok kill of
    the holder before it (no moment to measure from)."""
    f, notes, timed = [], [], 0   # timed: recoveries measured against the deadline
    regrant, rwhere = RECOVERY['regrant']
    wait, wwhere = RECOVERY['start_wait']
    for inv, comp in ops_:
        if inv['verb'] != 'restart':
            continue
        proc = PRODUCT.process_of(inv)
        name = 'restart seq %d %s' % (inv['seq'], proc)
        if proc == MASTER:
            notes.append('recovery not judged: %s (no restart bound stated for the master)' % name)
            continue
        if HOLDER_PREFIX is None or not proc.startswith(HOLDER_PREFIX):
            notes.append('recovery not judged: %s (not a holder)' % name)
            continue
        if comp is None or comp['outcome'] != 'ok':
            notes.append('recovery not judged: %s (%s: no moment to measure from)' % (name, 'no completion' if comp is None else 'outcome ' + comp['outcome']))
            continue
        h = next((r for r in healthy if r['seq'] > comp['seq'] and (r.get('result') or {}).get('healthy') is True), None)
        if h is None:
            f.append('%s: never healthy afterwards (bounds: re-grant %g s, %s; start wait %g s, %s)' % (name, regrant, rwhere, wait, wwhere))
            continue
        # The holder's stop: its latest kill before this restart.
        kills = [(i2, c2) for i2, c2 in ops_ if i2['verb'] == 'kill' and i2['seq'] < inv['seq']
                 and PRODUCT.process_of(i2) == proc]
        start = kills[-1][0]['seq'] if kills else inv['seq']
        master = next((i2 for i2, _ in ops_ if i2['verb'] in ('kill', 'restart') and start < i2['seq'] < h['seq']
                       and PRODUCT.process_of(i2) == MASTER), None)
        if master is not None:
            notes.append('recovery not judged: %s (%s %s seq %d before its healthy point)' % (name, MASTER, master['verb'], master['seq']))
            continue
        if not kills or kills[-1][1] is None or kills[-1][1]['outcome'] != 'ok':
            notes.append('recovery not judged: %s (no kill of %s completed ok before it: the stop is not known)' % (name, proc))
            continue
        stop = kills[-1][1]['mono_ns']
        regranted = stop + regrant * 10**9
        deadline = max(regranted, comp['mono_ns']) + wait * 10**9
        timed += 1
        if h['mono_ns'] > deadline:
            s = lambda ns: (ns - stop) / 1e9   # seconds after the stop
            f.append('%s: healthy at %.3f s after the stop (healthy seq %d), past the deadline %.3f s = max(re-grant: stop + %g s = %.3f s, '
                     'restart completion %.3f s) + start wait %g s (%s; %s)'
                     % (name, s(h['mono_ns']), h['seq'], s(deadline), regrant, s(regranted), s(comp['mono_ns']), wait, rwhere, wwhere))
    return f, notes, timed


def _settle(recs, ops_, inv, comp):
    """An unknown lifecycle operation leaves its volume unsettled until a
    settled record of the volume places it, within the recovery deadline:
    max(the latest completed, not refused kill reaching the volume before the operation's end +
    regrant, the operation's end) + start_wait; with no such kill, the end +
    regrant + start_wait. Returns ('resolved', why), ('fail', why), or
    ('superseded', why) when another lifecycle operation of the volume was
    invoked first, inside the deadline (its own answer sets the intent)."""
    vol = inv.get('volume')
    end = inv if comp is None else comp
    regrant, rwhere = RECOVERY['regrant']
    wait, wwhere = RECOVERY['start_wait']
    s = 10**9
    kills = [c2 for i2, c2 in ops_ if killed(i2, c2) and c2 is not None and i2['seq'] < end['seq']
             and (lambda sc: sc is None or vol in sc)(kill_scope(i2))]
    if kills:
        deadline = max(kills[-1]['mono_ns'] + regrant * s, end['mono_ns']) + wait * s
        terms = 'max(kill seq %d + %g s, the end) + %g s' % (kills[-1]['of'], regrant, wait)
    else:
        deadline = end['mono_ns'] + (regrant + wait) * s
        terms = 'the end + %g s + %g s (no kill)' % (regrant, wait)
    terms += ' (%s; %s)' % (rwhere, wwhere)
    late = lambda ns: (ns - end['mono_ns']) / 1e9
    for r in recs:
        if r['seq'] <= end['seq']:
            continue
        if r['kind'] == 'invoke' and r.get('volume') == vol and r['verb'] in PRODUCT.LIFECYCLE:
            if r['mono_ns'] <= deadline:
                return 'superseded', 'seq %d %s invoked before it settled' % (r['seq'], r['verb'])
            break
        got = PRODUCT.settled(r)
        if got is not None and got[0] == vol:
            if r['mono_ns'] <= deadline:
                return 'resolved', 'settled %s (seq %d, %.3f s after its end)' % (got[1], r['seq'], late(r['mono_ns']))
            return 'fail', 'settled only at seq %d, %.3f s after its end, past the deadline %.3f s = %s' % (r['seq'], late(r['mono_ns']), late(deadline), terms)
    return 'fail', 'never settled within the deadline %.3f s after its end = %s' % (late(deadline), terms)


@unknown_on_overlap
def c6(recs):
    """Bounds and "none stays unknown" (see the module docstring)."""
    f, notes, unruled, resolved = [], [], [], 0
    ops_ = operations(recs)
    healthy = [r for r in recs if r['kind'] == 'observe' and r.get('verb') == 'healthy']
    # An unknown resolves per request, by its last attempt: an earlier attempt
    # a later one replaced is not an unknown of its own.
    earlier = superseded(recs)
    unknowns = [(inv, comp) for inv, comp in ops_ if inv['verb'] not in RUNNER_VERBS and inv['verb'] != 'read'
                and inv['seq'] not in earlier and (comp is None or comp['outcome'] == 'unknown')]
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
    f_rec, n_rec, timed = recovery_findings(recs, ops_, healthy)
    f += f_rec
    notes += n_rec
    # A volume in neither consistent state, as the healthy record shows it.
    for r in healthy:
        for vol, why in PRODUCT.third_state(r):
            f.append('third state: %s at healthy seq %d: %s' % (vol, r['seq'], why))
    # None stays unknown.
    for inv, comp in unknowns:
        name = 'seq %d %s %s' % (inv['seq'], inv['verb'], inv.get('volume', ''))
        settled_ok = False
        if inv['verb'] in PRODUCT.NEEDS_SETTLED:
            # An unknown lifecycle operation is resolved by the settled record;
            # a verb with its own placement rule is still checked by it below
            # for a third state.
            how, why = _settle(recs, ops_, inv, comp)
            if how == 'fail':
                f.append('unsettled: %s: %s' % (name, why))
                continue
            if how == 'resolved':
                resolved += 1
                settled_ok = True
                if inv['verb'] not in PLACED_BY_OBSERVATION:
                    continue
            else:
                notes.append('not settled: %s: %s; placed by its own rule' % (name, why))
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
            base = _baseline(recs, inv['volume'], inv['seq']) if inv['verb'] in PRODUCT.NEEDS_BASELINE else None
            if obs is None:
                how, why = None, 'no observation of %s after %s' % (inv['volume'], at)
            elif inv['verb'] in PRODUCT.NEEDS_BASELINE and base is None:
                how, why = None, 'no clean observation of %s before it' % inv['volume']
            elif _volume_changed(recs, inv['volume'], done, obs['seq']) is not None:
                ch = _volume_changed(recs, inv['volume'], done, obs['seq'])
                how, why = None, 'seq %d %s changed %s before the observation seq %d' % (ch['seq'], ch['verb'], inv['volume'], obs['seq'])
            else:
                how, why = PRODUCT.place_by_status(inv, obs, base)
        if how is None:
            if not settled_ok:   # the settled record already resolved it
                f.append('unknown not resolved: %s (due at %s): %s' % (name, at, why))
        elif how == 'third':
            f.append('unknown in a third state: %s (due at %s): %s' % (name, at, why))
        elif not settled_ok:
            resolved += 1
            if inv['verb'] in PRODUCT.NOTED:
                notes.append('%s placed %s: %s' % (name, how, why))   # names which rule judged it
    # Always reported, in this exact form, so a night can require n > 0 in
    # at least one run; a single run with n = 0 and its healthy points PASSes.
    count = 'unknowns resolved: %d' % resolved
    judged = resolved + timed   # judged: unknowns resolved plus recoveries timed
    if f:
        return {'verdict': 'FAIL', 'judged': judged, 'findings': f + notes + [count]}
    if unruled:
        return {'verdict': 'UNKNOWN', 'judged': judged, 'findings': ['no resolution rule for an unknown: %s' % ', '.join(unruled)] + notes + [count]}
    if not judged:
        # Healthy points alone, with no unknown to resolve and no recovery
        # to time: nothing was judged, so no PASS.
        return {'verdict': 'FAIL', 'judged': 0, 'findings': ['no unknown resolved and no recovery timed: C6 not exercised'] + notes + [count]}
    return {'verdict': 'PASS', 'judged': judged, 'findings': notes + [count]}


def master_windows(recs):
    """(kill invoke, kill completion, restart invoke, restart completion or
    None) of each master outage: an ok kill of the master and the next ok
    restart of it after that kill's completion."""
    out = []
    ops_ = operations(recs)
    for i, (inv, comp) in enumerate(ops_):
        if inv['verb'] != 'kill' or PRODUCT.process_of(inv) != MASTER or comp is None or comp['outcome'] != 'ok':
            continue
        nxt = next(((i2, c2) for i2, c2 in ops_[i + 1:] if i2['verb'] == 'restart' and PRODUCT.process_of(i2) == MASTER
                    and i2['seq'] > comp['seq'] and c2 is not None and c2['outcome'] == 'ok'), None)
        out.append((inv, comp) + (nxt if nxt else (None, None)))
    return out


def mo(recs):
    """A short master outage interrupts nothing: inside the window from an ok
    kill of the master's completion to its ok restart's completion, when the
    window is at most MASTER_OUTAGE seconds on the history's clock, (a) every
    write invoked in it is answered ok, (b) every single-look healthy record
    in it (result.look == "single") shows no volume intended to be held not
    serving (product intended_states; a volume with no lifecycle record
    counts as held), (c) every
    holder identity such a look shows equals the volume's last identity
    recorded before the kill. A window with no write or no look is UNKNOWN; a
    longer window, or a kill never restarted, is not judged and listed. FAIL
    (not exercised) when the history holds no ok kill of the master."""
    if MASTER is None or MASTER_OUTAGE is None:
        return {'verdict': 'UNKNOWN', 'findings': ['the product description states no master or no short-outage bound: MO does not judge']}
    windows = master_windows(recs)
    if not windows:
        return {'verdict': 'FAIL', 'findings': ['no ok kill of %s in the history: MO not exercised' % MASTER]}
    limit, cite = MASTER_OUTAGE
    ops_ = operations(recs)
    healthy = [r for r in recs if r['kind'] == 'observe' and r.get('verb') == 'healthy']
    f, notes, unexercised, judged = [], [], [], 0
    for kinv, kcomp, rinv, rcomp in windows:
        name = 'outage kill seq %d' % kinv['seq']
        if rcomp is None:
            notes.append('not judged: %s: no ok restart of %s after it' % (name, MASTER))
            continue
        secs = (rcomp['mono_ns'] - kcomp['mono_ns']) / 1e9
        if secs > limit:
            notes.append('not judged: %s: %.3f s to the restart, above the %g s short outage (%s)' % (name, secs, limit, cite))
            continue
        lo, hi = kcomp['seq'], rcomp['seq']
        name = '%s (%.3f s)' % (name, secs)
        writes = [(i2, c2) for i2, c2 in ops_ if i2['verb'] == 'write' and lo < i2['seq'] < hi]
        looks = [r for r in healthy if lo < r['seq'] < hi and (r.get('result') or {}).get('look') == 'single']
        for i2, c2 in writes:
            if c2 is None or c2['outcome'] != 'ok':
                f.append('%s: write seq %d %s lba %s answered %s inside the window' % (
                    name, i2['seq'], i2.get('volume'), (i2.get('args') or {}).get('lba'), 'nothing' if c2 is None else c2['outcome']))
        before = {}
        for r in healthy:
            if r['seq'] >= kinv['seq']:
                break
            before.update(PRODUCT.holder_identity(r))
        for r in looks:
            ns = PRODUCT.not_serving(r)
            if ns is None:
                f.append('%s: look seq %d does not say which volumes serve' % (name, r['seq']))
            elif ns:
                # Only a volume intended to be held must serve: one stopped,
                # moved away or deleted is not held, and one unsettled is
                # placed by C6, not here.
                intent = PRODUCT.intended_states(recs, r['seq'])
                must = [v for v in ns if intent.get(v, 'held') == 'held']
                rest = [v for v in ns if v not in must]
                if must:
                    f.append('%s: look seq %d shows not serving: %s' % (name, r['seq'], ', '.join(must)))
                if rest:
                    notes.append('%s: look seq %d: not serving, not intended to be held: %s' % (
                        name, r['seq'], ', '.join('%s (%s)' % (v, intent[v]) for v in rest)))
            for vol, ident in sorted(PRODUCT.holder_identity(r).items()):
                if vol not in before:
                    notes.append('%s: look seq %d: no identity of the holder of %s recorded before the kill' % (name, r['seq'], vol))
                elif ident != before[vol]:
                    f.append('%s: look seq %d: holder of %s is %r, was %r before the kill' % (name, r['seq'], vol, ident, before[vol]))
        if not writes or not looks:
            unexercised.append('short master outage not exercised in window: %s (%d write(s), %d look(s))' % (name, len(writes), len(looks)))
            continue
        judged += 1
        unwritten = sorted(set(before) - {i2.get('volume') for i2, _ in writes})
        if unwritten:
            notes.append('%s: no write in the window on %s' % (name, ', '.join(unwritten)))
    count = 'short master outages judged: %d' % judged
    if f:
        return {'verdict': 'FAIL', 'judged': judged, 'findings': f + unexercised + notes + [count]}
    if unexercised or not judged:
        return {'verdict': 'UNKNOWN', 'judged': judged, 'findings': unexercised + notes + [count]}
    return {'verdict': 'PASS', 'judged': judged, 'findings': notes + [count]}   # judged: outages judged


CHECKERS = {'C0': c0, 'C1': c1, 'C2': c2, 'C3': c3, 'C5': c5, 'C6': c6, 'C7': c7, 'DUR': dur, 'MO': mo}

load_product(DEFAULT_PRODUCT)   # the default; main() loads --product over it


def check(lines):
    try:
        recs = read_history(lines)
    except Malformed as e:
        return {name: {'verdict': 'UNKNOWN', 'judged': 0, 'findings': ['history unreadable: %s' % e]} for name in CHECKERS}
    # Positive evidence first: an empty history, or one that does not end with
    # the writer's end record (a crashed runner, a truncated copy), is not a
    # run; every checker fails it rather than pass on what it never saw.
    if not recs:
        return {name: {'verdict': 'FAIL', 'judged': 0, 'findings': ['empty history: no evidence']} for name in CHECKERS}
    if recs[-1]['kind'] != 'end':
        return {name: {'verdict': 'FAIL', 'judged': 0, 'findings': ['the history has no end record: incomplete evidence (seq %d is the last)' % recs[-1]['seq']]} for name in CHECKERS}
    out = {}
    for name, fn in CHECKERS.items():
        try:
            out[name] = fn(recs)
        except (KeyError, TypeError) as e:
            out[name] = {'verdict': 'UNKNOWN', 'findings': ['record without a field the checker needs: %r' % (e,)]}
        # Every verdict carries its own count ("judged": the items the checker
        # actually judged); a verdict that judged nothing says 0.
        out[name].setdefault('judged', 0)
        if out[name]['verdict'] == 'PASS' and not out[name]['judged']:
            # Never reached by a checker (each one says "not exercised"
            # itself, and a test holds every checker to it); kept so a new
            # checker cannot PASS on nothing.
            out[name] = {'verdict': 'FAIL', 'judged': 0,
                         'findings': ['PASS with nothing judged: %s not exercised' % name] + out[name]['findings']}
    return out


def main(argv):
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument('history')
    p.add_argument('--json')
    p.add_argument('--product', default=DEFAULT_PRODUCT,
                   help='the product description directory (holds checks.json); default: the block product')
    p.add_argument('--checkers',
                   help='comma-separated checkers whose verdicts decide the exit code (default: all); '
                        'every checker is still run and written to --json')
    a = p.parse_args(argv)
    load_product(a.product)
    required = sorted(CHECKERS)
    if a.checkers:
        required = [c.strip() for c in a.checkers.split(',') if c.strip()]
        bad = [c for c in required if c not in CHECKERS]
        if bad or not required:
            p.error('unknown checker(s) %s; known: %s' % (', '.join(bad) or '(none given)', ', '.join(sorted(CHECKERS))))
    with open(a.history, encoding='utf-8') as fh:
        result = check(fh.read().splitlines())
    text = json.dumps(result, indent=1, sort_keys=True)
    if a.json:
        with open(a.json, 'w', encoding='utf-8') as fh:
            fh.write(text + '\n')
    for name, v in sorted(result.items()):
        print('%s %s%s%s' % (name, v['verdict'], '' if name in required else ' (not required)',
                             (': ' + '; '.join(v['findings'][:3])) if v['findings'] else ''))
    return 0 if all(result[name]['verdict'] == 'PASS' for name in required) else 1


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
