"""The seam between the generic checkers and the product description.

history_check.py names no product word outside comments and docstrings; the
product's facts come from its description directory (checks.json) and its
reader module. Each guard ships with a red control.
"""
import io
import json
import os
import re
import subprocess
import sys
import tokenize
import unittest
from pathlib import Path

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

    def test_every_ignored_field_says_why(self):
        for row in self.checks['observation']['ignored']:
            self.assertTrue(row.get('field') and row.get('why'), row)

    def test_the_checker_reads_the_tables_from_the_data(self):
        self.assertEqual(hc.RECOVERY, {r['term']: (r['seconds'], r['cite']) for r in self.checks['recovery']})
        self.assertEqual(hc.BOUNDS, {r['verb']: (r['seconds'], r['cite']) for r in self.checks['bounds']})
        self.assertEqual(hc.STATE_PARTS, tuple(self.checks['observation']['compared']))
        self.assertEqual(hc.PRODUCT.__name__, self.checks['reader']['module'])


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
