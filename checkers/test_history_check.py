"""Controls of history_check: the green history of a real M0 run, and one seeded
defect per checker that it must catch (and that no other checker reports)."""
import copy
import json
import unittest
from pathlib import Path

import history_check as hc

FIXTURE = Path(__file__).resolve().parent / 'testdata' / 'm0-history.jsonl'


def load():
    return [json.loads(l) for l in FIXTURE.read_text(encoding='utf-8').splitlines() if l.strip()]


def dump(recs):
    return [json.dumps(r) for r in recs]


def renumber(recs):
    """Sequence numbers and completion links after records were removed or added."""
    old_to_new = {}
    for i, r in enumerate(recs, 1):
        old_to_new[r['seq']] = i
    for i, r in enumerate(recs, 1):
        r['seq'] = i
        if r['kind'] == 'complete':
            r['of'] = old_to_new[r['of']]
    return recs


def ops(recs, verb, volume=None):
    """(invoke, complete) pairs of a verb, in order."""
    comp = {r['of']: r for r in recs if r['kind'] == 'complete'}
    return [(r, comp.get(r['seq'])) for r in recs if r['kind'] == 'invoke' and r['verb'] == verb and (volume is None or r['volume'] == volume)]


def verdicts(recs, **kw):
    return {k: v['verdict'] for k, v in hc.check(dump(recs), **kw).items()}


class Green(unittest.TestCase):
    def test_the_real_run_passes_c0_to_c3(self):
        v = verdicts(load())
        for name in ('C0', 'C1', 'C2', 'C3'):
            self.assertEqual(v[name], 'PASS', (name, hc.check(dump(load()))[name]))


class Reader(unittest.TestCase):
    def assert_unknown_everywhere(self, lines):
        for name, v in hc.check(lines).items():
            self.assertEqual(v['verdict'], 'UNKNOWN', name)

    def test_a_gap_a_torn_line_and_a_clock_going_back_are_unknown(self):
        recs = load()
        gap = dump(recs[:10] + recs[11:])
        torn = dump(recs)[:-1] + [dump(recs)[-1][:20]]
        back = copy.deepcopy(recs)
        back[5]['mono_ns'] = back[4]['mono_ns'] - 1
        for lines in (gap, torn, dump(back)):
            self.assert_unknown_everywhere(lines)

    def test_a_double_or_orphan_completion_is_unknown(self):
        recs = load()
        inv, comp = ops(recs, 'write')[0]
        double = copy.deepcopy(recs) + [dict(comp, seq=len(recs) + 1, mono_ns=recs[-1]['mono_ns'])]
        self.assert_unknown_everywhere(dump(double))
        orphan = copy.deepcopy(recs)
        orphan.append({'seq': len(orphan) + 1, 'kind': 'complete', 'of': 9999, 'op': 'X.Y', 'verb': 'write', 'mono_ns': orphan[-1]['mono_ns'], 'outcome': 'ok'})
        self.assert_unknown_everywhere(dump(orphan))


class C0(unittest.TestCase):
    def test_an_invocation_without_its_completion(self):
        recs = load()
        _, comp = ops(recs, 'snapshot')[0]
        recs = renumber([r for r in recs if r is not comp])
        v = hc.check(dump(recs))
        self.assertEqual(v['C0']['verdict'], 'FAIL')
        self.assertIn('no completion: snapshot', v['C0']['findings'][0])

    def test_an_unknown_outcome(self):
        recs = load()
        ops(recs, 'restore')[0][1]['outcome'] = 'unknown'
        self.assertEqual(verdicts(recs)['C0'], 'FAIL')

    def test_a_refusal_fails_unless_expected(self):
        recs = load()
        ops(recs, 'release')[0][1]['outcome'] = 'refused'
        self.assertEqual(verdicts(recs)['C0'], 'FAIL')
        self.assertEqual(verdicts(recs, expected_refusals=('release',))['C0'], 'PASS')


class C1(unittest.TestCase):
    def test_a_lost_acknowledged_write(self):
        recs = load()
        # The last read of v1's lba 0 returns the r1 content after r2 and r3
        # were acknowledged: the store lost acknowledged writes.
        first_w = ops(recs, 'write', 'v1')[0][0]
        last_r = ops(recs, 'read', 'v1')[-1]
        lba0 = [p for p in ops(recs, 'read', 'v1') if p[0]['args']['lba'] == first_w['args']['lba']][-1]
        lba0[1]['result']['sha256'] = first_w['args']['sha256']
        v = hc.check(dump(recs))
        self.assertEqual(v['C1']['verdict'], 'FAIL')
        self.assertTrue(last_r)

    def test_a_written_block_never_read_afterwards(self):
        recs = load()
        # Drop the final reads of v2: its t1 writes are read only there, so
        # they are never read back (and v2 is deleted afterwards, which
        # excuses nothing).
        finals = {id(x) for p in ops(recs, 'read', 'v2')[-3:] for x in p}
        recs = renumber([r for r in recs if id(r) not in finals])
        v = hc.check(dump(recs))
        self.assertEqual(v['C1']['verdict'], 'FAIL')
        self.assertTrue(any('never read afterwards' in f for f in v['C1']['findings']))

    def test_an_unknown_write_keeps_both_values_and_nothing_else(self):
        recs = load()
        w_inv, w_comp = ops(recs, 'write', 'v1')[-1]   # the r3 write of some lba
        w_comp['outcome'] = 'unknown'
        lba = w_inv['args']['lba']
        later = [p for p in ops(recs, 'read', 'v1') if p[0]['args']['lba'] == lba and p[0]['seq'] > w_comp['seq']]
        r_comp = later[-1][1]
        prev = [p[0] for p in ops(recs, 'write', 'v1') if p[0]['args']['lba'] == lba][-2]
        r_comp['result']['sha256'] = prev['args']['sha256']    # the earlier value: allowed
        self.assertEqual(verdicts(recs)['C1'], 'PASS')
        r_comp['result']['sha256'] = 'f' * 64                  # anything else: not allowed
        self.assertEqual(verdicts(recs)['C1'], 'FAIL')


class C2(unittest.TestCase):
    def test_a_target_holding_a_write_made_after_the_snapshot(self):
        recs = load()
        # The target's first read of an lba returns the source's r2 content,
        # written after s1 completed.
        r2 = [p[0] for p in ops(recs, 'write', 'v1') if p[0]['args']['round'] == 'r2'][0]
        first = [p for p in ops(recs, 'read', 'v2') if p[0]['args']['lba'] == r2['args']['lba']][0]
        first[1]['result']['sha256'] = r2['args']['sha256']
        v = hc.check(dump(recs))
        self.assertEqual(v['C2']['verdict'], 'FAIL')
        self.assertIn('first read', v['C2']['findings'][0])

    def test_a_write_during_the_snapshot_may_be_in_it(self):
        recs = load()
        # Move one r2 write so it is invoked and completed while s1 is being
        # taken: the snapshot may or may not hold it, so the target's first
        # read may return it (the window rule), but no later write.
        s_inv, s_comp = ops(recs, 'snapshot', 'v1')[0]
        w_inv, w_comp = [p for p in ops(recs, 'write', 'v1') if p[0]['args']['round'] == 'r2'][0]
        rest = [r for r in recs if r is not w_inv and r is not w_comp]
        at = rest.index(s_comp)
        moved = [dict(w_inv, mono_ns=s_inv['mono_ns']), dict(w_comp, mono_ns=s_inv['mono_ns'])]
        recs = renumber(rest[:at] + moved + rest[at:])
        lba = w_inv['args']['lba']
        first = [p for p in ops(recs, 'read', 'v2') if p[0]['args']['lba'] == lba][0]
        first[1]['result']['sha256'] = w_inv['args']['sha256']
        self.assertEqual(verdicts(recs)['C2'], 'PASS', hc.check(dump(recs))['C2'])

    def test_no_read_of_the_target_is_unknown_not_pass(self):
        recs = load()
        drop = {id(x) for p in ops(recs, 'read', 'v2') for x in p}
        recs = renumber([r for r in recs if id(r) not in drop])
        self.assertEqual(verdicts(recs)['C2'], 'UNKNOWN')


class C7(unittest.TestCase):
    def after_delete(self, recs, vol):
        at = ops(recs, 'delete', vol)[0][1]['seq']
        return [r for r in recs if r['kind'] == 'observe' and r.get('volume') == vol and r['seq'] > at][0]

    def test_the_real_run_passes(self):
        self.assertEqual(verdicts(load())['C7'], 'PASS', hc.check(dump(load()))['C7'])

    def test_a_pool_object_left_behind(self):
        recs = load()
        self.after_delete(recs, 'v1')['result']['files'].append('pool/0/0-' + 'a' * 64)
        v = hc.check(dump(recs))
        self.assertEqual(v['C7']['verdict'], 'FAIL')
        self.assertIn('objects left', v['C7']['findings'][0])

    def test_the_tombstone_missing(self):
        recs = load()
        obs = self.after_delete(recs, 'v2')
        obs['result']['files'].remove('HEAD')
        self.assertEqual(verdicts(recs)['C7'], 'FAIL')
        obs['result']['files'].append('HEAD')
        obs['result'].pop('head')
        obs['result']['show_error'] = {'exit': 1, 'code': 'volume_not_found'}
        self.assertEqual(verdicts(recs)['C7'], 'FAIL', 'absent instead of the tombstone')

    def test_a_create_of_the_same_name_reusing_the_identity(self):
        recs = load()
        old = ops(recs, 'delete', 'v1')[0][0]['args']['layout_id']
        mono = recs[-1]['mono_ns']
        n = len(recs)
        recs += [{'seq': n + 1, 'kind': 'invoke', 'client': 'c1', 'op': 'VolumeCreate.MarkReady', 'verb': 'create', 'volume': 'v1', 'args': {'size_bytes': 16777216}, 'mono_ns': mono},
                 {'seq': n + 2, 'kind': 'complete', 'of': n + 1, 'client': 'c1', 'op': 'VolumeCreate.MarkReady', 'verb': 'create', 'volume': 'v1', 'mono_ns': mono, 'outcome': 'ok', 'result': {'layout_id': old}}]
        self.assertEqual(verdicts(recs)['C7'], 'FAIL')
        recs[-1]['result']['layout_id'] = 'b' * 32
        self.assertEqual(verdicts(recs)['C7'], 'PASS')

    def test_a_delete_never_observed(self):
        recs = load()
        obs = self.after_delete(recs, 'v1')
        recs = renumber([r for r in recs if r is not obs])
        self.assertEqual(verdicts(recs)['C7'], 'FAIL')


class C3(unittest.TestCase):
    def test_a_source_write_showing_in_the_target(self):
        recs = load()
        r3 = [p[0] for p in ops(recs, 'write', 'v1') if p[0]['args']['round'] == 'r3'][0]
        after = [p for p in ops(recs, 'read', 'v2') if p[0]['args']['lba'] == r3['args']['lba'] and p[0]['seq'] > r3['seq']]
        after[0][1]['result']['sha256'] = r3['args']['sha256']
        v = hc.check(dump(recs))
        self.assertEqual(v['C3']['verdict'], 'FAIL')

    def test_a_target_write_showing_in_the_source(self):
        recs = load()
        t1 = [p[0] for p in ops(recs, 'write', 'v2') if p[0]['args']['round'] == 't1'][0]
        after = [p for p in ops(recs, 'read', 'v1') if p[0]['args']['lba'] == t1['args']['lba'] and p[0]['seq'] > t1['seq']]
        after[0][1]['result']['sha256'] = t1['args']['sha256']
        v = hc.check(dump(recs))
        self.assertEqual(v['C3']['verdict'], 'FAIL')
        self.assertIn('write to the target', v['C3']['findings'][0])

    def test_a_direction_not_exercised_is_unknown(self):
        recs = load()
        drop = {id(x) for p in ops(recs, 'write', 'v2') for x in p}
        recs = renumber([r for r in recs if id(r) not in drop])
        self.assertEqual(verdicts(recs)['C3'], 'UNKNOWN')


if __name__ == '__main__':
    unittest.main()
