"""Controls of C4 (epoch fencing): green histories and one seeded defect per
rule that it must catch."""
import json
import unittest

import history_check as hc
import product_block as pb
from test_history_check import dump, load, ws_text

A = 'a' * 64


def history(*steps):
    """A history from steps:
      ('w' | 'r', volume, server, replica, epoch_before, epoch_after, outcome[, during])
        a write or read through that holder; `during` is a list of line steps
        recorded between its invoke and its complete;
      ('line', volume, replica, epoch)  an observe record of the master's line;
      ('plain', volume)  a write that carries no holder sample.
    Closed by the end record; one clock, 1 ns per record."""
    recs = []

    def add(r):
        r['seq'] = r['mono_ns'] = len(recs) + 1
        recs.append(r)

    def line(vol, rep, ep):
        add({'kind': 'observe', 'client': 'runner', 'verb': 'observe', 'volume': vol, 'result': {'line': {'replica': rep, 'epoch': ep}}})

    for st in steps:
        if st[0] == 'line':
            line(*st[1:])
            continue
        if st[0] == 'plain':
            add({'kind': 'invoke', 'op': 'X.Y', 'verb': 'write', 'volume': st[1], 'args': {'lba': 0, 'sha256': A}})
            add({'kind': 'complete', 'of': recs[-1]['seq'], 'op': 'X.Y', 'verb': 'write', 'volume': st[1], 'outcome': 'ok', 'result': {}})
            continue
        kind, vol, server, rep, before, after, outcome = st[:7]
        verb = 'write' if kind == 'w' else 'read'
        args = {'lba': 0, 'holder': {'server': server, 'replica': rep, 'epoch_before': before}}
        if verb == 'write':
            args['sha256'] = A
        add({'kind': 'invoke', 'op': 'X.Y', 'verb': verb, 'volume': vol, 'args': args})
        of = recs[-1]['seq']
        for d in (st[7] if len(st) > 7 else []):
            line(*d[1:])
        res = {'holder': {'epoch_after': after}}
        if verb == 'read':
            res['sha256'] = A
        add({'kind': 'complete', 'of': of, 'op': 'X.Y', 'verb': verb, 'volume': vol, 'outcome': outcome, 'result': res})
    add({'kind': 'end'})
    return recs


def c4(*steps):
    return hc.check(dump(history(*steps)))['C4']


class C4(unittest.TestCase):
    def assert_c4(self, want, *steps, contains=None):
        v = c4(*steps)
        self.assertEqual(v['verdict'], want, v)
        if contains:
            self.assertTrue(any(contains in x for x in v['findings']), v)
        return v

    def test_listed(self):
        self.assertIs(hc.CHECKERS['C4'], hc.c4)

    # --- green ---------------------------------------------------------------

    def test_one_holder_at_a_steady_epoch_passes(self):
        v = self.assert_c4('PASS', ('line', 'v1', 'r1', 1),
                           ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                           ('r', 'v1', 'a-v1', 'r1', 1, 1, 'ok'), ('line', 'v1', 'r1', 1))
        self.assertEqual(v['findings'], ['writes judged: 2 (either: 0, not acknowledged: 0)', 'stale reads: 0'])

    def test_a_hand_over_passes(self):
        # A at e1; the line moves to B at e2; B acknowledges at e2.
        self.assert_c4('PASS', ('line', 'v1', 'r1', 1),
                       ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                       ('line', 'v1', 'r2', 2),
                       ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'),
                       ('r', 'v1', 'b-v1', 'r2', 2, 2, 'ok'))

    def test_a_write_open_while_the_line_moves_is_not_fenced(self):
        # The line at e2 is observed after A's write was invoked: the write
        # may have been served before the move, rule 2 does not apply.
        self.assert_c4('PASS', ('line', 'v1', 'r1', 1),
                       ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok', [('line', 'v1', 'r2', 2)]),
                       ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'))

    def test_an_unacknowledged_write_is_not_judged(self):
        v = self.assert_c4('PASS', ('line', 'v1', 'r1', 1), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                           ('line', 'v1', 'r2', 2), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'unknown'),
                           ('w', 'v1', 'a-v1', 'r1', 1, 1, 'refused'))
        self.assertIn('writes judged: 1 (either: 0, not acknowledged: 2)', v['findings'])

    def test_volumes_are_judged_apart(self):
        # One epoch number on two volumes, two holders: no conflict.
        self.assert_c4('PASS', ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'), ('w', 'v2', 'a-v2', 'r1', 1, 1, 'ok'))

    # --- red controls --------------------------------------------------------

    def test_red_two_holders_acknowledging_at_one_epoch(self):
        self.assert_c4('FAIL', ('line', 'v1', 'r1', 1),
                       ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'), ('w', 'v1', 'b-v1', 'r2', 1, 1, 'ok'),
                       contains='two holders at one epoch: v1 epoch 1 acknowledged writes through a-v1/r1 (write seq 2), b-v1/r2 (write seq 4)')
        # the same server with another replica is another holder
        self.assert_c4('FAIL', ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'), ('w', 'v1', 'a-v1', 'r2', 1, 1, 'ok'),
                       contains='two holders at one epoch')

    def test_red_a_write_acknowledged_below_an_observed_line(self):
        self.assert_c4('FAIL', ('line', 'v1', 'r1', 1), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                       ('line', 'v1', 'r2', 2),
                       ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                       contains='write acknowledged below the line: write seq 5 v1 through a-v1/r1 at epoch 1, the line r2@2 observed before its invoke (seq 4)')

    def test_red_either_overlapping_a_hand_over_is_not_blamed(self):
        # B's status epoch steps 1 -> 2 while its write is open, because the
        # hand-over to B lands during it; A acknowledged at e1 before. Taking
        # B's epoch as the one before (1) would blame two holders at e1, and
        # rule 2 against the line r2@2 observed before B's invoke would blame
        # a write below the line. B may have been served at e2, where no other
        # holder acknowledged and no line is above it: not blamed.
        v = self.assert_c4('PASS', ('line', 'v1', 'r1', 1),
                           ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                           ('line', 'v1', 'r2', 2),
                           ('w', 'v1', 'b-v1', 'r2', 1, 2, 'ok'))
        self.assertIn('writes judged: 2 (either: 1, not acknowledged: 0)', v['findings'])
        # The same either write IS blamed when every epoch it may have had is
        # taken by another holder (A at e1 and C at e2 both acknowledged) ...
        self.assert_c4('FAIL', ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'), ('w', 'v1', 'c-v1', 'r3', 2, 2, 'ok'),
                       ('w', 'v1', 'b-v1', 'r2', 1, 2, 'ok'),
                       contains='write seq 5 v1 through b-v1/r2 at epoch either 1..2, every epoch taken by another holder: 1: a-v1/r1 (write seq 1); 2: c-v1/r3 (write seq 3)')
        # ... or when a line above both of its epochs was observed before it.
        self.assert_c4('FAIL', ('line', 'v1', 'r3', 3), ('w', 'v1', 'b-v1', 'r2', 1, 2, 'ok'),
                       contains='through b-v1/r2 at epoch either 1..2, the line r3@3')

    def test_red_a_stale_read_is_reported_not_failed(self):
        v = self.assert_c4('PASS', ('line', 'v1', 'r1', 1), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                           ('line', 'v1', 'r2', 2), ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'),
                           ('r', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                           contains='stale read: read seq 7 v1 answered by a-v1/r1 at epoch 1 after the line r2@2 was observed (seq 4)')
        self.assertIn('stale reads: 1', v['findings'])

    def test_red_not_exercised(self):
        for steps in ([('plain', 'v1')], [('w', 'v1', 'a-v1', 'r1', 1, 1, 'unknown')], []):
            v = self.assert_c4('FAIL', *steps, contains='C4 not exercised')
        # the real M0 run records no holder samples
        self.assertIn('C4 not exercised', hc.check(dump(load()))['C4']['findings'][0])

    def test_a_malformed_sample_is_not_judged(self):
        recs = history(('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'), ('w', 'v1', 'b-v1', 'r2', 1, 1, 'ok'))
        recs[3]['result']['holder'] = {'epoch_after': 'one'}
        v = hc.check(dump(recs))['C4']
        self.assertEqual(v['verdict'], 'PASS', v)
        self.assertTrue(any(x.startswith('not judged: write seq 3') for x in v['findings']), v)


class C4Reader(unittest.TestCase):
    def test_the_line_from_todays_workspace_answer(self):
        obs = {'kind': 'observe', 'verb': 'observe', 'volume': 'v1', 'result': {'workspace': {'exit': 0, 'text': ws_text(epoch=3, replica='r2')}}}
        self.assertEqual(pb.master_lines(obs), {'v1': ('r2', 3)})
        none = {'kind': 'observe', 'verb': 'observe', 'volume': 'v1', 'result': {'workspace': {'exit': 0, 'text': ws_text(lease=False)}}}
        self.assertEqual(pb.master_lines(none), {})
        failed = {'kind': 'observe', 'verb': 'observe', 'volume': 'v1', 'result': {'workspace': {'exit': 1, 'text': ws_text()}}}
        self.assertEqual(pb.master_lines(failed), {})

    def test_the_m0_run_observations_give_lines(self):
        lines = [pb.master_lines(r) for r in load() if r['kind'] == 'observe']
        self.assertIn({'v1': ('r1', 1)}, lines)

    def test_result_line_wins_and_a_healthy_record_names_many(self):
        obs = {'kind': 'observe', 'verb': 'observe', 'volume': 'v1',
               'result': {'line': {'replica': 'r9', 'epoch': 9}, 'workspace': {'exit': 0, 'text': ws_text()}}}
        self.assertEqual(pb.master_lines(obs), {'v1': ('r9', 9)})
        healthy = {'kind': 'observe', 'verb': 'healthy', 'volume': '',
                   'result': {'lines': {'v1': {'replica': 'r1', 'epoch': 2}, 'v2': {'replica': 'r2', 'epoch': 5}}}}
        self.assertEqual(pb.master_lines(healthy), {'v1': ('r1', 2), 'v2': ('r2', 5)})
        gen = {'kind': 'observe', 'verb': 'gen_step', 'volume': 'v1', 'result': {'line': {'replica': 'r1', 'epoch': 1}}}
        self.assertEqual(pb.master_lines(gen), {})

    def test_red_a_line_derived_from_the_workspace_fences_a_write(self):
        recs = history(('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'))
        recs.insert(0, {'kind': 'observe', 'verb': 'observe', 'volume': 'v1', 'result': {'workspace': {'exit': 0, 'text': ws_text(epoch=2, replica='r2')}}})
        for i, r in enumerate(recs, 1):
            r['seq'] = r['mono_ns'] = i
            if r['kind'] == 'complete':
                r['of'] = i - 1
        v = hc.check(dump(recs))['C4']
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('the line r2@2 observed before its invoke (seq 1)', v['findings'][0])

    def test_holder_sample_shapes(self):
        inv = {'args': {'holder': {'server': 's', 'replica': 'r', 'epoch_before': 2}}}
        self.assertEqual(pb.holder_sample(inv, {'result': {'holder': {'epoch_after': 3}}}), ('ok', ('s', 'r'), 2, 3))
        self.assertEqual(pb.holder_sample(inv, None), ('ok', ('s', 'r'), 2, None))
        self.assertIsNone(pb.holder_sample({'args': {}}, None))
        self.assertEqual(pb.holder_sample({'args': {'holder': {'server': 's'}}}, None)[0], 'bad')


if __name__ == '__main__':
    unittest.main()
