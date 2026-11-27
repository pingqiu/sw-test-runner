"""Controls of C4 (epoch fencing): green histories and one seeded defect per
rule that it must catch."""
import unittest

import history_check as hc
import product_block as pb
from test_history_check import dump, load, ws_text

A = 'a' * 64


def ident(server):
    """A holder process identity, one per server unless a step says otherwise."""
    return {'pid': 100 + sum(map(ord, server)), 'start': 'start-' + server}


def history(*steps):
    """A history from steps:
      ('w' | 'r', volume, server, replica, epoch_before, epoch_after, outcome[, opts])
        a write or read through that holder; opts: 'during', a list of line
        steps recorded between its invoke and its complete; 'id_after', the
        process identity sampled after it (default: the server's one);
        'no_id', no identity recorded;
      ('line', volume, replica, epoch)  an observe record of the master's line;
      ('header', ack_modes)  the run's header record;
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
        if st[0] == 'header':
            add({'kind': 'observe', 'client': 'runner', 'verb': 'gen_header', 'volume': '', 'result': {'seed': '1', 'ack_modes': st[1]}})
            continue
        if st[0] == 'plain':
            add({'kind': 'invoke', 'op': 'X.Y', 'verb': 'write', 'volume': st[1], 'args': {'lba': 0, 'sha256': A}})
            add({'kind': 'complete', 'of': recs[-1]['seq'], 'op': 'X.Y', 'verb': 'write', 'volume': st[1], 'outcome': 'ok', 'result': {}})
            continue
        kind, vol, server, rep, before, after, outcome = st[:7]
        opts = st[7] if len(st) > 7 else {}
        verb = 'write' if kind == 'w' else 'read'
        id_before = {} if opts.get('no_id') else ident(server)
        id_after = {} if opts.get('no_id') else opts.get('id_after', ident(server))
        args = {'lba': 0, 'holder': dict({'server': server, 'replica': rep, 'epoch_before': before}, **id_before)}
        if verb == 'write':
            args['sha256'] = A
        add({'kind': 'invoke', 'op': 'X.Y', 'verb': verb, 'volume': vol, 'args': args})
        of = recs[-1]['seq']
        for d in opts.get('during', []):
            line(*d[1:])
        res = {'holder': dict({'epoch_after': after}, **id_after)}
        if verb == 'read':
            res['sha256'] = A
        add({'kind': 'complete', 'of': of, 'op': 'X.Y', 'verb': verb, 'volume': vol, 'outcome': outcome, 'result': res})
    add({'kind': 'end'})
    return recs


def c4(*steps):
    return hc.check(dump(history(*steps)))['C4']


# A hand-over on v1: holder A (a-v1/r1) at epoch 1 under the line r1@1; the
# line moves to r2@2 (observation seq 4; 5 after a header); holder B (b-v1/r2) writes at 2.
HANDOVER = [('line', 'v1', 'r1', 1),
            ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
            ('line', 'v1', 'r2', 2),
            ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok')]
ACK = ('header', {'v1': 'acknowledging'})
BEST = ('header', {'v1': 'best_effort'})


class C4(unittest.TestCase):
    def assert_c4(self, want, *steps, contains=None):
        v = c4(*steps)
        self.assertEqual(v['verdict'], want, v)
        for c in ([contains] if isinstance(contains, str) else contains or []):
            self.assertTrue(any(c in x for x in v['findings']), (c, v))
        return v

    def test_listed(self):
        self.assertIs(hc.CHECKERS['C4'], hc.c4)

    # --- green ---------------------------------------------------------------

    def test_a_hand_over_passes(self):
        v = self.assert_c4('PASS', ACK, *HANDOVER, ('r', 'v1', 'b-v1', 'r2', 2, 2, 'ok'))
        self.assertEqual(v['findings'], ['either: 0 of 2 acknowledged writes', 'not acknowledged: 0', 'stale reads: 0',
                                         'stale primary writes: 0', 'line changes with writes on both sides: 1'])

    def test_no_mode_in_the_header_is_judged_acknowledging_and_noted(self):
        self.assert_c4('PASS', *HANDOVER, contains='v1: no acknowledgement mode in the header: judged as acknowledging (the strict default)')

    def test_a_write_open_while_the_line_moves_is_not_fenced(self):
        # The line r2@2 is observed after A's second write was invoked: the
        # write may have been served before the move; rule 2 does not apply.
        self.assert_c4('PASS', ACK, ('line', 'v1', 'r1', 1), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                       ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok', {'during': [('line', 'v1', 'r2', 2)]}),
                       ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'))

    def test_an_unacknowledged_write_is_not_judged(self):
        self.assert_c4('PASS', ACK, *HANDOVER, ('w', 'v1', 'a-v1', 'r1', 1, 1, 'unknown'),
                       ('w', 'v1', 'a-v1', 'r1', 1, 1, 'refused'), contains='not acknowledged: 2')

    def test_volumes_are_judged_apart(self):
        # One epoch number on two volumes, two holders: no conflict.
        self.assert_c4('PASS', ('header', {'v1': 'acknowledging', 'v2': 'acknowledging'}), *HANDOVER,
                       ('w', 'v2', 'b-v2', 'r2', 2, 2, 'ok'))

    # --- rule 1 --------------------------------------------------------------

    def test_red_two_holders_acknowledging_at_one_epoch(self):
        self.assert_c4('FAIL', ACK, *HANDOVER, ('w', 'v1', 'c-v1', 'r2', 2, 2, 'ok'),
                       contains='two holders at one epoch: v1 epoch 2 acknowledged writes through b-v1/r2 (write seq 6), c-v1/r2 (write seq 8)')
        # the same server with another replica is another holder
        self.assert_c4('FAIL', ACK, *HANDOVER, ('w', 'v1', 'b-v1', 'r9', 2, 2, 'ok'), contains='two holders at one epoch')

    # --- rule 2 --------------------------------------------------------------

    def test_red_a_write_acknowledged_below_an_observed_line(self):
        self.assert_c4('FAIL', ACK, *HANDOVER, ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                       contains='write acknowledged below the line: write seq 8 v1 through a-v1/r1 at epoch 1, the line r2@2 observed before its invoke (seq 5)')

    # --- rule 3 --------------------------------------------------------------

    def test_red_a_write_through_a_replica_the_line_does_not_name(self):
        # x-v1/r5 writes at epoch 1; the line at epoch 1, observed only AFTER
        # the write, names r1. Rule 3 judges a line whenever it was observed.
        v = self.assert_c4('FAIL', ACK, ('w', 'v1', 'x-v1', 'r5', 1, 1, 'ok'), ('line', 'v1', 'r1', 1),
                           ('line', 'v1', 'r2', 2), ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'),
                           contains='write through a replica the line does not name: write seq 2 v1 through x-v1/r5 at epoch 1, the line at epoch 1 names r1 (seq 4)')
        self.assertEqual(len([x for x in v['findings'] if x.startswith(('two holders', 'write ', 'stale primary write:'))]), 1, v)
        # the replica it names passes (the hand-over)
        self.assert_c4('PASS', ACK, ('w', 'v1', 'x-v1', 'r1', 1, 1, 'ok'), ('line', 'v1', 'r1', 1),
                       ('line', 'v1', 'r2', 2), ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'))

    def test_red_two_replicas_named_at_one_epoch_fail_with_no_write(self):
        v = self.assert_c4('FAIL', ACK, ('line', 'v1', 'r1', 2), ('line', 'v1', 'r2', 2),
                           contains='the line names 2 replicas at one epoch: v1 epoch 2: r1 (seq 2), r2 (seq 3)')
        self.assertFalse(any('not exercised' in x for x in v['findings']), 'a split line is exercised by itself')
        # with writes: the split fails, and each write is judged against both replicas
        v = self.assert_c4('FAIL', ACK, *HANDOVER, ('line', 'v1', 'r9', 2),
                           contains=['the line names 2 replicas at one epoch: v1 epoch 2: r2 (seq 5), r9 (seq 8)',
                                     'write seq 6 v1 through b-v1/r2 at epoch 2, the line at epoch 2 names r9 (seq 8)'])

    def test_the_same_replica_named_twice_at_one_epoch_passes(self):
        self.assert_c4('PASS', ACK, *HANDOVER, ('line', 'v1', 'r2', 2), ('line', 'v1', 'r2', 2))
        # and alone it is no evidence: not exercised
        self.assert_c4('FAIL', ACK, ('line', 'v1', 'r2', 2), ('line', 'v1', 'r2', 2), contains='C4 not exercised')

    def test_an_either_write_is_not_judged_by_rule_3(self):
        self.assert_c4('PASS', ACK, ('w', 'v1', 'x-v1', 'r5', 0, 1, 'ok'), ('line', 'v1', 'r1', 1),
                       ('line', 'v1', 'r2', 2), ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'))

    # --- rule 4 --------------------------------------------------------------

    # A at e1, B at e2 completes, then A at e1 again (invoked after B's
    # completion, before any line shows e2: rule 2 does not see it).
    STALE_PRIMARY = [('line', 'v1', 'r1', 1), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                     ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                     ('line', 'v1', 'r2', 2), ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok')]
    STALE_MSG = 'stale primary write: write seq 7 v1 through a-v1/r1 at epoch 1, after write seq 5 through b-v1/r2 at epoch 2 completed (seq 6)'

    def test_red_a_stale_primary_in_an_acknowledging_volume(self):
        v = self.assert_c4('FAIL', ACK, *self.STALE_PRIMARY, contains=[self.STALE_MSG, 'stale primary writes: 1'])
        self.assertFalse(any(x.startswith(('write acknowledged below', 'two holders')) for x in v['findings']), v)
        # with no mode recorded: the strict default
        self.assert_c4('FAIL', *self.STALE_PRIMARY, contains=['stale primary write: write seq 6 v1 through a-v1/r1 at epoch 1', 'judged as acknowledging'])

    def test_red_a_stale_primary_in_a_best_effort_volume_is_reported(self):
        v = self.assert_c4('PASS', BEST, *self.STALE_PRIMARY, contains=[self.STALE_MSG + ': best-effort volume, reported', 'stale primary writes: 1'])
        self.assertFalse(any('no acknowledgement mode' in x for x in v['findings']), v)

    def test_a_write_invoked_before_the_successor_completed_is_not_stale(self):
        # A's write invoked before B's write completed: overlapping, not after.
        steps = [('line', 'v1', 'r1', 1), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok')]
        recs = history(ACK, *steps, ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                       ('line', 'v1', 'r2', 2), ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'))
        # move A's second invoke (seq 7) before B's completion (seq 6)
        recs[5], recs[6] = recs[6], recs[5]
        for i, r in enumerate(recs, 1):
            r['seq'] = r['mono_ns'] = i
        recs[6]['of'] = 5   # B's completion
        recs[7]['of'] = 6   # A's completion
        self.assertEqual((recs[5]['kind'], recs[6]['kind'], recs[6]['volume']), ('invoke', 'complete', 'v1'))
        v = hc.check(dump(recs))['C4']
        self.assertEqual(v['verdict'], 'PASS', v)

    def test_bad_header_modes_are_noted(self):
        v = self.assert_c4('PASS', ('header', {'v1': 'sometimes'}), *HANDOVER, contains="v1 has mode 'sometimes'")
        self.assertTrue(any('judged as acknowledging' in x for x in v['findings']), v)

    # --- "either" ------------------------------------------------------------

    def test_red_either_overlapping_a_hand_over_is_not_blamed(self):
        # B's status epoch steps 1 -> 2 while its write is open, because the
        # hand-over to B lands during it; A acknowledged at e1 before. Taking
        # B's epoch as the one before (1) would blame two holders at e1, and
        # rule 2 against the line r2@2 observed before B's invoke would blame
        # a write below the line. B may have been served at e2, where no other
        # holder acknowledged and no line is above it: not blamed.
        self.assert_c4('PASS', ACK, ('line', 'v1', 'r1', 1), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                       ('line', 'v1', 'r2', 2), ('w', 'v1', 'b-v1', 'r2', 1, 2, 'ok'),
                       contains='either: 1 of 2 acknowledged writes')
        # The same either write IS blamed when every epoch it may have had is
        # taken by another holder (A at e1 and C at e2 both acknowledged) ...
        self.assert_c4('FAIL', ACK, *HANDOVER, ('w', 'v1', 'c-v1', 'r3', 1, 2, 'ok'),
                       contains='write seq 8 v1 through c-v1/r3 at epoch either 1..2, every epoch taken by another holder: 1: a-v1/r1 (write seq 3); 2: b-v1/r2 (write seq 6)')
        # ... or when a line above both of its epochs was observed before it.
        self.assert_c4('FAIL', ACK, *HANDOVER, ('line', 'v1', 'r3', 3), ('w', 'v1', 'c-v1', 'r3', 1, 2, 'ok'),
                       contains='through c-v1/r3 at epoch either 1..2, the line r3@3')

    # --- epoch going down ----------------------------------------------------

    def test_red_an_epoch_going_down_in_one_process_fails(self):
        self.assert_c4('FAIL', ACK, *HANDOVER, ('w', 'v1', 'b-v1', 'r2', 2, 1, 'ok'),
                       contains='holder epoch went down within one operation: write seq 8 v1 through b-v1/r2, epoch 2 -> 1, same process')
        # whatever the outcome: it is the holder's state, not the write's
        self.assert_c4('FAIL', ACK, *HANDOVER, ('r', 'v1', 'b-v1', 'r2', 2, 1, 'unknown'),
                       contains='holder epoch went down within one operation: read seq 8')

    def test_an_epoch_going_down_with_another_process_is_a_note(self):
        self.assert_c4('PASS', ACK, *HANDOVER, ('w', 'v1', 'b-v1', 'r2', 2, 1, 'ok', {'id_after': {'pid': 7, 'start': 'later'}}),
                       contains='the holder epoch went down while it was open (2 -> 1)')
        self.assert_c4('PASS', ACK, *HANDOVER, ('w', 'v1', 'b-v1', 'r2', 2, 1, 'ok', {'no_id': True}),
                       contains='identity not recorded')

    # --- stale read ----------------------------------------------------------

    def test_red_a_stale_read_is_reported_not_failed(self):
        self.assert_c4('PASS', ACK, *HANDOVER, ('r', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                       contains=['stale read: read seq 8 v1 answered by a-v1/r1 at epoch 1 after the line r2@2 was observed (seq 5)', 'stale reads: 1'])

    # --- exercised -----------------------------------------------------------

    def test_red_not_exercised(self):
        for steps in ([('plain', 'v1')], [('w', 'v1', 'a-v1', 'r1', 1, 1, 'unknown')], []):
            self.assert_c4('FAIL', *steps, contains='no acknowledged write carries holder and epoch samples')
        # the real M0 run records no holder samples
        self.assertIn('C4 not exercised', hc.check(dump(load()))['C4']['findings'][0])

    def test_red_not_exercised_without_an_epoch_change(self):
        steady = [ACK, ('line', 'v1', 'r1', 1), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'),
                  ('line', 'v1', 'r1', 1)]
        self.assert_c4('FAIL', *steady, contains='no line changed epoch with acknowledged writes on both sides of the change: C4 not exercised')
        # a change with writes on one side only
        self.assert_c4('FAIL', ACK, ('line', 'v1', 'r1', 1), ('line', 'v1', 'r2', 2), ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'),
                       contains='C4 not exercised')
        self.assert_c4('FAIL', ACK, ('line', 'v1', 'r1', 1), ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'), ('line', 'v1', 'r2', 2),
                       contains='C4 not exercised')
        # epochs that change without a line change are not a change of the line
        self.assert_c4('FAIL', ACK, ('w', 'v1', 'a-v1', 'r1', 1, 1, 'ok'), ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'),
                       contains='C4 not exercised')

    def test_a_malformed_sample_is_not_judged(self):
        recs = history(ACK, *HANDOVER, ('w', 'v1', 'b-v1', 'r2', 2, 2, 'ok'))
        recs[-2]['result']['holder'] = {'epoch_after': 'one'}
        v = hc.check(dump(recs))['C4']
        self.assertEqual(v['verdict'], 'PASS', v)
        self.assertTrue(any(x.startswith('not judged: write seq 8') for x in v['findings']), v)


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
        inv = {'args': {'holder': {'server': 's', 'replica': 'r', 'epoch_before': 2, 'pid': 5, 'start': 't'}}}
        self.assertEqual(pb.holder_sample(inv, {'result': {'holder': {'epoch_after': 3, 'pid': 5, 'start': 't'}}}),
                         ('ok', ('s', 'r'), 2, 3, (5, 't'), (5, 't')))
        self.assertEqual(pb.holder_sample(inv, None), ('ok', ('s', 'r'), 2, None, (5, 't'), None))
        self.assertEqual(pb.holder_sample({'args': {'holder': {'server': 's', 'replica': 'r', 'epoch_before': 2}}},
                                          {'result': {'holder': {'epoch_after': 2}}}), ('ok', ('s', 'r'), 2, 2, None, None))
        self.assertIsNone(pb.holder_sample({'args': {}}, None))
        self.assertEqual(pb.holder_sample({'args': {'holder': {'server': 's'}}}, None)[0], 'bad')

    def test_ack_modes_from_the_header(self):
        recs = history(('header', {'v1': 'acknowledging', 'v2': 'best_effort'}))
        self.assertEqual(pb.ack_modes(recs), ({'v1': 'acknowledging', 'v2': 'best_effort'}, []))
        self.assertEqual(pb.ack_modes(history()), ({}, []))
        modes, notes = pb.ack_modes(history(('header', ['v1'])))
        self.assertEqual(modes, {})
        self.assertTrue(notes)


if __name__ == '__main__':
    unittest.main()
