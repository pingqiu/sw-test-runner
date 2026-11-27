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
# checks.json placement: {verb: row}, how an unknown of the verb is placed by
# a list of names (the one definition the driver's rerun reads too).
_placement = {}
PLACEMENT_BY = ('snapshot_names',)
PLACEMENT_CASES = ('present_before', 'others_lost', 'present', 'absent')
PLACEMENT_OUTCOMES = {'took_effect': 'took effect', 'did_not': 'did not', 'third': 'third', 'not_placed': None}
PLACEMENT_KEYS = {'verb', 'by', 'part', 'arg', 'outcomes', 'cite', 'what'}
# checks.json observation.time_derived: {field: (fact, validity, steps)}, a
# workspace fact derived from another fact and the clock, and the (earlier,
# later) values time alone may step it through.
_time_derived = {}


def placement_rows(checks):
    """{verb: row} of checks.json placement, refusing a row outside the
    closed vocabulary: another key, another by, a case missing or added, an
    outcome outside the enumeration, a second row of a verb."""
    out = {}
    for row in checks.get('placement', []):
        extra = set(row) - PLACEMENT_KEYS
        if extra:
            raise ValueError('placement row %r: keys %s outside the vocabulary' % (row.get('verb'), ', '.join(sorted(extra))))
        if row.get('by') not in PLACEMENT_BY:
            raise ValueError('placement row %r: by %r; the one way to place is %s' % (row.get('verb'), row.get('by'), PLACEMENT_BY[0]))
        outcomes = row.get('outcomes') or {}
        if set(outcomes) != set(PLACEMENT_CASES):
            raise ValueError('placement row %r: cases %s, want %s' % (row.get('verb'), sorted(outcomes), list(PLACEMENT_CASES)))
        bad = [o for o in outcomes.values() if o not in PLACEMENT_OUTCOMES]
        if bad:
            raise ValueError('placement row %r: outcomes %s outside %s' % (row.get('verb'), bad, sorted(PLACEMENT_OUTCOMES)))
        if not row.get('part') or not row.get('arg') or row.get('verb') in out:
            raise ValueError('placement row %r: its part and arg, one row per verb' % row.get('verb'))
        out[row['verb']] = row
    return out


def bind(checks):
    """Take the product facts this module reads from checks.json."""
    global _master, _time_derived, _placement
    masters = [t['name'] for t in checks.get('fault_targets', []) if t.get('role') == 'master']
    _master = masters[0] if masters else None
    _placement = placement_rows(checks)
    rows = (checks.get('observation') or {}).get('time_derived') or []
    _time_derived = {r['field']: (r['fact'], r['validity'], frozenset(tuple(x) for x in r['steps'])) for r in rows}


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


class Workspace(tuple):
    """A workspace answer's compared value, (exit, facts): equal as that tuple.
    `clock` carries what is kept but not compared: the dropped fields, by
    name, and the observation's reading window (from_ns, at_ns); a
    time_derived step reads them (same_part)."""
    clock = None


def workspace_clock(text):
    """The dropped line's fields {name: int}, or {} when it is absent."""
    lines = text.splitlines() if isinstance(text, str) else text
    for line in lines or ():
        if isinstance(line, str) and WORKSPACE_DROPPED['lease expiry_ns'].fullmatch(line):
            nums = re.findall(r'-?\d+', line)
            return dict(zip(WORKSPACE_DROPPED_FIELDS, map(int, nums)))
    return {}


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
    v = Workspace((0, facts))
    v.clock = workspace_clock(ws.get('text'))
    return 'ok', v


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
        if isinstance(got[1], Workspace):
            frm, at = obs.get('from_mono_ns'), obs.get('mono_ns')
            got[1].clock = dict(got[1].clock, from_ns=frm if isinstance(frm, int) else None,
                                at_ns=at if isinstance(at, int) else None)
        (state if got[0] == 'ok' else bad)[part] = got[1]
    return state, bad


def time_explains(key, bv, av, before, after):
    """Whether time alone steps a time_derived fact from bv to av: a listed
    step, its fact equal in both observations, and the earlier validity not
    larger than the longest time between the two readings (the later
    observation's end minus the earlier one's start)."""
    if key not in _time_derived:
        return False
    fact, validity, steps = _time_derived[key]
    if (bv, av) not in steps:
        return False
    bc, ac = before.clock or {}, after.clock or {}
    if bc.get(fact) is None or bc.get(fact) != ac.get(fact):
        return False
    if bc.get(validity) is None or bc.get('from_ns') is None or ac.get('at_ns') is None:
        return False
    return bc[validity] <= ac['at_ns'] - bc['from_ns']


def same_part(part, before, after):
    """Whether the later value of a part shows the same state as the earlier
    one. Equal values are; so is a workspace answer whose only difference is
    one step of a time_derived fact that time alone explains (checks.json
    observation.time_derived), every other kept fact equal. Direction
    matters: `before` is the earlier observation."""
    if before == after:
        return True
    if part != 'workspace' or not isinstance(before, Workspace) or not isinstance(after, Workspace):
        return False
    (bx, bf), (ax, af) = before, after
    if bx != 0 or ax != 0 or len(bf) != len(af):
        return False
    stepped = False
    for (bk, bv), (ak, av) in zip(bf, af):
        if (bk, bv) == (ak, av):
            continue
        if bk == ak and not stepped and time_explains(bk, bv, av, before, after):
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

def place_by_names(row, name, bnames, names, base):
    """C6's placement by a placement row (by snapshot_names): the case the
    two lists of names show, mapped by the row to its outcome."""
    lost = sorted(set(bnames) - set(names))
    if name in bnames:
        case, why = 'present_before', 'by snapshot names: %r already present before it (observation seq %d)' % (name, base['seq'])
    elif lost:
        case, why = 'others_lost', 'by snapshot names: other snapshots lost: %s' % ', '.join(lost[:4])
    elif name in names:
        case, why = 'present', 'by snapshot names: %r present' % name
    else:
        case, why = 'absent', 'by snapshot names: %r absent' % name
    return PLACEMENT_OUTCOMES[row['outcomes'][case]], why


def place_by_status(inv, obs, base):
    """Placement of a snapshot, delete, move or restore from the observation
    after it (and, for a snapshot, the one before it): (outcome, why), the
    outcome 'took effect', 'did not', 'third', or None when not placed."""
    verb, args = inv['verb'], inv.get('args') or {}
    res = obs.get('result') or {}
    head, files = res.get('head'), res.get('files')
    err = (res.get('show_error') or {}).get('code') if isinstance(res.get('show_error'), dict) else None
    if verb == 'snapshot':
        # By HEAD's snapshot names when both observations record them (the
        # checks.json placement row: the one definition); by the root/
        # objects only when neither does (code, here alone).
        bres = base.get('result') or {}
        row = _placement.get(verb)
        part = row['part'] if row else 'snapshots'
        bnames, names = bres.get(part), res.get(part)
        if row and isinstance(bnames, list) and isinstance(names, list):
            return place_by_names(row, args.get(row['arg']), bnames, names, base)
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
