"""The block product's reading code for the history checkers.

history_check.py is generic: it loads this module by file, named in the
product directory's checks.json (reader.module), and calls only the names
below. Everything here reads what the block product shows: HEAD, its
snapshot names, the control plane's workspace answer, the volume's
directories and files, the tombstone a delete leaves, and the process names
of a kill. Numbers and names a page states live in checks.json, not here.

  bind(checks)                  receives checks.json once, at load
  observed_state(obs, parts)    an observation's comparable parts
  same_part(part, before, after)    whether a later value shows the same state
  show_part(state, part)        one part, short, for a finding
  PLACED_BY_STATUS              verbs of an unknown placed by an observation
  NEEDS_BASELINE                of those, the verbs placed against the one before too
  NOTED                         of those, the verbs whose placement C6 names in a note
  effect_state(part, to, after)   whether a part shows a refused_effect's state: (mismatches, cannot)
  place_by_status(inv, obs, base)   C6's per-verb placement rules
  after_delete_findings(vol, obs)   C7: the tombstone shape after an ok delete
  recreate_finding(vol, comp)       C7: a create of a deleted name must be refused
  process_of(inv)               the process a kill or restart names
  kill_scope(inv)               the volumes a kill reaches on the data path
  holder_identity(healthy_obs)  {volume: (pid, start)} of each holder, from a healthy record
  not_serving(healthy_obs)      the volumes a healthy record shows not serving, or None
  LIFECYCLE                     verb -> the intended state (held, not_held) its ok answer sets
  NEEDS_SETTLED                 of those, the verbs whose unknown a settled record resolves
  intended_states(recs, seq)    {volume: held | not_held | unsettled} before a record
  third_state(healthy_obs)      [(volume, why)] of volumes in neither consistent state
  settled(obs)                  (volume, state, evidence) of a settled record, else None
  holder_sample(inv, comp)      C4: the holder an operation went through, its epochs and identities before and after
  ack_modes(recs)               C4: {volume: acknowledging | best_effort} from the run's header
  master_lines(obs)             C4: {volume: (replica, epoch)} the master's line an observation shows
"""
import re

TOMBSTONE = {'HEAD', 'VOLUME'}   # a deleted volume's directory keeps exactly these
LIVE = {'HEAD', 'VOLUME'}        # a live volume's directory holds at least these
# The master's answer to a create of a deleted name (v1: the name's deletion
# reservation). C7 requires this reason, not any FailedPrecondition.
RESERVED_NAME = 'is being deleted or was deleted'

PLACED_BY_STATUS = ('snapshot', 'delete', 'move', 'restore')
NEEDS_BASELINE = ('snapshot',)
NOTED = ('snapshot',)   # the note names which rule (names or roots) judged it

_master = None
_lease_steps = frozenset()   # (earlier, later) lease states time alone may step through


def bind(checks):
    """Take the product facts this module reads from checks.json."""
    global _master, _lease_steps
    masters = [t['name'] for t in checks.get('fault_targets', []) if t.get('role') == 'master']
    _master = masters[0] if masters else None
    steps = (checks.get('observation') or {}).get('lease_time_steps') or {}
    _lease_steps = frozenset(tuple(s) for s in steps.get('steps', []))


# --- observation -----------------------------------------------------------

def _head(res):
    head = res.get('head')
    if isinstance(head, dict) and isinstance(head.get('gen'), int) and not isinstance(head.get('gen'), bool):
        return 'ok', ('head', head.get('gen'), head.get('epoch'), head.get('holder'), head.get('state'))
    if isinstance(res.get('show_error'), dict):
        return 'ok', ('show_error', res['show_error'].get('code'))
    if head is not None:
        return 'bad', 'HEAD %r' % (head,)
    return None


def _snapshots(res):
    # HEAD's snapshot names; a volume whose show answered an error code has no
    # HEAD and so no snapshots, a state like any other.
    if isinstance(res.get('snapshots'), list):
        return 'ok', tuple(sorted(res['snapshots']))
    if isinstance(res.get('show_error'), dict) and 'snapshots_error' in res:
        return 'ok', ('no HEAD',)
    if 'snapshots_error' in res:
        return 'bad', res['snapshots_error']
    return None


# `ops workspace show` (cmd/sw-block/workspace_show.go at read_at) prints one
# "key: value" fact per line. These keys it always prints, in this order of
# lines; then zero or more "move:" and "binding:" lines.
WORKSPACE_KEYS = ('volume', 'replica', 'epoch', 'lease state', 'holder', 'recorded ACK', 'host')
# Dropped: the line keyed "lease expiry_ns" also carries "host validity_ns at
# query"; the expiry moves at every renewal and the validity counts down at
# every query, so two queries with nothing between them differ there. Not a
# state a refusal can change (listed in checks.json observation.ignored). The
# line must have exactly this shape, or the text is not comparable.
WORKSPACE_DROPPED = {'lease expiry_ns': re.compile(r'lease expiry_ns: -?\d+; host validity_ns at query: -?\d+')}
WORKSPACE_DROPPED_FIELDS = ('lease expiry_ns', 'host validity_ns at query')


def workspace_facts(text):
    """The stable facts of a workspace answer printed with exit 0: a tuple of
    (key, value) per line, in order, without the dropped line; or None when
    the text is not in that shape (a line with no "key: value", a dropped
    line of another shape, or a fact the product always prints missing)."""
    lines = text.splitlines() if isinstance(text, str) else text
    if not isinstance(lines, list) or not all(isinstance(x, str) for x in lines):
        return None
    facts = []
    for line in lines:
        if not line.strip():
            continue
        key, sep, value = line.partition(': ')
        if not sep or not key.strip():
            return None
        if key in WORKSPACE_DROPPED:
            if not WORKSPACE_DROPPED[key].fullmatch(line):
                return None
            continue
        facts.append((key, value))
    if not set(WORKSPACE_KEYS) <= {k for k, _ in facts}:
        return None
    return tuple(facts)


def _workspace(res):
    ws = res.get('workspace')
    if not isinstance(ws, dict):
        return None
    if ws.get('exit') != 0:
        # A failed show prints its error on stderr; what was recorded (stdout)
        # is compared as it stands, with the exit status.
        return 'ok', (ws.get('exit'), ws.get('text') if not isinstance(ws.get('text'), list) else tuple(ws['text']))
    facts = workspace_facts(ws.get('text'))
    if facts is None:
        return 'bad', 'workspace text not in its "key: value" shape: %r' % (ws.get('text'),)
    return 'ok', (0, facts)


def _listing(part, err):
    def read(res):
        if isinstance(res.get(part), list):
            return 'ok', tuple(sorted(res[part]))
        if err in res:
            return 'bad', res.get(err)
        return None
    return read


READERS = {'head': _head, 'snapshots': _snapshots, 'workspace': _workspace,
           'dirs': _listing('dirs', 'dirs_error'), 'files': _listing('files', 'listing_error')}


def observed_state(obs, parts):
    """The comparable state of an observation: {part: value} for each part it
    records, and {part: why} for each part it records but cannot compare.
    `parts` is checks.json's observation.compared; a part this module cannot
    read is a KeyError (caught at load)."""
    res = obs.get('result') or {}
    state, bad = {}, {}
    for part in parts:
        got = READERS[part](res)
        if got is None:
            continue
        (state if got[0] == 'ok' else bad)[part] = got[1]
    return state, bad


LEASE_STATE = 'lease state'


def same_part(part, before, after):
    """Whether the later value of a part shows the same state as the earlier
    one. Equal values are; so is a workspace answer whose only difference is
    a lease state step time alone makes (checks.json lease_time_steps: the
    lease state is the host validity at query time), every other kept fact
    equal. Direction matters: `before` is the earlier observation."""
    if before == after:
        return True
    if part != 'workspace' or not _lease_steps:
        return False
    (bx, bf), (ax, af) = before, after
    if bx != 0 or ax != 0 or len(bf) != len(af):
        return False
    stepped = False
    for (bk, bv), (ak, av) in zip(bf, af):
        if (bk, bv) == (ak, av):
            continue
        if bk == ak == LEASE_STATE and (bv, av) in _lease_steps and not stepped:
            stepped = True
            continue
        return False
    return stepped


def show_part(state, part):
    v = state[0].get(part)
    if part in ('dirs', 'files'):
        return '%d entries' % len(v)
    return repr(v)[:120]


# --- refused_effect ----------------------------------------------------------

# The conditions effect.to may state, per kind of part. Each names a state
# the observation can show; there is deliberately no "exit non-zero": a
# crashed master or a wrong flag answers that too, so an effect must name
# the state by the answer it gives (its exit status AND its text).
RECORD_CONDITIONS = ('stderr_contains', 'text_contains')   # besides a field's exact value
LIST_CONDITIONS = ('has', 'lacks')


def effect_state(part, to, after):
    """Whether the observation `after` shows `part` in the state effect.to
    names: (mismatches, cannot). `mismatches` lists what the part shows
    instead ([] when it is in that state); `cannot` lists the conditions the
    observation cannot evaluate (a stderr not recorded, a part not recorded,
    a condition this reader does not accept), which leaves the refusal NOT
    JUDGED, never passed. The effect is a state that stays: what the part
    held before does not matter (another value, or already this one).
    `to` is an object of conditions on the part as the observation records it:
      a record part (head, workspace): each key a field and its exact value
        (e.g. "exit": 1), "stderr_contains": a phrase in result.<part>.stderr
        (the first lines of the command's stderr, a list or a string), and
        "text_contains": a phrase in result.<part>.text;
      a list part (snapshots, dirs, files): "has" and "lacks", entries the
        part holds and does not hold."""
    if not isinstance(to, dict) or not to:
        return [], ['effect.to %r is not an object of conditions' % (to,)]
    a = (after.get('result') or {}).get(part)
    out, cannot = [], []
    if part in ('dirs', 'files', 'snapshots'):
        if not isinstance(a, list):
            return [], ['%s not recorded as a list after it' % part]
        for key, val in to.items():
            if key not in LIST_CONDITIONS:
                cannot.append('%s: condition %r not accepted for a list part (%s)' % (part, key, ', '.join(LIST_CONDITIONS)))
            elif key == 'has' and sorted(set(val) - set(a)):
                out.append('%s lacks %s' % (part, sorted(set(val) - set(a))))
            elif key == 'lacks' and sorted(set(val) & set(a)):
                out.append('%s holds %s' % (part, sorted(set(val) & set(a))))
        return out, cannot
    if not isinstance(a, dict):
        return [], ['%s not recorded as a record after it (%r)' % (part, a)]
    for key, val in to.items():
        if key in RECORD_CONDITIONS:
            field = key[:-len('_contains')]
            got = a.get(field)
            if got is None:
                cannot.append('%s.%s not recorded' % (part, field))
                continue
            text = '\n'.join(got) if isinstance(got, list) else str(got)
            if val not in text:
                out.append('%s.%s does not contain %r: %r' % (part, field, val, text[:200]))
        elif key.endswith('_contains') or key == 'exit_nonzero':
            cannot.append('%s: condition %r not accepted (fields, %s)' % (part, key, ', '.join(RECORD_CONDITIONS)))
        elif key not in a:
            cannot.append('%s.%s not recorded' % (part, key))
        elif a.get(key) != val:
            out.append('%s.%s is %r, effect %r' % (part, key, a.get(key), val))
    return out, cannot


# --- C6 placement ----------------------------------------------------------

def place_by_status(inv, obs, base):
    """Placement of a snapshot, delete, move or restore from the observation
    after it (and, for a snapshot, the one before it): (outcome, why), the
    outcome 'took effect', 'did not', 'third', or None when not placed."""
    verb, args = inv['verb'], inv.get('args') or {}
    res = obs.get('result') or {}
    head, files = res.get('head'), res.get('files')
    err = (res.get('show_error') or {}).get('code') if isinstance(res.get('show_error'), dict) else None
    if verb == 'snapshot':
        # By HEAD's snapshot names when both observations record them; by the
        # root/ objects only when neither does.
        bres = base.get('result') or {}
        bnames, names = bres.get('snapshots'), res.get('snapshots')
        if isinstance(bnames, list) and isinstance(names, list):
            name = args.get('name')
            if name in bnames:
                return None, 'by snapshot names: %r already present before it (observation seq %d)' % (name, base['seq'])
            lost = sorted(set(bnames) - set(names))
            if lost:
                return 'third', 'by snapshot names: other snapshots lost: %s' % ', '.join(lost[:4])
            return ('took effect' if name in names else 'did not'), 'by snapshot names: %r %s' % (name, 'present' if name in names else 'absent')
        if isinstance(bnames, list) or isinstance(names, list):
            return None, 'snapshot names recorded by one observation only (seq %d, %d)' % (base['seq'], obs['seq'])
        bfiles = bres.get('files')
        if not isinstance(files, list) or not isinstance(bfiles, list):
            return None, 'no listing before and after (observations seq %d, %d)' % (base['seq'], obs['seq'])
        before = {f for f in bfiles if f.startswith('root/')}
        now = {f for f in files if f.startswith('root/')}
        if before - now:
            return 'third', 'by root objects: root objects lost: %s' % ', '.join(sorted(before - now)[:4])
        return ('took effect' if now - before else 'did not'), 'by root objects (no snapshot names recorded): roots %d -> %d' % (len(before), len(now))
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


# --- C7 --------------------------------------------------------------------

def after_delete_findings(vol, obs):
    """The volume's first observation after its ok delete shows HEAD in state
    deleted (the tombstone), and its directory holds exactly HEAD and VOLUME:
    any pool, seg, root or RETENTION object is a finding, and so is a missing
    HEAD (the tombstone keeps a later volume of the same name out of it)."""
    f = []
    res = obs.get('result') or {}
    head = res.get('head')
    if not isinstance(head, dict) or head.get('state') != 'deleted':
        f.append('%s after delete: ops chunk show answered %r, not HEAD in state deleted' % (vol, head or res.get('show_error')))
    files = res.get('files')
    if files is None:
        f.append('%s after delete: no listing (%s)' % (vol, res.get('listing_error', 'none recorded')))
        return f
    extra = sorted(set(files) - TOMBSTONE)
    if extra:
        f.append('%s after delete: objects left: %s' % (vol, ', '.join(extra[:8])))
    missing = sorted(TOMBSTONE - set(files))
    if missing:
        f.append('%s after delete: %s missing from its directory (the tombstone keeps the name out of it)' % (vol, ', '.join(missing)))
    return f


def recreate_finding(vol, comp):
    """In v1 the master keeps a deleted name's reservation: a create of that
    name after its ok delete is REFUSED (FailedPrecondition, the reservation's
    reason). A finding if it is not; None if it is. If a build lets it
    through, this goes red so that someone decides it."""
    detail = (comp.get('result') or {}).get('detail', '') if comp else ''
    if comp is None or comp['outcome'] != 'refused' or 'FailedPrecondition' not in detail or RESERVED_NAME not in detail:
        return ('%s created again after its delete: %s, not refused for the name''s reservation (FailedPrecondition, %r): %s'
                % (vol, comp['outcome'] if comp else 'no completion', RESERVED_NAME, detail or (comp.get('result') if comp else '')))
    return None


# --- faults ----------------------------------------------------------------

def process_of(inv):
    """The process a kill or restart names (args.process), as a string."""
    return str((inv.get('args') or {}).get('process', ''))


def holder_identity(healthy_obs):
    """{volume: (pid, start)} of each volume's holder process, from a healthy
    record's result.holders; empty when the record names none. A volume
    whose identity changes between two healthy records with no kill or
    restart reaching it had its holder restarted (DUR decides that)."""
    seen = (healthy_obs.get('result') or {}).get('holders') or {}
    return {vol: (h.get('pid'), h.get('start')) for vol, h in seen.items()}


def not_serving(healthy_obs):
    """The volumes a healthy record shows not serving (result.not_serving,
    each holder's own /status, which does not need the master), as a sorted
    list; None when the record does not say."""
    ns = (healthy_obs.get('result') or {}).get('not_serving')
    return sorted(ns) if isinstance(ns, list) else None


# The intended state a lifecycle operation answered ok sets on its volume: an
# attach leaves it held; a stop, a move or a delete not held. A move only
# hands the volume off (the source stops, the clean move is recorded) and
# nothing serves it until the next attach on the target host: the model's
# "moved" state is unattached. One answered unknown leaves the volume
# unsettled until a settled record places it.
LIFECYCLE = {'attach': 'held', 'stop': 'not_held', 'move': 'not_held', 'delete': 'not_held'}
# Of those, the verbs whose unknown is resolved by a settled record. A delete
# is placed by its own observation rule; the runner writes no settled record
# for it.
NEEDS_SETTLED = ('attach', 'stop', 'move')
SETTLED_STATES = ('held', 'not_held')


def intended_states(recs, upto_seq):
    """{volume: 'held' | 'not_held' | 'unsettled'} as the records before seq
    `upto_seq` leave it: a lifecycle operation's completion (ok: its state;
    unknown: unsettled; refused: no change), and a settled record (its
    state). A volume with neither is absent."""
    out, inv_of = {}, {}
    for r in recs:
        if r['seq'] >= upto_seq:
            break
        if r['kind'] == 'invoke':
            inv_of[r['seq']] = r
        elif r['kind'] == 'complete':
            inv = inv_of.get(r['of'])
            st = LIFECYCLE.get(inv['verb']) if inv else None
            if st is not None and r['outcome'] == 'ok':
                out[inv.get('volume')] = st
            elif st is not None and r['outcome'] == 'unknown':
                out[inv.get('volume')] = 'unsettled'
        else:
            got = settled(r)
            if got is not None:
                out[got[0]] = got[1]
    return out


def third_state(healthy_obs):
    """[(volume, why)] of each volume a healthy record shows in neither
    consistent state (result.third_state: a holder alive but not serving, or
    the holder gone while the workspace is still live past the bound)."""
    rows = (healthy_obs.get('result') or {}).get('third_state') or []
    return [(str(r.get('volume')), str(r.get('why', ''))) for r in rows if isinstance(r, dict)] if isinstance(rows, list) else [('?', 'third_state %r' % (rows,))]


def settled(obs):
    """(volume, state, evidence) of an observe record of verb settled whose
    state is one of SETTLED_STATES; None for any other record."""
    if obs.get('kind') != 'observe' or obs.get('verb') != 'settled':
        return None
    res = obs.get('result') or {}
    if res.get('state') not in SETTLED_STATES or not res.get('volume'):
        return None
    return res['volume'], res['state'], res.get('evidence')


# --- C4 (epoch fencing) ----------------------------------------------------
#
# The record shape C4 reads (the runner writes it from M3 on):
#   a write's (or read's) invoke: args.holder = {"server": "a-v1", "replica": "r1",
#       "epoch_before": 3, "pid": 4242, "start": "636310"}: the holder the
#       operation goes through, its status epoch and its process identity
#       (pid and start time), sampled BEFORE the invoke;
#   its complete: result.holder = {"epoch_after": 3, "pid": 4242, "start": "636310"}:
#       the same holder's status epoch and process identity sampled AFTER the
#       completion;
#   an observe record: result.line = {"replica": "r1", "epoch": 3}: the
#       master's line for the record's volume; a healthy record may carry
#       result.lines = {volume: {"replica", "epoch"}} for every volume;
#   the run's header (observe record of verb gen_header):
#       result.ack_modes = {volume: "acknowledging" | "best_effort"}.
# Without result.line, an observe record of verb observe whose workspace
# answer exited 0 gives the line from its "replica:" and "epoch:" facts, so
# today's observations already carry it.

ACK_MODES = ('acknowledging', 'best_effort')


def _epoch(v):
    return v if isinstance(v, int) and not isinstance(v, bool) and v >= 0 else None


def _ident(h):
    """(pid, start) of a holder sample, or None when it records neither."""
    if h.get('pid') is None and h.get('start') is None:
        return None
    return (h.get('pid'), h.get('start'))


def holder_sample(inv, comp):
    """The holder an operation went through, its status epochs and process
    identities: None when the invoke carries no args.holder; ('bad', why)
    when the sample is there but unreadable; else ('ok', (server, replica),
    epoch_before, epoch_after, ident_before, ident_after), epoch_after and
    ident_after None when the completion carries no result.holder (an
    operation that never completed, or a runner that did not sample), an
    identity None when the sample records no pid and start."""
    h = (inv.get('args') or {}).get('holder')
    if h is None:
        return None
    if not isinstance(h, dict) or not h.get('server') or not h.get('replica') or _epoch(h.get('epoch_before')) is None:
        return 'bad', 'args.holder %r is not {server, replica, epoch_before, pid, start}' % (h,)
    after = ident_after = None
    if comp is not None:
        ch = (comp.get('result') or {}).get('holder')
        if ch is not None:
            if not isinstance(ch, dict) or _epoch(ch.get('epoch_after')) is None:
                return 'bad', 'result.holder %r is not {epoch_after, pid, start}' % (ch,)
            after, ident_after = ch['epoch_after'], _ident(ch)
    return 'ok', (str(h['server']), str(h['replica'])), h['epoch_before'], after, _ident(h), ident_after


def ack_modes(recs):
    """({volume: mode}, [notes]): each volume's acknowledgement mode from the
    run's header (the first gen_header record's result.ack_modes); a value
    other than ACK_MODES is noted and left out (judged as acknowledging)."""
    for r in recs:
        if r['kind'] == 'observe' and r.get('verb') == 'gen_header':
            modes = (r.get('result') or {}).get('ack_modes')
            if modes is None:
                return {}, []
            if not isinstance(modes, dict):
                return {}, ['header ack_modes %r is not {volume: mode}' % (modes,)]
            out, notes = {}, []
            for vol, m in sorted(modes.items()):
                if m in ACK_MODES:
                    out[str(vol)] = m
                else:
                    notes.append('header ack_modes: %s has mode %r, not one of %s' % (vol, m, ', '.join(ACK_MODES)))
            return out, notes
    return {}, []


def _line(row):
    if isinstance(row, dict) and row.get('replica') and _epoch(row.get('epoch')) is not None:
        return str(row['replica']), row['epoch']
    return None


def child_exit(obs):
    """(op_seq, 'ok' | 'failed') of an observe record of verb child_exit: the
    abandoned child of the unknown operation invoked at op_seq exited, and
    the write landed at or before this record (ok) or never (failed); None
    for any other record."""
    if obs.get('kind') != 'observe' or obs.get('verb') != 'child_exit':
        return None
    res = obs.get('result') or {}
    op, out = res.get('op_seq'), res.get('outcome')
    if not isinstance(op, int) or isinstance(op, bool) or out not in ('ok', 'failed'):
        return None
    return op, out


def disconnect(obs):
    """The volume whose guest device an observe record of verb disconnect
    shows disconnected (result.volume, else the record's volume); None for
    any other record."""
    if obs.get('kind') != 'observe' or obs.get('verb') != 'disconnect':
        return None
    vol = (obs.get('result') or {}).get('volume') or obs.get('volume')
    return str(vol) if vol else None


def master_lines(obs):
    """{volume: (replica, epoch)}: the master's line for each volume an
    observe record shows; empty for a record that shows none (a generator
    record, a failed workspace answer, a volume without a lease)."""
    if obs.get('kind') != 'observe' or str(obs.get('verb', '')).startswith('gen_'):
        return {}
    res = obs.get('result') or {}
    out = {}
    if isinstance(res.get('lines'), dict):
        for vol, row in res['lines'].items():
            got = _line(row)
            if got:
                out[str(vol)] = got
    vol = obs.get('volume')
    if not vol:
        return out
    got = _line(res.get('line'))
    if got:
        out[vol] = got
        return out
    ws = res.get('workspace')
    if obs.get('verb') == 'observe' and isinstance(ws, dict) and ws.get('exit') == 0:
        facts = workspace_facts(ws.get('text'))
        if facts:
            d = dict(facts)
            rep, ep = d.get('replica'), d.get('epoch', '')
            if rep and rep != 'none' and ep.isdigit():
                out[vol] = (rep, int(ep))
    return out


def kill_scope(inv):
    """The volumes a kill reaches on the data path: a holder kill names its
    volume in args.volume (the record's own volume stays ""), and reaches
    that volume only; a kill of the master reaches no holder, so no volume;
    a kill naming no volume (an older history, or a process the runner could
    not map) reaches every volume, None."""
    args = inv.get('args') or {}
    if args.get('volume'):
        return {args['volume']}
    if _master is not None and args.get('process') == _master:
        return set()
    return None
