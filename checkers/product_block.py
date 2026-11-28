"""The block product's reading code for the history checkers.

history_check.py is generic: it loads this module by file, named in the
product directory's checks.json (reader.module), and calls only the names
below. Everything here reads what the block product shows: HEAD, its
snapshot names, the control plane's workspace answer, the volume's
directories and files, the tombstone a delete leaves, and the process names
of a kill. Numbers and names a page states live in checks.json, not here.

  bind(checks)                  receives checks.json once, at load
  observed_state(obs, parts, wall_fit=None)
                                an observation's comparable parts
  wall_fit(observations)        a history's wall clock minus its own clock,
                                fitted from the leases' printed facts, for
                                observations without from_wall_ns (or None)
  releases(recs)                [(invoke seq, complete seq, source, target)] of
                                each delete that releases a restore target
                                from its source (checks.json release_on_delete)
  same_part(part, before, after, restart_wall_ns=None, released=0)
                                whether a later value shows the same state
                                (across a master restart at that wall time:
                                checks.json observation.across_master_restart)
  expiry_rule(part, before, after, restart_wall_ns=None)
                                which across_master_restart rule compared a
                                pair's expiry, or None
  compared_note(counts)         C5's note of the pairs each rule compared
  bind_product_dir(dir)         reads the description's actions.json (the verbs and record args)
  followup_calls(followup)      the invocations a store pause's follow-up makes: [(verb, volume, {arg: value})]
  observation_findings(obs)     what one observation contradicts in itself
                                (a lease printed expired while still valid)
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
import json
import os
import re
import statistics

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
PLACEMENT_BY = ('list_member',)
PLACEMENT_CASES = ('present_before', 'others_lost', 'present', 'absent')
PLACEMENT_OUTCOMES = {'took_effect': 'took effect', 'did_not': 'did not', 'third': 'third', 'not_placed': None}
PLACEMENT_KEYS = {'verb', 'by', 'field', 'arg', 'outcomes', 'cite', 'what'}
# checks.json observation.across_master_restart: {fact: row}, a lease fact
# compared while the lease does not renew: it may move later, never earlier,
# within a ceiling (the row's ttl, slack, factor and skew, in ns).
_restart_allow = {}
RESTART_ALLOW_KEYS = {'fact', 'state_field', 'states', 'ttl_seconds', 'slack_seconds', 'cite', 'why',
                      'no_restart_ttl_factor', 'no_restart_cite', 'validity', 'expired_state', 'skew_seconds', 'skew_cite'}
RESTART_STATE_KEYS = {'state', 'renews', 'cite'}
# checks.json observation.release_on_delete: the row, or None. A delete of
# a restore target releases it from its source's dependents, which moves the
# source's HEAD generation and nothing it observes.
_release = None
RELEASE_KEYS = {'verb', 'outcomes', 'restored_by', 'restored_outcomes', 'source_arg', 'part', 'moves', 'per_release', 'cite', 'why'}
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
        if not row.get('field') or not row.get('arg') or row.get('verb') in out:
            raise ValueError('placement row %r: its field and arg, one row per verb' % row.get('verb'))
        out[row['verb']] = row
    return out


def bind(checks):
    """Take the product facts this module reads from checks.json."""
    global _master, _time_derived, _placement, _restart_allow, _release
    masters = [t['name'] for t in checks.get('fault_targets', []) if t.get('role') == 'master']
    _master = masters[0] if masters else None
    _placement = placement_rows(checks)
    rows = (checks.get('observation') or {}).get('time_derived') or []
    _time_derived = {r['field']: (r['fact'], r['validity'], frozenset(tuple(x) for x in r['steps'])) for r in rows}
    _release = None
    rel = (checks.get('observation') or {}).get('release_on_delete')
    if rel is not None:
        if (set(rel) != RELEASE_KEYS or rel['part'] != 'head' or rel['moves'] != 'gen' or not isinstance(rel['per_release'], int)
                or isinstance(rel['per_release'], bool) or rel['per_release'] <= 0 or not rel['cite']
                or not set(rel['outcomes']) <= {'ok', 'unknown'} or not rel['outcomes'] or rel['restored_outcomes'] != ['ok']):
            raise ValueError('release_on_delete: keys %s, part head, moves gen, per_release a positive int, outcomes within ok and unknown, '
                             'restored_outcomes ["ok"], a cite' % sorted(RELEASE_KEYS))
        _release = rel
    _restart_allow = {}
    for r in (checks.get('observation') or {}).get('across_master_restart') or []:
        extra = set(r) - RESTART_ALLOW_KEYS
        if extra or not isinstance(r.get('ttl_seconds'), int) or not isinstance(r.get('slack_seconds'), int) or r['ttl_seconds'] <= 0 or r['slack_seconds'] < 0:
            raise ValueError('across_master_restart row %r: keys %s outside %s, or a ttl/slack that is not a non-negative int' % (r.get('fact'), sorted(extra), sorted(RESTART_ALLOW_KEYS)))
        factor, skew = r.get('no_restart_ttl_factor'), r.get('skew_seconds')
        if (not isinstance(factor, (int, float)) or isinstance(factor, bool) or factor <= 0 or not r.get('no_restart_cite')
                or not isinstance(skew, int) or isinstance(skew, bool) or skew <= 0 or not r.get('skew_cite')
                or not r.get('validity') or not r.get('expired_state')):
            raise ValueError('across_master_restart row %r: its no_restart_ttl_factor (a positive number) and no_restart_cite, '
                             'its validity field, its expired_state, its skew_seconds (a positive int) and skew_cite' % r.get('fact'))
        states = r.get('states')
        if not r.get('state_field') or not isinstance(states, list) or not states:
            raise ValueError('across_master_restart row %r: its state_field and its states' % r.get('fact'))
        renews = {}
        for st in states:
            if set(st) - RESTART_STATE_KEYS or not isinstance(st.get('renews'), bool) or not st.get('state') or not st.get('cite') or st['state'] in renews:
                raise ValueError('across_master_restart row %r: state %r: {state, renews (true or false), cite}, once each' % (r.get('fact'), st))
            renews[st['state']] = st['renews']
        if r['expired_state'] not in renews or renews[r['expired_state']]:
            raise ValueError('across_master_restart row %r: expired_state %r is not one of its states that does not renew' % (r.get('fact'), r['expired_state']))
        _restart_allow[r['fact']] = {
            'ttl': r['ttl_seconds'] * 10**9, 'slack': r['slack_seconds'] * 10**9, 'field': r['state_field'], 'renews': renews,
            'extend': int(round(factor * r['ttl_seconds'] * 10**9)), 'validity': r['validity'],
            'expired': r['expired_state'], 'skew': skew * 10**9, 'skew_cite': r['skew_cite']}


# The description's actions (product/actions.json): {action: (verb, each,
# {record arg: the param its template names, or ITEM})}. An action with
# "each" records one invocation per item of that param ({{item}}).
_actions = {}
ITEM = object()


def bind_product_dir(product_dir):
    """Read the actions of the description next to checks.json."""
    global _actions
    _actions = {}
    p = os.path.join(product_dir, 'actions.json')
    if not os.path.exists(p):
        return
    with open(p, encoding='utf-8') as fh:
        doc = json.load(fh)
    for a in doc.get('actions', []):
        args = {}
        for k, v in ((a.get('record') or {}).get('args') or {}).items():
            t = v.get('template') if isinstance(v, dict) else v
            m = re.fullmatch(r'\{\{(\w+)\}\}', t) if isinstance(t, str) else None
            if m:
                args[k] = ITEM if m.group(1) == 'item' else m.group(1)
        _actions[a['action']] = (a.get('verb'), a.get('each'), args)


def followup_calls(fu):
    """The invocations a store pause's follow-up {action, volume, params}
    makes, as the description records them: one per item of its "each"
    param (a read or a write: one per lba), else one; each (verb, volume,
    {record arg: the value the follow-up's param gives it}). None when the
    action is not in the description (the follow-up cannot be matched)."""
    a = _actions.get(fu.get('action'))
    if a is None or not a[0]:
        return None
    verb, each, args = a
    params = fu.get('params') or {}
    items = str(params.get(each, '')).split() if each else [None]
    out = []
    for item in items:
        want = {}
        for k, src in args.items():
            if src is ITEM:
                want[k] = item
            elif src in params:
                want[k] = str(params[src])
        out.append((verb, fu.get('volume') or '', want))
    return out


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
    name, the observation's reading window (from_ns, at_ns) and the
    reading's end in wall time (end_wall_ns, None when neither the record
    nor a fit gives it); a time_derived step and the lease rule read them
    (same_part)."""
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


def _int(v):
    return v if isinstance(v, int) and not isinstance(v, bool) else None


def reading_end_wall(obs, wall_fit=None):
    """The reading's end in wall time (ns): from_wall_ns + (mono_ns -
    from_mono_ns) when the record carries both starts; else mono_ns + the
    history's fitted offset (wall_fit) when there is one; else None."""
    fw, frm, at = _int(obs.get('from_wall_ns')), _int(obs.get('from_mono_ns')), _int(obs.get('mono_ns'))
    if fw is not None and frm is not None and at is not None:
        return fw + (at - frm)
    if wall_fit is not None and at is not None:
        return at + wall_fit
    return None


def wall_fit(observations):
    """For a history before from_wall_ns: its wall clock minus its own clock
    (ns), fitted from what the leases print. While a lease's validity at
    query lies strictly between 0 and the TTL it is not clamped, and the
    product computes it as expiry - skew/2 - now (checks.json
    across_master_restart validity, skew_cite): now = expiry - skew/2 -
    validity. The median over those observations of now - mono_ns (the
    record's stamp, just after the reading); None with no row, a history that
    records from_wall_ns (it needs no fit), or no such observation."""
    if not _restart_allow:
        return None
    row = next(iter(_restart_allow.values()))
    fact = next(iter(_restart_allow))
    offs = []
    for obs in observations:
        if _int(obs.get('from_wall_ns')) is not None:
            return None
        ws = (obs.get('result') or {}).get('workspace')
        if not isinstance(ws, dict) or ws.get('exit') != 0:
            continue
        c = workspace_clock(ws.get('text'))
        e, v, at = c.get(fact), c.get(row['validity']), _int(obs.get('mono_ns'))
        if e is None or v is None or at is None or not 0 < v < row['ttl']:
            continue
        offs.append(e - row['skew'] // 2 - v - at)
    return statistics.median_low(offs) if offs else None


def observed_state(obs, parts, wall_fit=None):
    """The comparable state of an observation: {part: value} for each part it
    records, and {part: why} for each part it records but cannot compare.
    `parts` is checks.json's observation.compared; a part this module cannot
    read is a KeyError (caught at load). wall_fit: the history's fitted wall
    offset (wall_fit()), used only when the record has no from_wall_ns."""
    res = obs.get('result') or {}
    state, bad = {}, {}
    for part in parts:
        got = READERS[part](res)
        if got is None:
            continue
        if isinstance(got[1], Workspace):
            got[1].clock = dict(got[1].clock, from_ns=_int(obs.get('from_mono_ns')), at_ns=_int(obs.get('mono_ns')),
                                end_wall_ns=reading_end_wall(obs, wall_fit))
        (state if got[0] == 'ok' else bad)[part] = got[1]
    return state, bad


def observation_findings(obs):
    """What one observation contradicts in itself: a lease printed in its
    expired state (the row's expired_state) whose printed expiry is after
    the reading's END in wall time + skew/2. Expired means no validity is
    left, validity = expiry - skew/2 - now (the row's skew_cite), so at the
    query expiry <= now + skew/2 <= the reading's end + skew/2. Needs the
    record's from_wall_ns: an observation without it is not checked (the
    fit is derived from the same printed validity, so it cannot judge it)."""
    end = reading_end_wall(obs)
    if end is None or not _restart_allow:
        return []
    ws = (obs.get('result') or {}).get('workspace')
    if not isinstance(ws, dict) or ws.get('exit') != 0:
        return []
    facts = workspace_facts(ws.get('text'))
    if facts is None:
        return []
    out = []
    for fact, row in _restart_allow.items():
        e = workspace_clock(ws.get('text')).get(fact)
        if dict(facts).get(row['field']) != row['expired'] or e is None:
            continue
        if e > end + row['skew'] // 2:
            out.append('observation seq %s of %s: lease state %s, but its printed expiry %d is %.3f s after the reading\'s end in wall time %d + skew/2 (%d s): '
                       'the lease was still valid at the query (%s)' % (obs.get('seq'), obs.get('volume') or '(no volume)', row['expired'], e,
                                                                       (e - end - row['skew'] // 2) / 1e9, end, row['skew'] // 2 // 10**9, row['skew_cite']))
    return out


def restart_expiry(before, after, restart_wall_ns):
    """The expiry of a lease in a state that does not renew (the row's
    states, renews false) in both observations (checks.json
    across_master_restart), keyed on the lease STATE, step or no step: True
    when it holds, False when it does not. It is MONOTONE: equal or later,
    never earlier. Across a master restart at restart_wall_ns it is at most
    the restart's wall time + ttl + slack (a lease the restart left never
    expiring is one that never takes its step). Across no restart (or a
    restart that records no wall time in a history that has the field) it
    is at most the later reading's end in wall time + no_restart_ttl_factor
    x ttl + slack (the extender's horizon); with no end in wall time (no
    from_wall_ns and no fit) that ceiling is not checked, the order still
    is. None when it is not compared: no row, a renewing state at either
    observation (granted), a state the row does not list, a value not
    recorded, or a master restart between in a history that records no
    restart's wall time at all (restart_wall_ns 'unrecorded': a history
    from before the field)."""
    if not isinstance(before, Workspace) or not isinstance(after, Workspace) or restart_wall_ns == 'unrecorded':
        return None
    for fact, row in _restart_allow.items():
        bs, as_ = dict(before[1]).get(row['field']), dict(after[1]).get(row['field'])
        if row['renews'].get(bs, True) or row['renews'].get(as_, True):
            return None
        b, a = (before.clock or {}).get(fact), (after.clock or {}).get(fact)
        if b is None or a is None:
            return None
        if a < b:
            return False
        if restart_wall_ns is not None:
            return a <= restart_wall_ns + row['ttl'] + row['slack']
        end = (after.clock or {}).get('end_wall_ns')
        return end is None or a <= end + row['extend'] + row['slack']
    return None


EXPIRY_RULES = ('across a master restart', 'no restart')
NOT_COMPARED = 'not compared'


def expiry_rule(part, before, after, restart_wall_ns=None):
    """Which rule of checks.json across_master_restart compared the expiry
    of this pair (restart_expiry not None): 'across a master restart' (a
    restart with its wall time between) or 'no restart'. NOT_COMPARED when
    the row had a lease to look at (both workspace answers print the row's
    state field) but did not compare it (a renewing state, a value missing,
    an unrecorded restart); None when there was no lease to look at."""
    if part != 'workspace' or not isinstance(before, Workspace) or not isinstance(after, Workspace) or not _restart_allow:
        return None
    if not all(dict(v[1]).get(row['field']) is not None for v in (before, after) for row in _restart_allow.values()):
        return None
    if restart_expiry(before, after, restart_wall_ns) is None:
        return NOT_COMPARED
    return EXPIRY_RULES[0] if restart_wall_ns is not None else EXPIRY_RULES[1]


def compared_note(counts):
    """C5's note line: {rule: pairs} of expiry_rule, each pair once; the
    total compared, and of it the pairs across a master restart. Zero is
    printed as zero: a history whose leases the row looked at and compared
    none says so (NOT_COMPARED pairs are not in the total)."""
    return 'lease expiry pairs compared: %d (%s: %d)' % (sum(counts.get(r, 0) for r in EXPIRY_RULES), EXPIRY_RULES[0], counts.get(EXPIRY_RULES[0], 0))


def time_explains(key, bv, av, before, after, fact_compared=False):
    """Whether time alone steps a time_derived fact from bv to av: a listed
    step, its fact equal in both observations (or already compared across a
    master restart and found within its bound: fact_compared,
    restart_expiry), and the earlier validity not larger than the longest
    time between the two readings (the later observation's end minus the
    earlier one's start)."""
    if key not in _time_derived:
        return False
    fact, validity, steps = _time_derived[key]
    if (bv, av) not in steps:
        return False
    bc, ac = before.clock or {}, after.clock or {}
    if bc.get(fact) is None or (bc.get(fact) != ac.get(fact) and not fact_compared):
        return False
    if bc.get(validity) is None or bc.get('from_ns') is None or ac.get('at_ns') is None:
        return False
    return bc[validity] <= ac['at_ns'] - bc['from_ns']


def releases(recs):
    """[(invoke seq, complete seq, source, target)] of each delete
    (release_on_delete verb, an outcome it lists) of a volume the history
    shows restored from a source (restored_by completed ok, its source arg):
    the delete releases the target from that source's dependents. An ok
    delete ends the target; a later volume of its name is not that
    restore's target."""
    if _release is None:
        return []
    inv_of, source, out = {}, {}, []
    for r in recs:
        if r['kind'] == 'invoke':
            inv_of[r['seq']] = r
            continue
        if r['kind'] != 'complete' or r.get('of') not in inv_of:
            continue
        inv = inv_of[r['of']]
        vol = inv.get('volume')
        if inv['verb'] == _release['restored_by'] and r['outcome'] in _release['restored_outcomes']:
            src = (inv.get('args') or {}).get(_release['source_arg'])
            if src:
                source[vol] = src
        elif inv['verb'] == _release['verb'] and r['outcome'] in _release['outcomes'] and source.get(vol):
            out.append((inv['seq'], r['seq'], source[vol], vol))
            if r['outcome'] == 'ok':
                del source[vol]
    return out


def released_head(before, after, released):
    """A HEAD whose only difference is its generation moved LATER by at most
    `released` x per_release (release_on_delete): `released` is the number
    of deletes of restore targets between the two observations (ok and
    unknown). Epoch, holder and state equal."""
    if _release is None or not released:
        return False
    if not (isinstance(before, tuple) and isinstance(after, tuple) and len(before) == len(after) == 5 and before[0] == after[0] == 'head'):
        return False
    b, a = before[1], after[1]
    return before[2:] == after[2:] and isinstance(b, int) and isinstance(a, int) and b < a <= b + released * _release['per_release']


def same_part(part, before, after, restart_wall_ns=None, released=0):
    """Whether the later value of a part shows the same state as the earlier
    one. Equal values are; so is a workspace answer whose only difference is
    one step of a time_derived fact that time alone explains (checks.json
    observation.time_derived), every other kept fact equal. Direction
    matters: `before` is the earlier observation. restart_wall_ns is the wall
    time of a master restart between the two (None: none, or not recorded).
    While the lease is in a state that does not renew, its expiry IS
    compared (restart_expiry), step or no step. released: the restore
    targets of this volume released between the two (releases): its HEAD
    generation may move later by that many (released_head), nothing else."""
    if part == (_release or {}).get('part') and before != after:
        return released_head(before, after, released)
    across = restart_expiry(before, after, restart_wall_ns) if part == 'workspace' else None
    if across is False:
        return False
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
        if bk == ak and not stepped and time_explains(bk, bv, av, before, after, across is True):
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
    """C6's placement by a placement row (by list_member): the case the
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
        part = row['field'] if row else 'snapshots'
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
