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
        self.assertEqual(self.checks['read_at'], 'seaweed-block 14dff69b0f7cc139f9bde7849dec6ab34063c947')

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

    def lease(self, state, **swap):
        lines = [('lease state: %s' % state) if x.startswith('lease state: ') else x for x in REAL_WS]
        for old, new in swap.items():
            lines = [x.replace(old, new) for x in lines]
        return hc.observed_state(ws_obs(lines))

    def same(self, a, b):
        diff, _ = hc.state_diff(a, b)
        self.assertIsNotNone(diff)
        return diff == []

    def test_revoking_to_expired_by_time_compares_equal(self):
        # From a host run: a stopped volume's lease ran out, validity reaching 0.
        expired = self.lease('expired', **{'host validity_ns at query: 14849887534': 'host validity_ns at query: 0'})
        self.assertTrue(self.same(self.lease('revoking'), expired))

    def test_red_other_lease_steps_compare_unequal(self):
        for a, b in (('expired', 'revoking'), ('granted', 'revoking'), ('revoking', 'granted'),
                     ('granted', 'expired'), ('expired', 'granted')):
            self.assertFalse(self.same(self.lease(a), self.lease(b)), (a, b))

    def test_red_revoking_to_expired_with_another_change_compares_unequal(self):
        before = self.lease('revoking')
        for swap in ({'epoch: 1': 'epoch: 2'}, {'holder: r1@1': 'holder: r2@1'}, {'replica: r1': 'replica: r2'},
                     {'recorded ACK: none': 'recorded ACK: r1@1 HEAD generation 4'}):
            self.assertFalse(self.same(before, self.lease('expired', **swap)), swap)
        moved = hc.observed_state(ws_obs([x.replace('granted', 'expired') for x in REAL_WS] + ['binding: host=b data=d ctrl=c retired=false']))
        self.assertFalse(self.same(before, moved))

    def test_the_allowed_steps_are_the_data(self):
        checks = json.loads((PRODUCT / 'checks.json').read_text(encoding='utf-8'))
        row = checks['observation']['lease_time_steps']
        self.assertEqual(row['steps'], [['revoking', 'expired']])
        self.assertEqual(row['cite'], 'docs/volume-lifecycle.md:35')
        # with no step in the data, the same pair is a change
        with mock.patch.object(hc.PRODUCT, '_lease_steps', frozenset()):
            self.assertFalse(self.same(self.lease('revoking'), self.lease('expired')))

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


if __name__ == '__main__':
    unittest.main()
