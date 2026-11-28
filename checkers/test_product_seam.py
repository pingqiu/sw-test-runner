"""The seam between the generic checkers and the product description.

history_check.py names no product word outside comments and docstrings; the
product's facts come from its description directory (checks.json) and its
reader module. Each guard ships with a red control.
"""
import hashlib
import io
import json
import os
import re
import subprocess
import sys
import tokenize
import unittest
from pathlib import Path
from unittest import mock

import history_check as hc

HERE = Path(__file__).resolve().parent
PRODUCT = HERE.parent / 'packs' / 'v3block' / 'product'

# Words of the block product. Matched against each word of an identifier or a
# string (split at anything not a letter): the lowercase ones in any case,
# the uppercase ones exactly (the history's own field 'volume' is generic).
ANY_CASE = {'head', 'snapshots', 'workspace', 'dirs', 'tombstone', 'blockvolume', 'blockmaster'}
EXACT = {'HEAD', 'VOLUME'}


def product_words(source):
    """(line, token) of every product word in code: comments and docstrings
    (a string that is a statement of its own) are skipped."""
    hits = []
    toks = list(tokenize.generate_tokens(io.StringIO(source).readline))
    skip = {tokenize.NL, tokenize.NEWLINE, tokenize.INDENT, tokenize.DEDENT, tokenize.COMMENT}
    prev = None   # the previous significant token
    for i, t in enumerate(toks):
        if t.type in skip:
            if t.type in (tokenize.NEWLINE, tokenize.INDENT, tokenize.DEDENT):
                prev = t
            continue
        if t.type == tokenize.STRING:
            nxt = next((u for u in toks[i + 1:] if u.type not in (tokenize.NL, tokenize.COMMENT)), None)
            statement = (prev is None or prev.type in (tokenize.NEWLINE, tokenize.INDENT, tokenize.DEDENT)) \
                and (nxt is None or nxt.type in (tokenize.NEWLINE, tokenize.ENDMARKER))
            if statement:   # a docstring, or a bare string statement
                prev = t
                continue
        if t.type in (tokenize.NAME, tokenize.STRING):
            for w in re.findall(r'[A-Za-z]+', t.string):
                if w.lower() in ANY_CASE or w in EXACT:
                    hits.append((t.start[0], t.string))
        prev = t
    return hits


class GuardGeneric(unittest.TestCase):
    def test_history_check_names_no_product_word(self):
        src = (HERE / 'history_check.py').read_text(encoding='utf-8')
        self.assertEqual(product_words(src), [], 'history_check.py must read product facts only through the product directory')

    def test_red_the_guard_catches_a_product_word_in_code(self):
        for seeded in ("x = res.get('head')\n", "TOMBSTONE = {1}\n", "y = 'blockmaster'\n", "z = {'HEAD'}\n",
                       "def f():\n    '''doc'''\n    return r['dirs']\n", "a = b.snapshots_error\n"):
            self.assertTrue(product_words(seeded), seeded)

    def test_comments_docstrings_and_generic_words_pass(self):
        ok = ('"""HEAD and the tombstone, in a docstring."""\n'
              "def f(r):\n    '''the head of it'''\n    return r['volume']   # HEAD, blockmaster\n"
              "v = 'ahead'\n")
        self.assertEqual(product_words(ok), [])


def numbers_without_cite(node, path='$'):
    """Paths of every object in the JSON that states a number with no cite."""
    out = []
    if isinstance(node, dict):
        if any(isinstance(v, (int, float)) and not isinstance(v, bool) for v in node.values()) and not node.get('cite'):
            out.append(path)
        for k, v in node.items():
            out += numbers_without_cite(v, '%s.%s' % (path, k))
    elif isinstance(node, list):
        for i, v in enumerate(node):
            out += numbers_without_cite(v, '%s[%d]' % (path, i))
    return out


class Data(unittest.TestCase):
    def setUp(self):
        self.checks = json.loads((PRODUCT / 'checks.json').read_text(encoding='utf-8'))

    def test_header(self):
        self.assertEqual(self.checks['product'], 'block')
        # The pin moves with the product; every description file names the same one.
        self.assertRegex(self.checks['read_at'], r'^seaweed-block [0-9a-f]{40}$')
        for name in ('operations.json', 'outcomes.json', 'actions.json'):
            other = json.loads((PRODUCT / name).read_text(encoding='utf-8'))
            self.assertEqual(other['read_at'], self.checks['read_at'], name)

    def test_every_number_has_a_cite(self):
        self.assertEqual(numbers_without_cite(self.checks), [])

    def test_red_a_number_without_a_cite_is_caught(self):
        self.assertEqual(numbers_without_cite({'recovery': [{'term': 'x', 'seconds': 5}]}), ['$.recovery[0]'])
        self.assertEqual(numbers_without_cite({'bounds': [{'verb': 'write', 'seconds': 1.5, 'cite': ''}]}), ['$.bounds[0]'])

    def test_every_fault_target_has_a_cite(self):
        self.assertTrue(self.checks['fault_targets'])
        for row in self.checks['fault_targets']:
            self.assertTrue(row.get('cite'), row)

    def test_red_a_fault_target_without_a_cite_is_caught(self):
        self.checks['fault_targets'][0].pop('cite')
        with self.assertRaises(AssertionError):
            self.test_every_fault_target_has_a_cite()

    def test_zero_is_one_block_of_zeros(self):
        n = self.checks['block_bytes']['bytes']
        self.assertTrue(self.checks['block_bytes']['cite'])
        self.assertEqual(hc.ZERO, hashlib.sha256(bytes(n)).hexdigest())
        self.assertEqual(hc.ZERO, hashlib.sha256(b'\0' * 4096).hexdigest(), 'block: 4096 bytes')

    def test_required_parts_are_compared_parts(self):
        self.assertEqual(hc.REQUIRED_PARTS, tuple(self.checks['observation']['required']))
        self.assertLessEqual(set(hc.REQUIRED_PARTS), set(hc.STATE_PARTS))

    def test_every_ignored_field_says_why(self):
        for row in self.checks['observation']['ignored']:
            self.assertTrue(row.get('field') and row.get('why'), row)

    def test_the_checker_reads_the_tables_from_the_data(self):
        self.assertEqual(hc.RECOVERY, {r['term']: (r['seconds'], r['cite']) for r in self.checks['recovery']})
        self.assertEqual(hc.BOUNDS, {r['verb']: (r['seconds'], r['cite']) for r in self.checks['bounds']})
        self.assertEqual(hc.STATE_PARTS, tuple(self.checks['observation']['compared']))
        self.assertEqual(hc.PRODUCT.__name__, self.checks['reader']['module'])


class Reader(unittest.TestCase):
    def test_holder_identity_reads_the_healthy_record(self):
        obs = {'result': {'healthy': True, 'holders': {'v1': {'pid': 7, 'start': 'a'}, 'v2': {'pid': 9}}}}
        self.assertEqual(hc.PRODUCT.holder_identity(obs), {'v1': (7, 'a'), 'v2': (9, None)})
        self.assertEqual(hc.PRODUCT.holder_identity({'result': {'healthy': True}}), {})
        self.assertEqual(hc.PRODUCT.holder_identity({}), {})


REAL_WS = ['volume: v01 (query snapshot, not authority to serve)', 'replica: r1', 'epoch: 1', 'lease state: granted',
           'holder: r1@1 (logical replica)', 'lease expiry_ns: 1795738167151366324; host validity_ns at query: 14849887534',
           'recorded ACK: none', 'host: unknown (not recorded by the master); issued moves and bindings below are separate facts']


def ws_obs(lines, exit_=0):
    return {'result': {'workspace': {'exit': exit_, 'text': lines}}}


class Workspace(unittest.TestCase):
    """The workspace part keeps the stable facts and drops the two that change
    on their own (from a host run: two queries with nothing between them)."""

    def part(self, lines, exit_=0):
        state, bad = hc.observed_state(ws_obs(lines, exit_))
        return state.get('workspace'), bad.get('workspace')

    def test_a_real_pair_compares_equal(self):
        later = [x.replace('14849887534', '14716527984') for x in REAL_WS]
        self.assertNotEqual(later, REAL_WS)
        a, b = self.part(REAL_WS), self.part(later)
        self.assertIsNone(a[1])
        self.assertEqual(a, b)
        # a renewal moves the expiry too: still equal
        renewed = [x.replace('1795738167151366324', '1795738177151366324') for x in later]
        self.assertEqual(self.part(renewed), a)

    def test_red_a_change_of_lease_state_epoch_or_holder_compares_unequal(self):
        base = self.part(REAL_WS)[0]
        for old, new in (('lease state: granted', 'lease state: revoking'), ('epoch: 1', 'epoch: 2'),
                         ('holder: r1@1 (logical replica)', 'holder: r2@1 (logical replica)'),
                         ('recorded ACK: none', 'recorded ACK: r1@1 HEAD generation 4')):
            changed = [new if x == old else x for x in REAL_WS]
            got, bad = self.part(changed)
            self.assertIsNone(bad, old)
            self.assertNotEqual(got, base, old)
        # a move line issued since is a fact too
        moved = REAL_WS + ['move: request=v01-move-1 kind=clean issued=true a/r1@1 -> b/r1@2 HEAD generation 7 remote LSN 9']
        self.assertNotEqual(self.part(moved)[0], base)

    def test_parsed_by_key_not_by_position(self):
        shuffled = REAL_WS[:1] + REAL_WS[5:6] + REAL_WS[1:5] + REAL_WS[6:]
        got, bad = self.part(shuffled)
        self.assertIsNone(bad)
        self.assertNotIn('lease expiry_ns', dict(got[1]))
        self.assertEqual(dict(got[1])['lease state'], 'granted')

    def test_a_garbled_text_is_not_judged(self):
        for garbled in (['volume v01 granted'],                                          # no key: value
                        REAL_WS[:3] + ['lease state granted'] + REAL_WS[4:],            # one line without its key
                        [x.replace('host validity_ns at query: 14849887534', 'validity: soon') for x in REAL_WS],  # dropped line of another shape
                        REAL_WS[:2] + REAL_WS[3:],                                      # a fact always printed is missing
                        'not a list of lines at all: {',
                        None):
            got, bad = self.part(garbled)
            self.assertIsNone(got, garbled)
            self.assertTrue(bad, garbled)
        # and a pair with one garbled side is not judged, never equal
        diff, why = hc.state_diff(hc.observed_state(ws_obs(REAL_WS)), hc.observed_state(ws_obs(['garbled'])))
        self.assertIsNone(diff)
        self.assertIn('workspace', why)

    def test_a_failed_show_is_compared_as_recorded(self):
        self.assertEqual(self.part([''], 1), ((1, ('',)), None))
        self.assertNotEqual(self.part([''], 1)[0], self.part([''], 3)[0])

    # From the rc.2 night (seed 2): v01's holder killed, its lease never
    # renewed; one observation began at FROM1 and was stamped at AT1 with
    # validity V1, the next, stamped at AT2, shows expired, the expiry unchanged.
    EXPIRY, V1, FROM1, AT1, AT2 = 1795738167151366324, 1857068441, 51400000000, 51499141249, 53428294450

    def lease(self, state, validity=None, expiry=None, frm=None, at=None, **swap):
        lines = [('lease state: %s' % state) if x.startswith('lease state: ') else x for x in REAL_WS]
        line = 'lease expiry_ns: %d; host validity_ns at query: %d' % (expiry or self.EXPIRY, self.V1 if validity is None else validity)
        lines = [line if x.startswith('lease expiry_ns: ') else x for x in lines]
        for old, new in swap.items():
            lines = [x.replace(old, new) for x in lines]
        obs = ws_obs(lines)
        if frm is not None:
            obs['from_mono_ns'] = frm
        if at is not None:
            obs['mono_ns'] = at
        return hc.observed_state(obs)

    def before(self, state='granted', **kw):
        return self.lease(state, **dict(dict(frm=self.FROM1, at=self.AT1), **kw))

    def after(self, state='expired', **kw):
        return self.lease(state, **dict(dict(validity=0, frm=self.AT1 + 1000, at=self.AT2), **kw))

    def same(self, a, b):
        diff, _ = hc.state_diff(a, b)
        self.assertIsNotNone(diff)
        return diff == []

    def test_a_lease_running_out_by_time_compares_equal(self):
        for state in ('granted', 'revoking'):
            self.assertTrue(self.same(self.before(state), self.after()), state)

    def test_the_window_is_the_later_end_minus_the_earlier_start(self):
        # A validity larger than the gap between the two stamps, but not than
        # the readings' window: each reading lies inside its observation.
        gap = self.AT2 - self.AT1
        self.assertTrue(self.same(self.before(validity=gap + 1), self.after()))
        self.assertTrue(self.same(self.before(validity=self.AT2 - self.FROM1), self.after()))

    def test_red_a_validity_longer_than_the_window_is_a_change(self):
        # a forced expiry: the lease had time left that no clock could use up
        for state in ('granted', 'revoking'):
            self.assertFalse(self.same(self.before(state, validity=self.AT2 - self.FROM1 + 1), self.after()), state)

    def test_red_a_changed_expiry_is_a_change(self):
        # granted renews: a step needs its expiry equal. revoking does not
        # renew: its expiry is monotone (checks.json across_master_restart),
        # so an earlier one is a change and a later one is not (no ceiling
        # here: neither from_wall_ns nor a fit is given).
        for d in (1, -1):
            self.assertFalse(self.same(self.before('granted'), self.after(expiry=self.EXPIRY + d)), d)
        self.assertFalse(self.same(self.before('revoking'), self.after(expiry=self.EXPIRY - 1)))
        self.assertTrue(self.same(self.before('revoking'), self.after(expiry=self.EXPIRY + 1)))

    def test_red_time_does_not_run_backwards(self):
        # expired -> granted with the expiry unchanged, and every other unlisted step
        for a, b in (('expired', 'granted'), ('expired', 'revoking'), ('granted', 'revoking'), ('revoking', 'granted')):
            self.assertFalse(self.same(self.before(a, validity=0), self.after(b, validity=self.V1)), (a, b))

    def test_red_an_observation_without_its_start_gives_no_bound(self):
        self.assertFalse(self.same(self.lease('granted', at=self.AT1), self.after()))
        self.assertFalse(self.same(self.before(), self.lease('expired', validity=0, frm=self.AT1 + 1000)))

    def test_red_a_time_step_with_another_change_is_a_change(self):
        before = self.before('revoking')
        for swap in ({'epoch: 1': 'epoch: 2'}, {'holder: r1@1': 'holder: r2@1'}, {'replica: r1': 'replica: r2'},
                     {'recorded ACK: none': 'recorded ACK: r1@1 HEAD generation 4'}):
            self.assertFalse(self.same(before, self.after(**swap)), swap)
        lines = [x.replace('granted', 'expired') for x in REAL_WS] + ['binding: host=b data=d ctrl=c retired=false']
        moved = dict(ws_obs(lines), from_mono_ns=self.AT1 + 1000, mono_ns=self.AT2)
        self.assertFalse(self.same(before, hc.observed_state(moved)))

    def test_the_time_derived_fields_are_the_data(self):
        checks = json.loads((PRODUCT / 'checks.json').read_text(encoding='utf-8'))
        rows = checks['observation']['time_derived']
        self.assertEqual([(r['field'], r['fact'], r['validity'], r['steps']) for r in rows],
                         [('lease state', 'lease expiry_ns', 'host validity_ns at query', [['granted', 'expired'], ['revoking', 'expired']])])
        self.assertIn('cmd/sw-block/workspace_show.go:54-58', rows[0]['cite'])
        # with no row in the data, the same pair is a change
        with mock.patch.object(hc.PRODUCT, '_time_derived', {}):
            self.assertFalse(self.same(self.before(), self.after()))

    def test_the_dropped_fields_are_listed_as_ignored(self):
        checks = json.loads((PRODUCT / 'checks.json').read_text(encoding='utf-8'))
        ignored = {r['field'] for r in checks['observation']['ignored']}
        for f in hc.PRODUCT.WORKSPACE_DROPPED_FIELDS:
            self.assertIn('workspace: ' + f, ignored)


class ProductFlag(unittest.TestCase):
    """--product names the description directory; without it, block."""

    def run_checker(self, *extra):
        hist = HERE / 'testdata' / 'm0-history.jsonl'
        if not hist.exists():
            hist = sorted((HERE / 'testdata').glob('*.jsonl'))[0]
        p = subprocess.run([sys.executable, '-I', str(HERE / 'history_check.py'), str(hist)] + list(extra),
                           capture_output=True, text=True, cwd=str(HERE.parent.parent))
        return p.returncode, p.stdout, p.stderr

    def test_default_and_explicit_block_agree_under_isolated_mode(self):
        a = self.run_checker()
        b = self.run_checker('--product', str(PRODUCT))
        self.assertEqual(a, b)
        self.assertEqual(a[2], '', a)

    def test_red_a_missing_product_directory_is_an_error(self):
        rc, _, err = self.run_checker('--product', str(HERE / 'testdata' / 'no-such-product'))
        self.assertNotEqual(rc, 0)
        self.assertIn('checks.json', err)



class PlacementRow(unittest.TestCase):
    """The placement table in checks.json is a closed vocabulary: one way to
    place (by list_member), four cases, each mapped to one of four fixed
    outcomes. product_block reads the names branch of C6's snapshot rule from
    it; a row outside the vocabulary does not load (red controls)."""

    def rows(self):
        return json.loads((PRODUCT / 'checks.json').read_text(encoding='utf-8'))

    def test_the_row_loads_and_places_the_names_branch(self):
        import product_block as pb
        rows = pb.placement_rows(self.rows())
        self.assertEqual(set(rows), {'snapshot'})
        row = rows['snapshot']
        base = {'seq': 1}
        self.assertEqual(pb.place_by_names(row, 's1', [], ['s1'], base)[0], 'took effect')
        self.assertEqual(pb.place_by_names(row, 's1', [], [], base)[0], 'did not')
        self.assertEqual(pb.place_by_names(row, 's1', ['s0'], ['s1'], base)[0], 'third')
        self.assertIsNone(pb.place_by_names(row, 's1', ['s1'], ['s1'], base)[0])

    def test_red_a_row_outside_the_vocabulary_does_not_load(self):
        import copy
        import product_block as pb
        good = self.rows()
        for name, seed in {
            'another by': lambda r: r.update(by='snapshot_names'),
            'another outcome': lambda r: r['outcomes'].update(absent='maybe'),
            'a case missing': lambda r: r['outcomes'].pop('present'),
            'a case added': lambda r: r['outcomes'].update(gone='third'),
            'another key': lambda r: r.update(when='always'),
        }.items():
            checks = copy.deepcopy(good)
            seed(checks['placement'][0])
            with self.assertRaises(ValueError, msg=name):
                pb.placement_rows(checks)


if __name__ == '__main__':
    unittest.main()
