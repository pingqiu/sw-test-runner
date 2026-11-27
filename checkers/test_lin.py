"""Controls of LIN, the per-block linearizability checker: green histories it
must pass, and the seeded defects (red controls) it must catch. Sequential
histories come from the shared synth(); concurrent ones are built here."""
import copy
import unittest
from unittest import mock

import history_check as hc
from test_history_check import A, B, C, JUNK, HKILL, dump, fl, load, ops, rd, synth, w, ws_text

U = 'e' * 64   # the value of an unknown write


def hist(*events):
    """A history from events in file order, the clock their index:
    ('i', key, verb, volume, args)     an invoke
    ('c', key, outcome, result)        the completion of the invoke `key`
    ('o', volume, result)              an observation
    ('x', key, outcome)                the abandoned child of `key` exited
    ('k', key) / ('a', key) / ('n',)   a child_exit the runner killed (outcome
                                       unknown, exit 137), one still alive at
                                       the end, one of no operation (op_seq null)
    ('d', volume)                      the guest device of `volume` disconnected
    closed by the writer's end record."""
    recs, at = [], {}
    for e in events:
        n = len(recs) + 1
        if e[0] == 'i':
            _, key, verb, vol, args = e
            at[key] = recs.append({'seq': n, 'kind': 'invoke', 'op': 'X.Y', 'verb': verb, 'volume': vol, 'args': args, 'mono_ns': n}) or recs[-1]
        elif e[0] == 'c':
            _, key, outcome, result = e
            inv = at[key]
            recs.append({'seq': n, 'kind': 'complete', 'of': inv['seq'], 'op': 'X.Y', 'verb': inv['verb'], 'volume': inv['volume'],
                         'mono_ns': n, 'outcome': outcome, 'result': result})
        elif e[0] == 'x':
            _, key, outcome = e
            recs.append({'seq': n, 'kind': 'observe', 'verb': 'child_exit', 'volume': '', 'mono_ns': n,
                         'result': {'op_seq': at[key]['seq'], 'pid': 7, 'start': 's7', 'exit': 0 if outcome == 'ok' else 1, 'outcome': outcome}})
        elif e[0] in ('k', 'a', 'n'):
            res = {'op_seq': None if e[0] == 'n' else at[e[1]]['seq'], 'pid': 7, 'start': 's7', 'outcome': 'unknown'}
            res.update({'k': {'killed': True, 'exit': 137, 'alive': False}, 'a': {'exit': None, 'alive': True}, 'n': {'exit': 0}}[e[0]])
            recs.append({'seq': n, 'kind': 'observe', 'verb': 'child_exit', 'volume': '', 'mono_ns': n, 'result': res})
        elif e[0] == 'd':
            recs.append({'seq': n, 'kind': 'observe', 'verb': 'disconnect', 'volume': e[1], 'mono_ns': n,
                         'result': {'volume': e[1], 'nqn': 'nqn.test:' + e[1]}})
        else:
            _, vol, result = e
            recs.append({'seq': n, 'kind': 'observe', 'verb': 'observe', 'volume': vol, 'mono_ns': n, 'result': result})
    recs.append({'seq': len(recs) + 1, 'kind': 'end', 'mono_ns': len(recs) + 1})
    return recs


def wi(key, sha, lba=0, vol='v1', flush=True):
    args = {'lba': lba, 'sha256': sha, 'round': 'g1'}
    if not flush:
        args['flush'] = False
    return ('i', key, 'write', vol, args)


def ri(key, lba=0, vol='v1'):
    return ('i', key, 'read', vol, {'lba': lba})


def ok(key, sha=None):
    return ('c', key, 'ok', {} if sha is None else {'sha256': sha})


def epoch(e, vol='v1'):
    """An observation recording the master's line (result.line)."""
    return ('o', vol, {'line': {'replica': 'r1', 'epoch': e}})


def ws_epoch(e, vol='v1'):
    """An observation whose line is only in the workspace answer."""
    return ('o', vol, {'workspace': {'exit': 0, 'text': ws_text(volume=vol, epoch=e)}})


def head_epoch(e, vol='v1'):
    """An observation with HEAD's epoch and no line."""
    return ('o', vol, {'head': {'epoch': e, 'gen': 1, 'holder': 'x/r1', 'state': ''}})


def lin(recs):
    return hc.check(dump(recs))['LIN']


class Base(unittest.TestCase):
    def assert_pass(self, v):
        self.assertEqual(v['verdict'], 'PASS', v)

    def assert_fail(self, v, why='no order of its operations explains them'):
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertTrue(any(why in x for x in v['findings']), v)


class Green(Base):
    def test_sequential(self):
        self.assert_pass(lin(synth(rd(0, hc.ZERO), w(0, A), rd(0, A), w(0, B), rd(0, B), rd(1, hc.ZERO))))

    def test_two_overlapping_writes_and_a_read_seeing_either(self):
        for seen in (A, B):
            self.assert_pass(lin(hist(wi('a', A), wi('b', B), ok('a'), ok('b'), ri('r'), ok('r', seen))))

    def test_a_read_overlapping_a_write_sees_the_old_or_the_new_value(self):
        for seen in (A, B):
            self.assert_pass(lin(hist(wi('a', A), ok('a'), wi('b', B), ri('r'), ok('r', seen), ok('b'), ri('s'), ok('s', B))))

    def test_concurrent_writers_with_reads_that_fix_the_order(self):
        # A and B overlap both reads: A, the read of A, B, the read of B is an
        # order inside every interval.
        self.assert_pass(lin(hist(wi('a', A), wi('b', B), ri('r'), ok('r', A), ok('b'), ri('s'), ok('s', B), ok('a'))))

    def test_an_unknown_write_that_lands(self):
        self.assert_pass(lin(synth(w(0, A), w(0, U, outcome='unknown'), rd(0, A), rd(0, U), rd(0, U))))

    def test_an_unknown_write_that_never_lands(self):
        self.assert_pass(lin(synth(w(0, A), w(0, U, outcome='unknown'), rd(0, A), rd(0, A))))

    def test_a_write_that_never_completed_is_an_unknown_write(self):
        recs = hist(wi('a', A), ok('a'), wi('u', U), ri('r'), ok('r', A), ri('s'), ok('s', U))
        self.assert_pass(lin(recs))

    def test_a_lost_unflushed_write_after_a_kill_is_allowed(self):
        for seen in (A, B):
            self.assert_pass(lin(synth(w(0, A), w(0, B, flush=False), HKILL, rd(0, seen))))

    def test_blocks_are_independent(self):
        self.assert_pass(lin(hist(wi('a', A, lba=0), wi('b', B, lba=1), ri('r1', lba=1), ok('r1', B), ok('a'), ok('b'),
                                  ri('r0', lba=0), ok('r0', A))))


class Red(Base):
    """The seeded defects LIN must catch."""

    def test_red_a_stale_read_after_an_acknowledged_write(self):
        self.assert_fail(lin(synth(w(0, A), w(0, B), rd(0, A))))
        # Concurrent form: the read is invoked after B was acknowledged.
        self.assert_fail(lin(hist(wi('a', A), ok('a'), wi('b', B), ri('x'), ok('x', A), ok('b'), ri('r'), ok('r', A))))
        # Inside one piece (a long write of C overlaps everything): A, then B,
        # then the read, in real time; the read of A has no order.
        self.assert_fail(lin(hist(wi('l', C), wi('a', A), ok('a'), wi('b', B), ok('b'), ri('r'), ok('r', A), ok('l'))))
        self.assert_pass(lin(hist(wi('l', C), wi('a', A), ok('a'), wi('b', B), ok('b'), ri('r'), ok('r', C), ok('l'))))

    def test_red_a_value_back_after_it_was_overwritten_and_flushed(self):
        # B unflushed then flushed: durable. After the kill A comes back.
        self.assert_fail(lin(synth(w(0, A), w(0, B, flush=False), fl(), HKILL, rd(0, A))))
        # Control: without the flush B was never durable and A is allowed.
        self.assert_pass(lin(synth(w(0, A), w(0, B, flush=False), HKILL, rd(0, A))))
        # B written with its fsync is durable as well.
        self.assert_fail(lin(synth(w(0, A), w(0, B), HKILL, rd(0, A))))
        # And with no kill at all, A after the flushed B is a stale read.
        self.assert_fail(lin(synth(w(0, A), w(0, B, flush=False), fl(), rd(0, A))))

    def test_red_a_read_of_a_value_no_write_produced(self):
        self.assert_fail(lin(synth(w(0, A), rd(0, JUNK))))
        self.assert_fail(lin(hist(wi('a', A), wi('b', B), ok('a'), ok('b'), ri('r'), ok('r', hc.ZERO))))

    # kb r3: an unknown write's window closes at the first of its child's
    # exit, its device's disconnect, the line's advance; a kill closes nothing.
    BASE = (wi('a', A), ok('a'), wi('u', U), ('c', 'u', 'unknown', {}))
    LATE = (ri('r'), ok('r', A), ri('s'), ok('s', U))   # A read, then U lands

    def test_an_unknown_write_may_land_after_a_kill_while_its_window_is_open(self):
        kill = (('i', 'k', 'kill', '', HKILL[2]), ok('k'))
        self.assert_pass(lin(hist(*self.BASE, *kill, *self.LATE)))
        self.assert_pass(lin(synth(w(0, A), w(0, U, outcome='unknown'), HKILL, rd(0, A), rd(0, U))))

    def test_red_an_unknown_write_landing_after_its_child_exited_failed(self):
        v = lin(hist(*self.BASE, ('x', 'u', 'failed'), *self.LATE))
        self.assert_fail(v)
        self.assertIn('unknown writes whose window closed: exited failed 1', v['findings'])
        # A failed exit says it never landed: before the exit too.
        self.assert_fail(lin(hist(*self.BASE, ri('r'), ok('r', U), ('x', 'u', 'failed'))))
        # Green: the value it never wrote is the one read.
        self.assert_pass(lin(hist(*self.BASE, ('x', 'u', 'failed'), ri('r'), ok('r', A), ri('s'), ok('s', A))))
        # Also with a kill before the exit: the kill closed nothing, the exit does.
        kill = (('i', 'k', 'kill', '', HKILL[2]), ok('k'))
        self.assert_fail(lin(hist(*self.BASE, *kill, ('x', 'u', 'failed'), *self.LATE)))

    def test_red_an_unknown_write_whose_child_exited_ok_did_land(self):
        # Exited ok: it landed at or before the exit; a read after it sees U.
        self.assert_fail(lin(hist(*self.BASE, ('x', 'u', 'ok'), ri('r'), ok('r', A))))
        self.assert_pass(lin(hist(*self.BASE, ('x', 'u', 'ok'), ri('r'), ok('r', U))))
        # A read between the invoke and the exit may still see A.
        self.assert_pass(lin(hist(*self.BASE, ri('r'), ok('r', A), ('x', 'u', 'ok'), ri('s'), ok('s', U))))

    def test_red_an_unknown_write_landing_after_its_device_disconnected(self):
        self.assert_fail(lin(hist(*self.BASE, ('d', 'v1'), *self.LATE)))
        # Green: another volume's device, or U landing before the disconnect.
        self.assert_pass(lin(hist(*self.BASE, ('d', 'v2'), *self.LATE)))
        self.assert_pass(lin(hist(*self.BASE, ri('r'), ok('r', U), ('d', 'v1'), ri('s'), ok('s', U))))

    def test_an_unknown_write_landing_before_a_flush_is_durable(self):
        steps = (w(0, A), w(0, U, outcome='unknown', flush=False))
        # Read U (it landed), then the flush made it durable: A after the kill is red.
        self.assert_fail(lin(synth(*steps, rd(0, U), fl(), HKILL, rd(0, A))))
        # Green: unread, U may have landed before or after the flush.
        self.assert_pass(lin(synth(*steps, fl(), HKILL, rd(0, U))))
        self.assert_pass(lin(synth(*steps, fl(), HKILL, rd(0, A))))

    def test_red_an_unknown_write_appearing_after_the_epoch_advanced(self):
        base = [epoch(1), wi('a', A), ok('a'), wi('u', U), ('c', 'u', 'unknown', {})]
        late = [ri('r'), ok('r', A), ri('s'), ok('s', U)]
        v = lin(hist(*base, epoch(2), *late))
        self.assert_fail(v)
        self.assertIn('unknown writes whose window closed: line advanced 1', v['findings'])
        # Control: the same epoch shown again closes nothing.
        self.assert_pass(lin(hist(*base, epoch(1), *late)))
        # And U landing before the advance is allowed.
        self.assert_pass(lin(hist(*base, ri('r'), ok('r', U), epoch(2), ri('s'), ok('s', U))))

    def test_red_a_line_advance_seen_only_in_a_workspace_answer_closes_the_window(self):
        base = [ws_epoch(1), wi('a', A), ok('a'), wi('u', U), ('c', 'u', 'unknown', {})]
        late = [ri('r'), ok('r', A), ri('s'), ok('s', U)]
        v = lin(hist(*base, ws_epoch(2), *late))
        self.assert_fail(v)
        self.assertIn('unknown writes whose window closed: line advanced 1', v['findings'])
        self.assert_pass(lin(hist(*base, ws_epoch(1), *late)))
        # The window is the LINE's (as C4 reads it): HEAD's epoch alone closes nothing.
        self.assert_pass(lin(hist(head_epoch(1), wi('a', A), ok('a'), wi('u', U), ('c', 'u', 'unknown', {}),
                                  head_epoch(2), *late)))

    def test_red_an_older_value_after_a_later_write_completed_before_the_read(self):
        # A acknowledged; B overlaps the first read, which sees B; B completes;
        # the second read, invoked after, sees A.
        recs = hist(wi('a', A), ok('a'), wi('b', B), ri('r'), ok('r', B), ok('b'), ri('s'), ok('s', A))
        self.assert_fail(lin(recs))
        # Control: the reads in the other order are the write taking effect.
        self.assert_pass(lin(hist(wi('a', A), ok('a'), wi('b', B), ri('r'), ok('r', A), ok('b'), ri('s'), ok('s', B))))
        # The same inside one piece, a long write of C overlapping it all.
        self.assert_fail(lin(hist(wi('l', C), wi('a', A), ok('a'), wi('b', B), ri('r'), ok('r', B), ok('b'), ri('s'), ok('s', A), ok('l'))))
        self.assert_pass(lin(hist(wi('l', C), wi('a', A), ok('a'), wi('b', B), ri('r'), ok('r', A), ok('b'), ri('s'), ok('s', B), ok('l'))))


class AgreesWithC1(Base):
    """On sequential histories without kills LIN is C1's rule."""

    def test_the_real_run(self):
        recs = load()
        v = hc.check(dump(recs))
        self.assertEqual((v['C1']['verdict'], v['LIN']['verdict']), ('PASS', 'PASS'), (v['C1'], v['LIN']))
        # Seeded: a read of a block that was overwritten returns the older value.
        bad = copy.deepcopy(recs)
        writes = {}
        for inv, comp in ops(bad, 'write'):
            writes.setdefault((inv['volume'], inv['args']['lba']), []).append(inv['args']['sha256'])
        key = next(k for k, vals in writes.items() if len(set(vals)) > 1)
        inv, comp = [p for p in ops(bad, 'read', key[0]) if p[0]['args']['lba'] == key[1]][-1]
        comp['result']['sha256'] = writes[key][0]
        v = hc.check(dump(bad))
        self.assertEqual((v['C1']['verdict'], v['LIN']['verdict']), ('FAIL', 'FAIL'), (v['C1'], v['LIN']))

    def test_sequential_synthetic_histories(self):
        cases = [
            (w(0, A), rd(0, A)),
            (w(0, A), w(0, B), rd(0, B)),
            (w(0, A), w(0, B), rd(0, A)),
            (w(0, A), rd(0, JUNK)),
            (rd(0, hc.ZERO), w(0, A), rd(0, A), rd(1, hc.ZERO)),
            (w(0, A), w(0, U, outcome='unknown'), rd(0, U)),
            (w(0, A), w(0, U, outcome='unknown'), rd(0, A)),
            (w(0, A), w(0, B, outcome='refused'), rd(0, A)),
            (w(0, A), w(0, B, outcome='refused'), rd(0, B)),
            (w(0, A), w(1, B), rd(1, B), rd(0, A)),
            (w(0, A), w(0, B, flush=False), fl(), rd(0, B)),
        ]
        for steps in cases:
            v = hc.check(dump(synth(*steps)))
            self.assertEqual(v['C1']['verdict'], v['LIN']['verdict'], (steps, v['C1'], v['LIN']))
            self.assertIn(v['LIN']['verdict'], ('PASS', 'FAIL'))

    def test_lin_is_stricter_than_c1_on_a_value_that_flaps(self):
        # C1 keeps an unknown's value possible beside every later one; LIN
        # knows that once U was read after B, B cannot come back by itself.
        steps = (w(0, A), w(0, U, outcome='unknown'), w(0, B), rd(0, U), rd(0, B))
        v = hc.check(dump(synth(*steps)))
        self.assertEqual((v['C1']['verdict'], v['LIN']['verdict']), ('PASS', 'FAIL'))


class Budget(Base):
    OVERLAP = (wi('a', A), wi('b', B), ok('a'), ok('b'), ri('r'), ok('r', A))

    def test_a_piece_over_the_operation_budget_is_not_judged(self):
        with mock.patch.object(hc, 'LIN_MAX_OPS', 1):
            v = lin(hist(*self.OVERLAP))
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertTrue(any(x.startswith('NOT JUDGED: v1 lba 0, piece of 2 operation(s)') for x in v['findings']), v)

    def test_pieces_after_one_not_judged_are_still_judged(self):
        with mock.patch.object(hc, 'LIN_MAX_OPS', 1):
            # After the unjudged piece the value is not determined: the read of
            # A fixes it, a write of C follows, and a read of A is stale.
            self.assertEqual(lin(hist(*self.OVERLAP, wi('c', C), ok('c'), ri('s'), ok('s', C)))['verdict'], 'UNKNOWN')
            self.assert_fail(lin(hist(*self.OVERLAP, wi('c', C), ok('c'), ri('s'), ok('s', A))))

    def test_a_piece_over_the_time_budget_is_not_judged_and_many_say_so(self):
        with mock.patch.object(hc, 'LIN_SECONDS', 0):
            v = lin(synth(w(0, A), rd(0, A), w(0, B), rd(0, B)))
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertTrue(any('over the time guard of 0 s' in x and 'budget %d states' % hc.LIN_MAX_STATES in x for x in v['findings']), v)
        self.assertTrue(any("concurrency per block is too high" in x for x in v['findings']), v)

    def test_a_piece_over_the_state_budget_is_not_judged(self):
        # The primary budget: a deterministic count of states.
        with mock.patch.object(hc, 'LIN_MAX_STATES', 2):
            v = lin(hist(*self.OVERLAP))
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertTrue(any('over the budget of 2 states (time guard 60 s)' in x for x in v['findings']), v)

    def test_a_budget_never_passes(self):
        with mock.patch.object(hc, 'LIN_MAX_OPS', 0):
            self.assertNotEqual(lin(synth(w(0, A), rd(0, A)))['verdict'], 'PASS')


class OneRuleWithDUR(Base):
    """A failed child exit means the write never landed: DUR and LIN apply the
    same consequence (unknown_effect), so they cannot disagree."""

    def both(self, recs):
        v = hc.check(dump(recs))
        return v['DUR'], v['LIN']

    def test_red_a_read_of_a_write_whose_child_exited_failed_fails_both(self):
        # Unflushed (DUR is exercised); the read after the failed exit shows U.
        recs = hist(wi('a', A), ok('a'), wi('u', U, flush=False), ('c', 'u', 'unknown', {}), ('x', 'u', 'failed'), ri('r'), ok('r', U))
        dur, lin_ = self.both(recs)
        self.assert_fail(dur, 'not one of')
        self.assert_fail(lin_)
        # Also when the read comes before the exit: it never landed at all.
        recs = hist(wi('a', A), ok('a'), wi('u', U, flush=False), ('c', 'u', 'unknown', {}), ri('r'), ok('r', U), ('x', 'u', 'failed'))
        dur, lin_ = self.both(recs)
        self.assert_fail(dur, 'not one of')
        self.assert_fail(lin_)

    def test_the_same_with_the_child_exited_ok_passes_both(self):
        recs = hist(wi('a', A), ok('a'), wi('u', U, flush=False), ('c', 'u', 'unknown', {}), ('x', 'u', 'ok'), ri('r'), ok('r', U))
        dur, lin_ = self.both(recs)
        self.assert_pass(dur)
        self.assert_pass(lin_)


class KilledChild(Base):
    """A child the runner killed at its deadline (child_exit outcome unknown):
    its process exits only once the kernel I/O returned, so the write landed
    at or before the record or never, and cannot land after it. Both checkers."""
    KILL = (('i', 'k', 'kill', '', HKILL[2]), ok('k'))

    def both(self, *events):
        v = hc.check(dump(hist(*events)))
        return v['DUR'], v['LIN']

    def base(self):
        return (wi('a', A), ok('a'), wi('u', U, flush=False), ('c', 'u', 'unknown', {}))

    def test_a_read_of_its_value_after_the_killed_exit_with_no_earlier_read_passes(self):
        for dur, lin_ in (self.both(*self.base(), ('k', 'u'), ri('r'), ok('r', U)),
                          self.both(*self.base(), *self.KILL, ('k', 'u'), ri('r'), ok('r', U))):
            self.assert_pass(dur)
            self.assert_pass(lin_)

    def test_red_a_value_first_appearing_after_the_killed_exit(self):
        # A read after the exit saw A: U did not land by the exit, and cannot after.
        dur, lin_ = self.both(*self.base(), *self.KILL, ('k', 'u'), ri('r'), ok('r', A), ri('s'), ok('s', U))
        self.assert_fail(dur, 'the value changed')
        self.assert_fail(lin_)
        # Green: A read BEFORE the exit; U may land between that read and the exit.
        dur, lin_ = self.both(*self.base(), *self.KILL, ri('r'), ok('r', A), ('k', 'u'), ri('s'), ok('s', U))
        self.assert_pass(dur)
        self.assert_pass(lin_)
        # Once a read after the exit fixed U, A does not come back.
        dur, lin_ = self.both(*self.base(), *self.KILL, ri('r'), ok('r', A), ('k', 'u'), ri('s'), ok('s', U), ri('t'), ok('t', A))
        self.assert_fail(dur, 'the value changed')
        self.assert_fail(lin_)
        self.assert_fail(lin(hist(*self.base(), ('k', 'u'), ri('r'), ok('r', A), ri('s'), ok('s', U))))
        self.assertIn('unknown writes whose window closed: exited unknown 1',
                      lin(hist(*self.base(), ('k', 'u'), ri('r'), ok('r', U)))['findings'])

    def test_alive_or_of_no_operation_closes_nothing(self):
        for ev in (('a', 'u'), ('n',)):
            # The record before the read of A: had it closed, U could not appear.
            dur, lin_ = self.both(*self.base(), *self.KILL, ev, ri('r'), ok('r', A), ri('s'), ok('s', U))
            self.assert_pass(dur)
            self.assert_pass(lin_)


class Notes(Base):
    def test_pieces_in_flight_are_counted_and_a_sequential_history_says_so(self):
        v = lin(synth(w(0, A), rd(0, A)))
        self.assertIn('pieces with more than one operation in flight: 0', v['findings'])
        self.assertTrue(any(x.startswith('a sequential history') for x in v['findings']), v)
        v = lin(hist(wi('a', A), wi('b', B), ok('a'), ok('b'), ri('r'), ok('r', A)))
        self.assertIn('pieces with more than one operation in flight: 1', v['findings'])
        self.assertFalse(any(x.startswith('a sequential history') for x in v['findings']), v)


class Registry(Base):
    def test_registered_and_not_exercised_without_a_read(self):
        self.assertIs(hc.CHECKERS['LIN'], hc.lin)
        v = lin(synth(w(0, A)))
        self.assert_fail(v, 'no read in the history')


if __name__ == '__main__':
    unittest.main()
