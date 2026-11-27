"""Controls of history_check: the green history of a real M0 run, and one seeded
defect per checker that it must catch (and that no other checker reports)."""
import copy
import json
import unittest
from pathlib import Path
from unittest import mock

import history_check as hc
import product_block as pb

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


def with_fault(recs, apply_outcome='ok', restore_outcome='ok', restore=True, at=None):
    """The real run with a power fault spliced in before index `at` (default:
    before the end record): apply invoke/complete, then restore invoke/complete."""
    recs = copy.deepcopy(recs)
    at = len(recs) - 1 if at is None else at
    mono = recs[at - 1]['mono_ns']
    args = {'fault_key': 'vm_power/n2', 'resource': '/subscriptions/x/vm/n2', 'vm_id': 'vmid-1', 'node': 'n2'}
    new = [{'kind': 'invoke', 'seq': 0, 'op': 'Runner.VmPowerOff', 'verb': 'fault_apply', 'args': args, 'mono_ns': mono},
           {'kind': 'complete', 'seq': 0, 'of': 'A', 'op': 'Runner.VmPowerOff', 'verb': 'fault_apply', 'outcome': apply_outcome, 'mono_ns': mono}]
    if restore:
        new += [{'kind': 'invoke', 'seq': 0, 'op': 'Runner.VmPowerOn', 'verb': 'fault_restore', 'args': args, 'mono_ns': mono},
                {'kind': 'complete', 'seq': 0, 'of': 'R', 'op': 'Runner.VmPowerOn', 'verb': 'fault_restore', 'outcome': restore_outcome, 'mono_ns': mono}]
    recs[at:at] = new
    out = recs
    # number everything; link the spliced completes to their invokes
    seq_of = {}
    for i, r in enumerate(out, 1):
        old = r['seq']
        r['seq'] = i
        if r in new:
            if r['kind'] == 'invoke':
                seq_of['A' if r['verb'] == 'fault_apply' else 'R'] = i
        else:
            seq_of[old] = i
    for r in out:
        if r['kind'] == 'complete':
            r['of'] = seq_of[r['of']]
    return out


class Faults(unittest.TestCase):
    def test_an_applied_and_restored_fault_passes(self):
        v = verdicts(with_fault(load()))
        for name in ('C0', 'C1', 'C2', 'C3', 'C7'):
            self.assertEqual(v[name], 'PASS', name)
        w = hc.fault_windows(hc.read_history(dump(with_fault(load()))))
        self.assertEqual(len(w), 1)
        self.assertIsNotNone(w[0][3])

    def test_a_history_that_ends_after_the_apply_is_fault_left_applied(self):
        v = hc.check(dump(with_fault(load(), restore=False)))
        self.assertEqual(v['C0']['verdict'], 'FAIL')
        self.assertTrue(any(x.startswith('fault left applied: vm_power/n2') for x in v['C0']['findings']), v['C0'])

    def test_a_restore_not_proven_is_fault_left_applied_and_a_runner_failure(self):
        v = hc.check(dump(with_fault(load(), restore_outcome='unknown')))
        self.assertEqual(v['C0']['verdict'], 'FAIL')
        joined = ' | '.join(v['C0']['findings'])
        self.assertIn('fault left applied', joined)
        self.assertIn('RUNNER failure, not a product result: fault_restore', joined)

    def test_an_unknown_fault_outcome_leaves_the_data_checkers_unknown(self):
        v = hc.check(dump(with_fault(load(), apply_outcome='unknown')))
        for name in ('C1', 'C2', 'C3'):
            self.assertEqual(v[name]['verdict'], 'UNKNOWN', name)
            self.assertIn('environment is not known', v[name]['findings'][0])
        self.assertTrue(any('RUNNER failure, not a product result: fault_apply' in x for x in v['C0']['findings']))

    def test_the_active_window_runs_from_the_apply_invoke_to_the_restore_complete(self):
        recs = hc.read_history(dump(with_fault(load())))
        (key, _, start, end), = hc.fault_windows(recs)
        inv = [r for r in recs if r['kind'] == 'invoke' and r['verb'] == 'fault_apply'][0]
        comp = [r for r in recs if r['kind'] == 'complete' and r['verb'] == 'fault_restore'][0]
        self.assertEqual((start, end), (inv['seq'], comp['seq']))


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

    def test_a_refusal_fails_unless_its_own_step_declared_it(self):
        recs = load()
        inv, comp = ops(recs, 'release')[0]
        comp['outcome'] = 'refused'
        self.assertEqual(verdicts(recs)['C0'], 'FAIL')
        inv.setdefault('args', {})['expect'] = 'refused'
        self.assertEqual(verdicts(recs)['C0'], 'PASS')

    def test_a_declaration_covers_its_step_not_its_verb(self):
        recs = load()
        # The run declares one create refusal (the re-create after delete);
        # the first create refused as well is not covered by it.
        first = ops(recs, 'create', 'v1')[0]
        first[1]['outcome'] = 'refused'
        self.assertEqual(verdicts(recs)['C0'], 'FAIL')

    def test_a_declared_refusal_answered_ok(self):
        recs = load()
        recreate = ops(recs, 'create', 'v1')[-1]
        self.assertEqual(recreate[0]['args'].get('expect'), 'refused')
        recreate[1]['outcome'] = 'ok'
        self.assertEqual(verdicts(recs)['C0'], 'FAIL')


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
        # The checkers judge only one-at-a-time histories: this overlap is
        # UNKNOWN for C1, C2 and C3, never a verdict.
        v = verdicts(recs)
        self.assertEqual((v['C1'], v['C2'], v['C3']), ('UNKNOWN', 'UNKNOWN', 'UNKNOWN'))
        # The window rule itself, for when overlaps are judged: the frozen
        # snapshot may hold the write that completed during it.
        sh, _, _ = hc.replay(recs)
        self.assertIn(w_inv['args']['sha256'], sh.frozen[('v1', 's1')][lba])

    def test_a_write_to_another_volume_or_refused_is_not_in_the_snapshot(self):
        def rec(seq, kind, verb, vol, **kw):
            r = {'seq': seq, 'kind': kind, 'op': 'X.Y', 'verb': verb, 'volume': vol, 'mono_ns': seq}
            r.update(kw)
            return r
        recs = [
            rec(1, 'invoke', 'snapshot', 'v1', args={'name': 's1'}),
            rec(2, 'invoke', 'write', 'vX', args={'lba': 0, 'sha256': 'c' * 64}),
            rec(3, 'complete', 'write', 'vX', of=2, outcome='ok'),
            rec(4, 'invoke', 'write', 'v1', args={'lba': 0, 'sha256': 'd' * 64}),
            rec(5, 'complete', 'write', 'v1', of=4, outcome='refused'),
            rec(6, 'complete', 'snapshot', 'v1', of=1, outcome='ok'),
        ]
        sh, _, _ = hc.replay(hc.read_history(dump(recs)))
        held = sh.frozen[('v1', 's1')].get(0, {hc.ZERO})
        self.assertNotIn('c' * 64, held, 'a write to another volume')
        self.assertNotIn('d' * 64, held, 'a refused write')
    def test_no_read_of_the_target_fails(self):
        recs = load()
        drop = {id(x) for p in ops(recs, 'read', 'v2') for x in p}
        recs = renumber([r for r in recs if id(r) not in drop])
        self.assertEqual(verdicts(recs)['C2'], 'FAIL')


def synth(*steps):
    """A one-at-a-time history from (verb, volume, args, outcome, result) steps,
    closed by the writer's end record."""
    recs = []
    for verb, vol, args, outcome, result in steps:
        n = len(recs)
        recs.append({'seq': n + 1, 'kind': 'invoke', 'op': 'X.Y', 'verb': verb, 'volume': vol, 'args': args, 'mono_ns': n + 1})
        recs.append({'seq': n + 2, 'kind': 'complete', 'of': n + 1, 'op': 'X.Y', 'verb': verb, 'volume': vol, 'mono_ns': n + 2, 'outcome': outcome, 'result': result})
    recs.append({'seq': len(recs) + 1, 'kind': 'end', 'mono_ns': len(recs) + 1})
    return recs


A, T, S = 'a' * 64, 't' * 64, 's' * 64
RESTORED = [('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}),
            ('snapshot', 'v1', {'name': 's1'}, 'ok', {}),
            ('restore', 'v2', {'source': 'v1', 'snapshot': 's1'}, 'ok', {})]


class Synthetic(unittest.TestCase):
    def test_c2_does_not_judge_a_block_the_target_wrote_first(self):
        recs = synth(*RESTORED,
                     ('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {}),
                     ('read', 'v2', {'lba': 0}, 'ok', {'sha256': T}))
        self.assertEqual(verdicts(recs)['C2'], 'FAIL', 'only a written block was read: nothing restored was seen')
        recs = synth(*RESTORED,
                     ('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {}),
                     ('read', 'v2', {'lba': 0}, 'ok', {'sha256': T}),
                     ('read', 'v2', {'lba': 1}, 'ok', {'sha256': hc.ZERO}))
        self.assertEqual(verdicts(recs)['C2'], 'PASS', 'the unwritten block is checked')

    def test_c3_cannot_tell_directions_apart_on_a_shared_hash(self):
        recs = synth(*RESTORED,
                     ('write', 'v1', {'lba': 0, 'sha256': S}, 'ok', {}),
                     ('write', 'v2', {'lba': 0, 'sha256': S}, 'ok', {}),
                     ('read', 'v2', {'lba': 0}, 'ok', {'sha256': S}),
                     ('read', 'v1', {'lba': 0}, 'ok', {'sha256': S}))
        self.assertEqual(verdicts(recs)['C3'], 'UNKNOWN')

    def test_an_overlap_is_unknown_for_the_data_checkers(self):
        recs = synth(*RESTORED, ('read', 'v2', {'lba': 0}, 'ok', {'sha256': A}))
        # Two operations on v1 open at once: the write invoked, a read
        # invoked before the write completes.
        w_inv, w_comp = recs[0], recs[1]
        r_inv = {'seq': 0, 'kind': 'invoke', 'op': 'Guest.Read', 'verb': 'read', 'volume': 'v1', 'args': {'lba': 0}, 'mono_ns': 0}
        r_comp = {'seq': 0, 'kind': 'complete', 'op': 'Guest.Read', 'verb': 'read', 'volume': 'v1', 'mono_ns': 0, 'outcome': 'ok', 'result': {'sha256': A}}
        recs = [w_inv, r_inv, w_comp, r_comp] + recs[2:]
        for i, r in enumerate(recs, 1):
            r['seq'], r['mono_ns'] = i, i
        w_comp['of'], r_comp['of'] = w_inv['seq'], r_inv['seq']
        for r in recs[4:]:
            if r['kind'] == 'complete':
                r['of'] = r['seq'] - 1
        self.assertEqual(hc.overlaps(hc.read_history(dump(recs))), {'v1'})
        v = verdicts(recs)
        self.assertEqual((v['C1'], v['C2'], v['C3']), ('UNKNOWN', 'UNKNOWN', 'UNKNOWN'), hc.check(dump(recs)))
        self.assertEqual(hc.overlaps(hc.read_history(dump(load()))), set(), 'the real run is one-at-a-time')

class Evidence(unittest.TestCase):
    """No checker passes on what it never saw (the lead's rule): an empty or
    truncated history, or one lacking the operations a checker judges."""

    def all_fail(self, lines, why):
        for name, v in hc.check(lines).items():
            self.assertEqual(v['verdict'], 'FAIL', (name, why, v))

    def test_empty_and_whitespace_histories_fail_every_checker(self):
        self.all_fail([], 'empty')
        self.all_fail(['', '   ', '\t'], 'whitespace only')

    def test_a_valid_prefix_without_the_end_record_fails_every_checker(self):
        recs = load()
        # Cut after the last read of the run: every operation so far is
        # complete, the stops, deletes and re-create are missing.
        last_read = max(c['seq'] for i, c in ops(recs, 'read'))
        self.all_fail(dump([r for r in recs if r['seq'] <= last_read]), 'prefix')

    def test_a_record_after_the_end_record_is_unreadable(self):
        recs = load()
        recs.append({'seq': len(recs) + 1, 'kind': 'observe', 'volume': 'v1', 'mono_ns': recs[-1]['mono_ns'], 'result': {}})
        for name, v in hc.check(dump(recs)).items():
            self.assertEqual(v['verdict'], 'UNKNOWN', name)

    def test_each_checker_fails_without_its_operations(self):
        only_end = synth()
        v = verdicts(only_end)
        # C5 too: a run without a refusal did not exercise it.
        self.assertEqual(v, {'C0': 'FAIL', 'C1': 'FAIL', 'C2': 'FAIL', 'C3': 'FAIL', 'C5': 'FAIL', 'C6': 'FAIL', 'C7': 'FAIL', 'DUR': 'FAIL', 'MO': 'FAIL'})
        # DUR: flushed writes read back, no kill: not exercised.
        flushed_only = synth(('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}),
                             ('read', 'v1', {'lba': 0}, 'ok', {'sha256': A}))
        self.assertEqual(verdicts(flushed_only)['DUR'], 'FAIL')
        self.assertIn('not exercised', hc.check(dump(flushed_only))['DUR']['findings'][0])
        writes_only = synth(('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}))
        self.assertEqual(verdicts(writes_only)['C1'], 'FAIL', 'no read')
        reads_only = synth(('read', 'v1', {'lba': 0}, 'ok', {'sha256': hc.ZERO}))
        self.assertEqual(verdicts(reads_only)['C1'], 'FAIL', 'no write')


class Binding(unittest.TestCase):
    def test_c2_refuses_a_snapshot_from_the_future(self):
        # The restore completes before any snapshot of that name exists; a
        # later snapshot of the name must not be taken for its image.
        recs = synth(('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}),
                     ('restore', 'v2', {'source': 'v1', 'snapshot': 's1'}, 'ok', {}),
                     ('snapshot', 'v1', {'name': 's1'}, 'ok', {}),
                     ('read', 'v2', {'lba': 0}, 'ok', {'sha256': A}))
        v = hc.check(dump(recs))
        self.assertEqual(v['C2']['verdict'], 'FAIL')
        self.assertIn('before the restore', v['C2']['findings'][0])

    def test_c2_binds_the_image_as_it_stood_at_the_restore(self):
        # s1 is taken again, holding B, after the restore: the target restored
        # the first s1 (A), so reading A is right and reading B is not.
        steps = [('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}),
                 ('snapshot', 'v1', {'name': 's1'}, 'ok', {}),
                 ('restore', 'v2', {'source': 'v1', 'snapshot': 's1'}, 'ok', {}),
                 ('write', 'v1', {'lba': 0, 'sha256': S}, 'ok', {}),
                 ('snapshot', 'v1', {'name': 's1'}, 'ok', {})]
        self.assertEqual(verdicts(synth(*steps, ('read', 'v2', {'lba': 0}, 'ok', {'sha256': A})))['C2'], 'PASS')
        self.assertEqual(verdicts(synth(*steps, ('read', 'v2', {'lba': 0}, 'ok', {'sha256': S})))['C2'], 'FAIL')


class Exercise(unittest.TestCase):
    """C3 must establish each direction: a write on one side, then a read of
    the same block on the other."""

    def c3(self, *steps):
        return verdicts(synth(*RESTORED, *steps))['C3']

    def test_both_directions_read_after_their_writes_pass(self):
        self.assertEqual(self.c3(('write', 'v1', {'lba': 0, 'sha256': S}, 'ok', {}),
                                 ('read', 'v2', {'lba': 0}, 'ok', {'sha256': A}),
                                 ('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {}),
                                 ('read', 'v1', {'lba': 0}, 'ok', {'sha256': S})), 'PASS')

    def test_reads_before_the_writes_exercise_nothing(self):
        self.assertEqual(self.c3(('read', 'v2', {'lba': 0}, 'ok', {'sha256': A}),
                                 ('read', 'v1', {'lba': 0}, 'ok', {'sha256': A}),
                                 ('write', 'v1', {'lba': 0, 'sha256': S}, 'ok', {}),
                                 ('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {})), 'FAIL')

    def test_reads_of_another_block_exercise_nothing(self):
        self.assertEqual(self.c3(('write', 'v1', {'lba': 0, 'sha256': S}, 'ok', {}),
                                 ('read', 'v2', {'lba': 7}, 'ok', {'sha256': hc.ZERO}),
                                 ('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {}),
                                 ('read', 'v1', {'lba': 7}, 'ok', {'sha256': hc.ZERO})), 'FAIL')

    def test_a_write_the_other_side_may_already_hold_exercises_nothing(self):
        # All ok, but the source writes A, the snapshot's own content: a
        # target reading A cannot tell isolation from propagation.
        self.assertEqual(self.c3(('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}),
                                 ('read', 'v2', {'lba': 0}, 'ok', {'sha256': A}),
                                 ('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {}),
                                 ('read', 'v1', {'lba': 0}, 'ok', {'sha256': A})), 'FAIL')

    def test_an_overwritten_distinguishing_value_exercises_nothing(self):
        # The reviewer's sequence: the source writes S, then A (the snapshot's
        # own content); the target reads A. S was overwritten, so that read
        # could not tell isolation from propagation.
        self.assertEqual(self.c3(('write', 'v1', {'lba': 0, 'sha256': S}, 'ok', {}),
                                 ('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}),
                                 ('read', 'v2', {'lba': 0}, 'ok', {'sha256': A}),
                                 ('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {}),
                                 ('read', 'v1', {'lba': 0}, 'ok', {'sha256': A})), 'FAIL')

    def test_the_full_six_action_trace_leaves_source_to_target_unexercised(self):
        # The reviewer's complete trace: the target's final read of T comes after
        # its own write T, which masks anything the source's A could have carried.
        v = self.c3(('write', 'v1', {'lba': 0, 'sha256': S}, 'ok', {}),
                    ('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}),
                    ('read', 'v2', {'lba': 0}, 'ok', {'sha256': A}),
                    ('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {}),
                    ('read', 'v1', {'lba': 0}, 'ok', {'sha256': A}),
                    ('read', 'v2', {'lba': 0}, 'ok', {'sha256': T}))
        self.assertEqual(v, 'FAIL')

    def test_an_unmasked_witness_after_the_readers_write_still_exercises(self):
        # At each block the reader wrote first and the other side's distinct
        # write came after it: both directions are exercised, one block each.
        self.assertEqual(self.c3(('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {}),
                                 ('write', 'v1', {'lba': 0, 'sha256': S}, 'ok', {}),
                                 ('read', 'v2', {'lba': 0}, 'ok', {'sha256': T}),
                                 ('write', 'v1', {'lba': 1, 'sha256': S}, 'ok', {}),
                                 ('write', 'v2', {'lba': 1, 'sha256': T}, 'ok', {}),
                                 ('read', 'v1', {'lba': 1}, 'ok', {'sha256': S})), 'PASS')
    def test_an_unknown_latest_write_exercises_nothing(self):
        self.assertEqual(self.c3(('write', 'v1', {'lba': 0, 'sha256': S}, 'ok', {}),
                                 ('write', 'v1', {'lba': 0, 'sha256': 'f' * 64}, 'unknown', {}),
                                 ('read', 'v2', {'lba': 0}, 'ok', {'sha256': A}),
                                 ('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {}),
                                 ('read', 'v1', {'lba': 0}, 'ok', {'sha256': S})), 'FAIL')

    def test_an_unknown_write_exercises_nothing(self):
        self.assertEqual(self.c3(('write', 'v1', {'lba': 0, 'sha256': S}, 'unknown', {}),
                                 ('read', 'v2', {'lba': 0}, 'ok', {'sha256': A}),
                                 ('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {}),
                                 ('read', 'v1', {'lba': 0}, 'ok', {'sha256': S})), 'FAIL')

    def test_a_refused_write_exercises_nothing(self):
        self.assertEqual(self.c3(('write', 'v1', {'lba': 0, 'sha256': S}, 'refused', {}),
                                 ('read', 'v2', {'lba': 0}, 'ok', {'sha256': A}),
                                 ('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {}),
                                 ('read', 'v1', {'lba': 0}, 'ok', {'sha256': A})), 'FAIL')


class C7(unittest.TestCase):
    def after_delete(self, recs, vol):
        at = ops(recs, 'delete', vol)[0][1]['seq']
        return [r for r in recs if r['kind'] == 'observe' and r.get('volume') == vol and r['seq'] > at][0]

    def test_the_real_run_passes(self):
        self.assertEqual(verdicts(load())['C7'], 'PASS', hc.check(dump(load()))['C7'])

    def test_a_generator_record_naming_the_volume_is_not_its_observation(self):
        # Host run 5: a gen_step for the next tail step named the deleted
        # volume before its observation; C7 must judge the observation.
        recs = load()
        at = ops(recs, 'delete', 'v1')[0][1]['seq']
        i = next(i for i, r in enumerate(recs) if r['seq'] > at)
        recs.insert(i, {'seq': 0, 'kind': 'observe', 'client': 'gen', 'verb': 'gen_step', 'volume': 'v1',
                        'mono_ns': recs[i - 1]['mono_ns'], 'result': {'action': 'v3_op_observe', 'params': {'volume': 'v1'}}})
        self.assertEqual(verdicts(renumber(recs))['C7'], 'PASS', hc.check(dump(renumber(recs)))['C7'])

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

    def test_a_create_of_the_deleted_name_that_is_let_through(self):
        recs = load()
        inv, comp = ops(recs, 'create', 'v1')[-1]
        self.assertGreater(inv['seq'], ops(recs, 'delete', 'v1')[0][1]['seq'])
        comp['outcome'], comp['result'] = 'ok', {'layout_id': 'b' * 32}
        v = hc.check(dump(recs))
        self.assertEqual(v['C7']['verdict'], 'FAIL')
        self.assertIn('not refused', v['C7']['findings'][0])

    def test_a_create_of_the_deleted_name_refused_for_another_reason(self):
        recs = load()
        comp = ops(recs, 'create', 'v1')[-1][1]
        comp['result']['detail'] = 'rpc error: code = Unavailable desc = master down'
        self.assertEqual(verdicts(recs)['C7'], 'FAIL')
    def test_a_re_create_refused_for_another_precondition(self):
        recs = load()
        comp = ops(recs, 'create', 'v1')[-1][1]
        comp['result']['detail'] = 'rpc error: code = FailedPrecondition desc = create volume: size below the minimum'
        v = hc.check(dump(recs))
        self.assertEqual(v['C7']['verdict'], 'FAIL')
        self.assertIn('reservation', v['C7']['findings'][0])

    def test_no_re_create_attempted(self):
        recs = load()
        inv, comp = ops(recs, 'create', 'v1')[-1]
        recs = renumber([r for r in recs if r is not inv and r is not comp])
        v = hc.check(dump(recs))
        self.assertEqual(v['C7']['verdict'], 'FAIL')
        self.assertTrue(any('not exercised' in x for x in v['C7']['findings']))

    def test_no_delete_at_all(self):
        recs = load()
        drop = {id(x) for p in ops(recs, 'delete') for x in p}
        recs = renumber([r for r in recs if id(r) not in drop])
        self.assertEqual(verdicts(recs)['C7'], 'FAIL')

    def test_a_delete_never_observed(self):
        recs = load()
        at = ops(recs, 'delete', 'v1')[0][1]['seq']
        recs = renumber([r for r in recs if not (r['kind'] == 'observe' and r.get('volume') == 'v1' and r['seq'] > at)])
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

    def test_a_direction_not_exercised_fails(self):
        recs = load()
        drop = {id(x) for p in ops(recs, 'write', 'v2') for x in p}
        recs = renumber([r for r in recs if id(r) not in drop])
        v = hc.check(dump(recs))
        self.assertEqual(v['C3']['verdict'], 'FAIL')
        self.assertTrue(any('not exercised' in x for x in v['C3']['findings']))


def ws_text(volume='v1', state='granted', epoch=1, replica='r1', ack='none', validity=14849887534, lease=True):
    """The lines `ops workspace show` prints (exit 0). The lease expiry and the
    validity at query change on their own; the checker ignores them."""
    lines = ['volume: %s (query snapshot, not authority to serve)' % volume]
    if lease:
        lines += ['replica: %s' % replica, 'epoch: %d' % epoch, 'lease state: %s' % state,
                  'holder: %s@%d (logical replica)' % (replica, epoch),
                  'lease expiry_ns: 1795738167151366324; host validity_ns at query: %d' % validity]
    else:
        lines += ['replica: none', 'epoch: none', 'lease state: none', 'holder: none']
    return lines + ['recorded ACK: %s' % ack,
                    'host: unknown (not recorded by the master); issued moves and bindings below are separate facts']


def history(*steps):
    """A one-at-a-time history: ('obs', volume, gen, files) is an observation
    (gen or files None: not recorded), any other step is (verb, volume, args,
    outcome) as an invoke and its complete. Closed by the end record."""
    recs = []
    for st in steps:
        n = len(recs)
        if st[0] == 'obs':
            _, vol, gen, files = st[:4]
            # Without an extra dict, the control plane's answer C5 requires.
            res = dict(st[4]) if len(st) > 4 else {'workspace': {'exit': 0, 'text': ws_text(validity=10**10 - n)}}
            res['head'] = {'gen': gen, 'epoch': 1, 'holder': 'a/r1', 'state': ''} if gen is not None else 'unreadable'
            if files is not None:
                res['files'] = list(files)
            else:
                res['listing_error'] = 'filer down'
            # The reading began at the clock n (from_mono_ns) and was stamped at n + 1.
            recs.append({'seq': n + 1, 'kind': 'observe', 'verb': 'observe', 'volume': vol, 'from_mono_ns': n, 'mono_ns': n + 1, 'result': res})
            continue
        verb, vol, args, outcome = st
        recs.append({'seq': n + 1, 'kind': 'invoke', 'op': 'X.Y', 'verb': verb, 'volume': vol, 'args': args, 'mono_ns': n + 1})
        recs.append({'seq': n + 2, 'kind': 'complete', 'of': n + 1, 'op': 'X.Y', 'verb': verb, 'volume': vol, 'mono_ns': n + 2, 'outcome': outcome, 'result': {}})
    recs.append({'seq': len(recs) + 1, 'kind': 'end', 'mono_ns': len(recs) + 1})
    return recs


FILES = ['HEAD', 'VOLUME', 'seg/0-x']
NEG = {'expect': 'refused'}


BASE = (('obs', 'v0', 1, FILES), ('obs', 'v0', 1, FILES))   # a baseline pair on another volume
WS = {'workspace': {'exit': 0, 'text': ws_text()}, 'dirs': ['v1-0123abcd'], 'snapshots': []}


class C5(unittest.TestCase):
    def c5(self, *steps, base=True):
        return hc.check(dump(history(*((BASE if base else ()) + steps))))['C5']

    def test_the_real_run_judges_its_refused_create_but_has_no_baseline(self):
        v = hc.check(dump(load()))['C5']
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertIn('baseline not established', v['findings'][0])
        self.assertIn('limit ordinary create: 1 of 1 refusal(s) judged', v['findings'])

    def test_unchanged_passes(self):
        v = self.c5(('obs', 'v1', 4, FILES, WS), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, WS))
        self.assertEqual(v['verdict'], 'PASS', v)
        self.assertEqual(v['findings'], ['limit ordinary write: 1 of 1 refusal(s) judged'])

    def test_a_moved_generation_fails(self):
        v = self.c5(('obs', 'v1', 4, FILES), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 5, FILES))
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('seq 4 write v1', v['findings'][0])
        self.assertIn('its head changed', v['findings'][0])

    def test_a_listing_that_gained_or_lost_a_file_fails(self):
        for after in (FILES + ['pool/0/1-y'], FILES[:2]):
            v = self.c5(('obs', 'v1', 4, FILES), ('snapshot', 'v1', NEG, 'refused'), ('obs', 'v1', 4, after))
            self.assertEqual(v['verdict'], 'FAIL', (after, v))
            self.assertIn('seq 4 snapshot v1', v['findings'][0])
            self.assertIn('its files changed', v['findings'][0])

    def test_a_control_plane_change_fails(self):
        # A refused delete that left the control plane changed (e.g. reserved).
        moved = {'workspace': {'exit': 0, 'text': ws_text(state='revoking', ack='r1@1 HEAD generation 4')}, 'dirs': WS['dirs'], 'snapshots': []}
        v = self.c5(('obs', 'v1', 4, FILES, WS), ('delete', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, moved))
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('its workspace changed', v['findings'][0])
        # HEAD's epoch, holder or state counts too, not only its generation.
        recs = history(*(BASE + (('obs', 'v1', 4, FILES, WS), ('delete', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, WS))))
        recs[-2]['result']['head']['state'] = 'revoking'
        self.assertEqual(hc.check(dump(recs))['C5']['verdict'], 'FAIL')

    def test_a_refused_create_of_a_never_existing_name_is_judged_on_no_directory(self):
        # No HEAD (a show error) and no directory are observations too.
        none = {'show_error': {'exit': 1, 'code': 'not_found'}, 'dirs': [], 'workspace': {'exit': 1, 'text': 'v9 not found'}}
        recs = history(*(BASE + (('obs', 'v9', 4, FILES), ('create', 'v9', NEG, 'refused'), ('obs', 'v9', 4, FILES))))
        for i in (-5, -2):
            recs[i]['result'] = dict(none)
        v = hc.check(dump(recs))['C5']
        self.assertEqual(v['verdict'], 'PASS', v)
        recs[-2]['result'] = dict(none, dirs=['v9-0badc0de'])
        v = hc.check(dump(recs))['C5']
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('its dirs changed', v['findings'][0])

    def test_red_a_refusal_that_changed_the_snapshot_names(self):
        snaps = dict(WS, snapshots=['s1'])
        unread = {k: v for k, v in WS.items() if k != 'snapshots'}
        unread['snapshots_error'] = 'chunk show: HEAD unreadable'
        v = self.c5(('obs', 'v1', 4, FILES, snaps), ('snapshot_delete', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, snaps))
        self.assertEqual(v['verdict'], 'PASS', v)
        v = self.c5(('obs', 'v1', 4, FILES, snaps), ('snapshot_delete', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, dict(WS, snapshots=[])))
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('its snapshots changed', v['findings'][0])
        # an observation that could not read the names proves nothing either way
        v = self.c5(('obs', 'v1', 4, FILES, snaps), ('snapshot_delete', 'v1', NEG, 'refused'),
                    ('obs', 'v1', 4, FILES, unread))
        self.assertEqual(v['verdict'], 'UNKNOWN', v)

    def test_a_part_in_one_observation_only_is_not_judged(self):
        v = self.c5(('obs', 'v1', 4, FILES, WS), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES))
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertTrue(any('in one observation only' in x for x in v['findings']), v)

    def test_baseline(self):
        # Background change between two observations with nothing between: UNKNOWN, even over a change at a refusal.
        drift = (('obs', 'v0', 1, FILES), ('obs', 'v0', 2, FILES))
        v = self.c5(*(drift + (('obs', 'v1', 4, FILES), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 5, FILES))), base=False)
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertIn('baseline: v0 changed', v['findings'][0])
        # No pair at all: UNKNOWN.
        v = self.c5(('obs', 'v1', 4, FILES), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES), base=False)
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertIn('baseline not established', v['findings'][0])
        # A refused operation between two observations makes them the refusal's pair, not a baseline pair.
        v = self.c5(('obs', 'v1', 4, FILES), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES), ('obs', 'v1', 4, FILES), base=False)
        self.assertEqual(v['verdict'], 'PASS', v)

    def test_the_workspace_lease_clock_is_no_baseline_change(self):
        # From a host run: the lease expiry and the validity at query change
        # between two queries with nothing between them; the baseline holds.
        tick = lambda n: {'workspace': {'exit': 0, 'text': ws_text(validity=n)}, 'dirs': WS['dirs'], 'snapshots': []}
        v = self.c5(('obs', 'v0', 1, FILES, tick(14849887534)), ('obs', 'v0', 1, FILES, tick(14716527984)),
                    ('obs', 'v1', 4, FILES, tick(14600000000)), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, tick(14500000000)), base=False)
        self.assertEqual(v['verdict'], 'PASS', v)
        # red control: the lease state moving is a baseline change
        v = self.c5(('obs', 'v0', 1, FILES, tick(1)), ('obs', 'v0', 1, FILES, dict(tick(2), workspace={'exit': 0, 'text': ws_text(state='revoking')})),
                    ('obs', 'v1', 4, FILES, tick(3)), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, tick(4)), base=False)
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertIn('baseline: v0 changed with no operation on it (workspace;', v['findings'][0])

    def test_workspace_stderr_is_no_baseline_change(self):
        # The observation keeps the first lines of the show's stderr (why a
        # show failed). Comparisons ignore it: two observations that differ
        # only in stderr show the same state, for a successful and a failed
        # show alike.
        ok = lambda err: {'workspace': {'exit': 0, 'text': ws_text(), 'stderr': err}, 'dirs': WS['dirs'], 'snapshots': []}
        v = self.c5(('obs', 'v0', 1, FILES, ok(['warn: a'])), ('obs', 'v0', 1, FILES, ok(['warn: b', 'more'])),
                    ('obs', 'v1', 4, FILES, ok([''])), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, ok(['other'])), base=False)
        self.assertEqual(v['verdict'], 'PASS', v)
        failed = lambda err: {'workspace': {'exit': 1, 'text': [''], 'stderr': err}, 'dirs': WS['dirs'], 'snapshots': []}
        for part_state in (ok, failed):
            a, b = part_state(['master refused: v1 is being deleted']), part_state(['master did not answer'])
            sa, _ = pb.observed_state({'result': a}, ['workspace'])
            sb, _ = pb.observed_state({'result': b}, ['workspace'])
            self.assertEqual(sa, sb)
            self.assertTrue(pb.same_part('workspace', sa['workspace'], sb['workspace']))
        # red control: a different exit status is a different state.
        sa, _ = pb.observed_state({'result': ok([''])}, ['workspace'])
        sb, _ = pb.observed_state({'result': failed([''])}, ['workspace'])
        self.assertNotEqual(sa, sb)

    def test_a_lease_running_out_by_time_is_no_baseline_change(self):
        # From host runs: revoking -> expired on a stopped volume, granted ->
        # expired on a killed holder's volume, nothing in between. The two
        # readings lie within clock 0..2 (the first began at 0, the second was
        # stamped at 2), so a validity up to 2 can run out between them.
        lease = lambda state, n: {'workspace': {'exit': 0, 'text': ws_text(state=state, validity=n)}, 'dirs': WS['dirs'], 'snapshots': []}
        steps = lambda a, b, n=2: (('obs', 'v0', 1, FILES, lease(a, n)), ('obs', 'v0', 1, FILES, lease(b, 0)),
                                   ('obs', 'v1', 4, FILES, WS), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, WS))
        for a in ('revoking', 'granted'):
            v = self.c5(*steps(a, 'expired'), base=False)
            self.assertEqual(v['verdict'], 'PASS', (a, v))
            # red control: a validity longer than the readings' window did not run out by time
            v = self.c5(*steps(a, 'expired', 3), base=False)
            self.assertEqual(v['verdict'], 'UNKNOWN', (a, v))
            self.assertIn('baseline: v0 changed with no operation on it (workspace;', v['findings'][0])
        # red control: the reverse is a change
        v = self.c5(*steps('expired', 'revoking'), base=False)
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertIn('baseline: v0 changed with no operation on it (workspace;', v['findings'][0])

    def test_a_garbled_workspace_text_leaves_the_refusal_not_judged(self):
        bad = dict(WS, workspace={'exit': 0, 'text': ['lease state granted']})
        v = self.c5(('obs', 'v1', 4, FILES, WS), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, bad))
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertTrue(any('not judged: seq 4 write v1' in x and 'key: value' in x for x in v['findings']), v)

    def test_every_limit_must_be_judged(self):
        # One judged refusal does not carry another limit that was never judged.
        v = self.c5(('obs', 'v1', 4, FILES), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES), ('snapshot', 'v2', NEG, 'refused'))
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertIn('limit(s) with no judged refusal: ordinary snapshot', v['findings'][0])
        self.assertIn('limit ordinary write: 1 of 1 refusal(s) judged', v['findings'])

    def test_limits_come_from_the_generators_step(self):
        recs = history(*(BASE + (('obs', 'v1', 4, FILES), ('delete', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES))))
        recs.insert(-4, {'seq': 0, 'kind': 'observe', 'client': 'gen', 'verb': 'gen_step', 'volume': 'v1', 'mono_ns': recs[-5]['mono_ns'],
                         'result': {'opno': 1, 'action': 'v3_op_delete', 'negative': 'delete while attached'}})
        v = hc.check(dump(renumber(recs)))['C5']
        self.assertEqual(v['verdict'], 'PASS', v)
        self.assertIn('limit delete while attached: 1 of 1 refusal(s) judged', v['findings'])

    def test_red_a_negative_answered_ok_fails(self):
        v = self.c5(('obs', 'v1', 4, FILES), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES), ('delete', 'v1', NEG, 'ok'))
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertTrue(v['findings'][0].startswith('negative not refused: seq 7 delete v1'), v)
        self.assertTrue(v['findings'][0].endswith('answered ok'), v)

    def test_a_negative_answered_unknown_is_a_note(self):
        v = self.c5(('obs', 'v1', 4, FILES), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES), ('delete', 'v1', NEG, 'unknown'))
        self.assertEqual(v['verdict'], 'PASS', v)
        self.assertTrue(any(x.startswith('negative not refused: seq 7 delete v1') and x.endswith('answered unknown') for x in v['findings']), v)

    def test_red_a_required_part_recorded_by_no_observation_allows_no_pass(self):
        # Neither observation records the workspace answer: not judged, so
        # the limit has no judged refusal and C5 cannot PASS.
        nows = {'dirs': WS['dirs'], 'snapshots': []}
        v = self.c5(('obs', 'v1', 4, FILES, nows), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, nows))
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertTrue(any('not judged: seq 4 write v1' in x and 'record no workspace (required)' in x for x in v['findings']), v)
        # nor HEAD (no head and no show error in either)
        recs = history(*(BASE + (('obs', 'v1', 4, FILES, WS), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, WS))))
        for i in (-5, -2):
            del recs[i]['result']['head']
        v = hc.check(dump(recs))['C5']
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertTrue(any('record no head (required)' in x for x in v['findings']), v)
        self.assertEqual(hc.REQUIRED_PARTS, ('head', 'workspace'))

    def test_no_observation_after_is_not_judged(self):
        v = self.c5(('obs', 'v1', 4, FILES), ('write', 'v1', NEG, 'refused'))
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertTrue(any('not judged: seq 4 write v1' in x and 'after' in x for x in v['findings']), v)
        # An operation on the volume invoked before the observation voids it as "after".
        v = self.c5(('obs', 'v1', 4, FILES), ('write', 'v1', NEG, 'refused'), ('write', 'v1', {}, 'ok'), ('obs', 'v1', 5, FILES))
        self.assertEqual(v['verdict'], 'UNKNOWN', v)

    def test_a_stop_refused_with_detach_aborted_must_leave_the_volume_serving(self):
        # detach_aborted: "nothing was published and serving resumed here".
        # The observation after it must show what the one before showed: HEAD
        # unsealed and the workspace still serving.
        def run(after_ws=WS, after_state=''):
            recs = history(*(BASE + (('obs', 'v1', 4, FILES, WS), ('stop', 'v1', {}, 'refused'), ('obs', 'v1', 4, FILES, after_ws))))
            recs[-3]['result'] = {'detail': 'detach: detach_aborted: nothing was published and serving resumed here'}
            recs[-2]['result']['head']['state'] = after_state
            return hc.check(dump(recs))['C5']
        v = run()
        self.assertEqual(v['verdict'], 'PASS', v)
        self.assertEqual(v['findings'], ['limit ordinary stop: 1 of 1 refusal(s) judged'])
        # Red controls: a revoked or not-serving workspace, or a sealed HEAD, after it.
        for name, ws, state, part in (
                ('revoked', dict(WS, workspace={'exit': 0, 'text': ws_text(state='revoking')}), '', 'workspace'),
                ('no live lease', dict(WS, workspace={'exit': 0, 'text': ws_text(lease=False, ack='r1@1 HEAD generation 4')}), '', 'workspace'),
                ('workspace show failing', dict(WS, workspace={'exit': 3, 'text': ws_text()}), '', 'workspace'),
                ('HEAD sealed', WS, 'detached', 'head')):
            v = run(ws, state)
            self.assertEqual(v['verdict'], 'FAIL', (name, v))
            self.assertIn('seq 4 stop v1 (ordinary stop) refused but its %s changed' % part, v['findings'][0], name)

    def test_an_ok_operation_between_before_and_the_refusal_voids_before(self):
        # The ok write moved the generation from 4 to 5: comparing against the
        # stale observation would blame the refusal for it.
        v = self.c5(('obs', 'v1', 4, FILES), ('write', 'v1', {}, 'ok'), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 5, FILES))
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertTrue(any('not judged: seq 6 write v1' in x and 'before' in x for x in v['findings']), v)
        # An operation on another volume does not void it.
        v = self.c5(('obs', 'v1', 4, FILES), ('write', 'v2', {}, 'ok'), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES))
        self.assertEqual(v['verdict'], 'PASS', v)

    def test_an_observation_without_head_or_listing_is_not_judged(self):
        for after in (('obs', 'v1', None, FILES), ('obs', 'v1', 4, None)):
            v = self.c5(('obs', 'v1', 4, FILES), ('write', 'v1', NEG, 'refused'), after)
            self.assertEqual(v['verdict'], 'UNKNOWN', (after, v))

    def test_generator_records_are_not_observations(self):
        recs = history(*(BASE + (('obs', 'v1', 4, FILES), ('write', 'v1', NEG, 'refused'))))
        recs.insert(-1, {'seq': 0, 'kind': 'observe', 'client': 'gen', 'verb': 'gen_step', 'volume': 'v1', 'mono_ns': recs[-1]['mono_ns'],
                         'result': {'head': {'gen': 4}, 'files': FILES}})
        self.assertEqual(hc.check(dump(renumber(recs)))['C5']['verdict'], 'UNKNOWN')

    # refused_effect: the documented state after it, every other part unchanged
    # PHRASE stands for the product's exact words for a volume reserved for
    # deletion (the outcome-map row will carry the real phrase and its cite).
    PHRASE = 'PLACEHOLDER reserved for deletion'
    EFFECT = {'part': 'workspace', 'to': {'exit': 1, 'stderr_contains': PHRASE},
              'cite': 'docs/operator-guide.md:193-196; cmd/sw-block/workspace.go:18'}
    REFUSED_WS = dict(WS, workspace={'exit': 1, 'text': [''], 'stderr': ['sw-block ops workspace: rpc error: ' + PHRASE]})

    def effect_history(self, after_gen=4, after=None, effect=EFFECT, expect=True, before=None, repeat=False):
        """A clean pair around a delete answered refused_effect with `effect`;
        repeat: a second such delete and an observation after it."""
        after = self.REFUSED_WS if after is None else after
        steps = [('obs', 'v1', 4, FILES, WS if before is None else before), ('delete', 'v1', NEG if expect else {}, 'refused'),
                 ('obs', 'v1', after_gen, FILES, after)]
        if repeat:
            steps += [('delete', 'v1', NEG, 'refused'), ('obs', 'v1', after_gen, FILES, after)]
        recs = history(*(BASE + tuple(steps)))
        for comp in [r for r in recs if r['kind'] == 'complete' and r['verb'] == 'delete']:
            comp['outcome'] = 'refused_effect'
            comp['result'] = {'detail': 'snapshot_has_dependents: layout: a snapshot has dependents'}
            if effect is not None:
                comp['result']['effect'] = effect
        return recs

    def test_a_refused_effect_in_its_documented_state_passes(self):
        v = hc.check(dump(self.effect_history()))
        self.assertEqual(v['C5']['verdict'], 'PASS', v['C5'])
        self.assertEqual(v['C5']['findings'], ['limit ordinary delete: 1 of 1 refusal(s) judged'])
        # C0 takes it as a known outcome, with no note
        self.assertEqual((v['C0']['verdict'], v['C0']['findings']), ('PASS', []))
        # C6 has nothing to resolve: it is not an unknown
        self.assertIn('not exercised', v['C6']['findings'][0])

    def test_a_repeated_refused_effect_on_a_part_already_in_its_state_passes(self):
        # The second refused delete of a reserved volume sees the workspace
        # already refused, before and after: correct product behaviour.
        v = hc.check(dump(self.effect_history(repeat=True)))['C5']
        self.assertEqual(v['verdict'], 'PASS', v)
        self.assertEqual(v['findings'], ['limit ordinary delete: 2 of 2 refusal(s) judged'])

    def test_red_refused_effect_another_part_changed(self):
        v = hc.check(dump(self.effect_history(after_gen=5)))['C5']
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('seq 4 delete v1 (ordinary delete) refused_effect but its head changed', v['findings'][0])
        self.assertEqual(len([x for x in v['findings'] if 'refused_effect but' in x]), 1, 'the effect part itself is no finding')

    def test_red_refused_effect_its_part_lacks_the_documented_state(self):
        # unchanged (still answering exit 0)
        v = hc.check(dump(self.effect_history(after=WS)))['C5']
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('refused_effect but its workspace is not in the documented state: workspace.exit is 0, effect 1', v['findings'][0])
        # exit 1, but for another reason: a master down is not the reservation
        down = dict(WS, workspace={'exit': 1, 'text': [''], 'stderr': ['rpc error: code = Unavailable desc = connection refused']})
        v = hc.check(dump(self.effect_history(after=down)))['C5']
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn("workspace.stderr does not contain 'PLACEHOLDER reserved for deletion'", v['findings'][0])
        # exit 3 against exit 1
        v = hc.check(dump(self.effect_history(after=dict(self.REFUSED_WS, workspace=dict(self.REFUSED_WS['workspace'], exit=3)))))['C5']
        self.assertIn('workspace.exit is 3, effect 1', v['findings'][0])

    def test_a_condition_the_observation_cannot_evaluate_is_not_judged(self):
        # no stderr recorded: never a pass
        nostderr = dict(WS, workspace={'exit': 1, 'text': ['']})
        v = hc.check(dump(self.effect_history(after=nostderr)))['C5']
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertTrue(any('not judged: seq 4 delete v1: observation seq 6 cannot show its effect: workspace.stderr not recorded' in x
                            for x in v['findings']), v)
        # the loose "exit non-zero" is not an accepted condition
        v = hc.check(dump(self.effect_history(effect=dict(self.EFFECT, to={'exit_nonzero': True}))))['C5']
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertTrue(any("condition 'exit_nonzero' not accepted" in x for x in v['findings']), v)
        # but another part changing still fails, evaluable or not
        v = hc.check(dump(self.effect_history(after=nostderr, after_gen=5)))['C5']
        self.assertEqual(v['verdict'], 'FAIL', v)

    def test_red_refused_effect_without_its_effect_is_unreadable(self):
        # The runner copies the effect from the outcome map; a record without
        # it breaks the writer's own rule: nothing is judged, by any checker.
        for bad in (None, {'part': 'workspace', 'to': {'exit': 1}}, {'to': {}, 'cite': 'x'}):
            v = hc.check(dump(self.effect_history(effect=bad)))
            for name, r in v.items():
                self.assertEqual(r['verdict'], 'UNKNOWN', (bad, name, r))
            self.assertIn('refused_effect without result.effect', v['C5']['findings'][0])

    def test_refused_effect_is_a_refusal_for_the_declaration_rule(self):
        v = hc.check(dump(self.effect_history(expect=False)))['C0']
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('refusal not expected: delete v1 seq 4', v['findings'][0])

    def test_effect_state_reads_list_parts_and_refuses_unknown_conditions(self):
        a = {'result': {'files': ['HEAD', 'VOLUME', 'root/a'], 'workspace': {'exit': 1, 'text': ['']}}}
        es = hc.PRODUCT.effect_state
        self.assertEqual(es('files', {'has': ['HEAD'], 'lacks': ['seg/x']}, a), ([], []))
        self.assertTrue(es('files', {'lacks': ['root/a']}, a)[0])
        self.assertTrue(es('files', {'added': ['root/a']}, a)[1], 'a difference is not a state')
        self.assertTrue(es('workspace', {'exit_zero': True}, a)[1])
        self.assertTrue(es('workspace', {}, a)[1])
        self.assertEqual(es('workspace', {'exit': 1, 'text_contains': ''}, a), ([], []))

    def test_no_refusal_fails_not_exercised(self):
        v = self.c5(('obs', 'v1', 4, FILES), ('write', 'v1', {}, 'ok'), ('obs', 'v1', 5, FILES))
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('not exercised', v['findings'][0])


B, C, JUNK = 'b' * 64, 'c' * 64, 'f' * 64
# A holder kill of v1 (args.volume names it), a master kill, and a kill
# naming no volume (an older history): applied to every volume.
HKILL = ('kill', '', {'process': 'blockvolume:0', 'volume': 'v1', 'pid': 4242, 'signal': 'KILL'}, 'ok', {})
MKILL = ('kill', '', {'process': 'blockmaster', 'pid': 4141, 'signal': 'KILL'}, 'ok', {})
AKILL = ('kill', '', {'process': 'blockvolume:0', 'pid': 4242, 'signal': 'KILL'}, 'ok', {})


def w(lba, sha, outcome='ok', flush=True, vol='v1'):
    args = {'lba': lba, 'sha256': sha, 'round': 'g1'}
    if not flush:
        args['flush'] = False
    return ('write', vol, args, outcome, {})


def rd(lba, sha, vol='v1'):
    return ('read', vol, {'lba': lba}, 'ok', {'sha256': sha})


def fl(outcome='ok', vol='v1'):
    return ('flush', vol, {}, outcome, {})


class DUR(unittest.TestCase):
    def dur(self, *steps):
        return hc.check(dump(synth(*steps)))['DUR']

    def assert_pass(self, v, lost):
        self.assertEqual(v['verdict'], 'PASS', v)
        self.assertIn('acknowledged, not flushed, lost: %d' % lost, v['findings'])

    def assert_fail(self, v, why):
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertTrue(any(why in x for x in v['findings']), v)

    # Green: what the pages allow.
    def test_a_lost_unflushed_write_passes_and_is_counted(self):
        self.assert_pass(self.dur(w(0, A), w(0, B, flush=False), HKILL, rd(0, A)), lost=1)

    def test_a_kept_unflushed_write_passes_and_nothing_is_lost(self):
        self.assert_pass(self.dur(w(0, A), w(0, B, flush=False), HKILL, rd(0, B)), lost=0)

    def test_an_unflushed_write_of_a_new_volume_may_vanish_to_zero(self):
        self.assert_pass(self.dur(w(0, B, flush=False), HKILL, rd(0, hc.ZERO)), lost=1)

    def test_an_unknown_write_after_the_durable_one_is_allowed(self):
        self.assert_pass(self.dur(w(0, A), w(0, B, outcome='unknown'), HKILL, rd(0, B)), lost=0)
        self.assert_pass(self.dur(w(0, A), w(0, B, outcome='unknown'), HKILL, rd(0, A)), lost=0)

    def test_the_lost_count_is_every_acknowledged_write_after_the_value_read(self):
        steps = [w(0, A), w(0, B, flush=False), w(0, C, flush=False), HKILL]
        self.assert_pass(self.dur(*steps, rd(0, A)), lost=2)
        self.assert_pass(self.dur(*steps, rd(0, B)), lost=1)

    def test_a_refused_kill_is_no_kill(self):
        refused = ('kill', '', HKILL[2], 'refused', {})
        # No kill: C1's rule, and the unflushed write must read back.
        self.assert_fail(self.dur(w(0, A), w(0, B, flush=False), refused, rd(0, A)), 'not one of')

    # Red controls: the seeded defects DUR must catch.
    def test_red_an_older_value_than_the_last_durable_one(self):
        # B is durable through the acknowledged flush; A reappears after the kill.
        self.assert_fail(self.dur(w(0, A), w(0, B, flush=False), fl(), HKILL, rd(0, A)), 'an older value than the last durable one')
        # The same with a flushed write as the durable one.
        self.assert_fail(self.dur(w(0, A), w(0, B), HKILL, rd(0, A)), 'an older value than the last durable one')

    def test_red_content_no_write_produced(self):
        self.assert_fail(self.dur(w(0, A), w(0, B, flush=False), HKILL, rd(0, JUNK)), 'content no write produced')

    def test_red_zero_when_a_durable_write_exists(self):
        self.assert_fail(self.dur(w(0, B, flush=False), fl(), HKILL, rd(0, hc.ZERO)), 'ZERO although a durable write exists')

    def test_red_an_unknown_flush_is_not_treated_as_durable(self):
        # With the flush acknowledged, reading A after the kill is a FAIL; with
        # the flush unknown nothing became durable, so A (the flushed write) is
        # allowed and B counted lost. A checker that took the unknown flush for
        # an acknowledged one fails the second history.
        self.assert_fail(self.dur(w(0, A), w(0, B, flush=False), fl('ok'), HKILL, rd(0, A)), 'older value')
        self.assert_pass(self.dur(w(0, A), w(0, B, flush=False), fl('unknown'), HKILL, rd(0, A)), lost=1)
        self.assert_pass(self.dur(w(0, A), w(0, B, flush=False), fl('refused'), HKILL, rd(0, A)), lost=1)

    def test_red_a_flush_of_another_volume_makes_nothing_durable(self):
        self.assert_pass(self.dur(w(0, A), w(0, B, flush=False), fl(vol='v9'), HKILL, rd(0, A)), lost=1)

    def test_red_a_write_before_the_last_durable_one_does_not_come_back(self):
        # After C is durable (a flushed write), a second kill may not bring
        # back A, allowed after the first kill.
        self.assert_fail(self.dur(w(0, A), w(0, B, flush=False), HKILL, rd(0, A), w(0, C), HKILL, rd(0, A)), 'older value')

    def test_an_unknown_write_may_land_after_a_later_durable_one_and_is_counted(self):
        # r1: an unknown write invoked BEFORE the last durable value may land
        # after it while its holder lives; after the kill its value is allowed.
        v = self.dur(w(0, A, outcome='unknown'), w(0, B), HKILL, rd(0, A))
        self.assert_pass(v, lost=0)
        self.assertIn('unknown write landed after a later durable write: 1', v['findings'])
        # The same without a kill (C1's shadow keeps it possible).
        v = self.dur(w(0, B, flush=False), w(0, A, outcome='unknown'), w(0, B), rd(0, A))
        self.assert_pass(v, lost=0)
        self.assertIn('unknown write landed after a later durable write: 1', v['findings'])
        # It did not land: nothing counted.
        v = self.dur(w(0, A, outcome='unknown'), w(0, B), HKILL, rd(0, B))
        self.assertIn('unknown write landed after a later durable write: 0', v['findings'])

    def test_red_an_unknown_settled_by_a_kill_does_not_land_later(self):
        # The kill settles A (read B); a second kill may not bring A back.
        self.assert_fail(self.dur(w(0, A, outcome='unknown'), w(0, B), HKILL, rd(0, B), HKILL, rd(0, A)), 'older value')
        # An unknown invoked after the durable point, settled as not taken
        # effect (read ZERO), does not come back at a second kill.
        self.assert_fail(self.dur(w(0, A, outcome='unknown'), HKILL, rd(0, hc.ZERO), HKILL, rd(0, A)), 'no open unknown explains')

    # Review r1, change 1: the first read after a kill fixes what the next
    # kill may leave (the durable set before the kill plus the value read).
    def test_red_a_write_lost_at_one_kill_does_not_come_back_at_the_next(self):
        # A flushed, B acknowledged unflushed; kill, read A (B lost); kill,
        # read B: B was lost and comes back.
        self.assert_fail(self.dur(w(0, A), w(0, B, flush=False), HKILL, rd(0, A), HKILL, rd(0, B)), 'no open unknown explains')
        self.assert_fail(self.dur(w(0, A), w(0, B, flush=False), w(0, C, outcome='unknown'), HKILL, rd(0, A), HKILL, rd(0, B)), 'no open unknown explains')
        # Green: what the first read showed, or the durable value, may stay.
        self.assert_pass(self.dur(w(0, A), w(0, B, flush=False), HKILL, rd(0, A), HKILL, rd(0, A)), lost=1)
        self.assert_pass(self.dur(w(0, A), w(0, B, flush=False), HKILL, rd(0, B), HKILL, rd(0, B)), lost=0)
        self.assert_pass(self.dur(w(0, A), w(0, B, flush=False), HKILL, rd(0, B), HKILL, rd(0, A)), lost=0)

    def test_red_a_durable_value_read_after_a_kill_does_not_vanish_at_the_next(self):
        # An unknown unflushed U, then an acknowledged flush: durable is ZERO
        # or U. The read after the kill shows U, so U is the durable value;
        # ZERO at the next kill is a durable value vanishing.
        steps = [w(0, C, outcome='unknown', flush=False), fl(), HKILL, rd(0, C), HKILL]
        self.assert_fail(self.dur(*steps, rd(0, hc.ZERO)), 'ZERO although a durable write exists')
        self.assert_pass(self.dur(*steps, rd(0, C)), lost=0)
        # Unless a later write explains it: B written after the first read.
        self.assert_pass(self.dur(*steps[:4], w(0, B, flush=False), HKILL, rd(0, B)), lost=0)
        # A flushed A read after kill 1 does not vanish to ZERO at kill 2.
        self.assert_fail(self.dur(w(0, A), HKILL, rd(0, A), HKILL, rd(0, hc.ZERO)), 'ZERO although a durable write exists')

    def test_red_a_value_that_changes_between_two_reads_after_a_kill(self):
        # Both A and B are allowed after the kill; the first read fixes B, a
        # later read of A with no write in between is a change.
        steps = [w(0, A, outcome='unknown'), w(0, B), HKILL, rd(0, B)]
        self.assert_fail(self.dur(*steps, rd(0, A)), 'the value changed after the unknowns were settled')
        self.assert_fail(self.dur(w(0, A), w(0, B, flush=False), HKILL, rd(0, A), rd(0, B)), 'the value changed')
        # Stable reads pass; a write in between resets it.
        self.assert_pass(self.dur(*steps, rd(0, B), rd(0, B)), lost=0)
        self.assert_pass(self.dur(*steps, w(0, C), rd(0, C)), lost=0)

    def test_red_an_older_value_no_open_unknown_explains(self):
        # A acknowledged and flushed, then B: A is older than the last durable
        # value and no unknown write of A is open.
        self.assert_fail(self.dur(w(0, A), w(0, B), HKILL, rd(0, A)), 'an older value than the last durable one that no open unknown explains')
        # An unknown write of A AFTER a kill that settled it explains nothing
        # at a later kill either.
        self.assert_fail(self.dur(w(0, A, outcome='unknown'), HKILL, rd(0, hc.ZERO), w(0, B), HKILL, rd(0, A)), 'no open unknown explains')

    # Evidence.
    def test_a_kill_without_any_read_after_it_judges_nothing(self):
        v = self.dur(w(0, A), w(0, B, flush=False), rd(0, B), HKILL)
        self.assert_fail(v, 'judged nothing')
        self.assertIn('acknowledged, not flushed, not read after the kill: 1 block(s)', v['findings'])

    def test_without_a_kill_it_is_c1s_rule(self):
        self.assert_pass(self.dur(w(0, B, flush=False), rd(0, B)), lost=0)
        self.assert_fail(self.dur(w(0, B, flush=False), rd(0, hc.ZERO)), 'not one of')
        self.assert_fail(self.dur(w(0, B, flush=False)), 'written, never read afterwards')

    def test_a_flush_key_that_is_not_a_boolean_is_unknown(self):
        bad = ('write', 'v1', {'lba': 0, 'sha256': A, 'flush': 'false'}, 'ok', {})
        self.assertEqual(self.dur(bad, HKILL, rd(0, A))['verdict'], 'UNKNOWN')

    # The kill's scope: a holder kill reaches its volume, a master kill none.
    def test_red_a_master_kill_loses_no_unflushed_write(self):
        # The opposite of a holder kill: the data path is the holders', so
        # the old value of an unflushed write after a master kill is a FAIL.
        self.assert_fail(self.dur(w(0, A), w(0, B, flush=False), MKILL, rd(0, A)), 'not one of')
        self.assert_pass(self.dur(w(0, A), w(0, B, flush=False), MKILL, rd(0, B)), lost=0)
        # The same history with a holder kill of v1 passes, B counted lost.
        self.assert_pass(self.dur(w(0, A), w(0, B, flush=False), HKILL, rd(0, A)), lost=1)

    def test_red_a_master_kill_settles_no_unknown(self):
        # The unknown A stays open across a master kill: it may still land
        # after a later read of B, so reading A then is no value change.
        self.assert_pass(self.dur(w(0, B, flush=False), w(0, A, outcome='unknown'), w(0, B), MKILL, rd(0, B), rd(0, A)), lost=0)
        # A holder kill settles it: the same reads are a change.
        self.assert_fail(self.dur(w(0, B, flush=False), w(0, A, outcome='unknown'), w(0, B), HKILL, rd(0, B), rd(0, A)), 'the value changed')

    def test_red_a_holder_kill_of_v1_reaches_no_other_volume(self):
        # v2's unflushed B is not lost by a kill of v1's holder.
        self.assert_fail(self.dur(w(0, A, vol='v2'), w(0, B, flush=False, vol='v2'), w(0, C), HKILL, rd(0, C), rd(0, A, vol='v2')), 'read v2 lba 0')
        # v2's unknown A stays open: after B is read it may still land.
        v = self.dur(w(0, A, outcome='unknown', vol='v2'), w(0, B, vol='v2'), w(0, C), HKILL,
                     rd(0, B, vol='v2'), rd(0, A, vol='v2'), rd(0, C))
        self.assert_pass(v, lost=0)
        # The same on v1 itself is settled by the kill: a change.
        self.assert_fail(self.dur(w(0, A, outcome='unknown'), w(0, B), HKILL, rd(0, B), rd(0, A)), 'the value changed')

    def test_a_kill_naming_no_volume_reaches_every_volume_and_says_so(self):
        v = self.dur(w(0, A, vol='v2'), w(0, B, flush=False, vol='v2'), AKILL, rd(0, A, vol='v2'))
        self.assert_pass(v, lost=1)
        self.assertTrue(any('applied to every volume' in x for x in v['findings']), v)
        v = self.dur(w(0, A, vol='v2'), w(0, C, vol='v2'), AKILL, rd(0, A, vol='v2'))
        self.assert_fail(v, 'a kill naming no volume, applied to every volume')

    def test_only_master_kills_is_a_run_without_a_kill(self):
        v = self.dur(w(0, A), MKILL, rd(0, A))
        self.assert_fail(v, 'not exercised')

    # Review r1, change 2: a holder restarted with no recorded kill.
    def durh(self, *steps):
        """Like dur(); ('healthy', {volume: (pid, start)}) is a healthy
        observation carrying the holders' identities."""
        plain = [st for st in steps if st[0] != 'healthy']
        recs = synth(*plain)
        out, k = [], 0
        for st in steps:
            if st[0] == 'healthy':
                out.append({'seq': 0, 'kind': 'observe', 'client': 'runner', 'verb': 'healthy', 'volume': '', 'mono_ns': 0,
                            'result': {'healthy': True, 'down': [], 'not_serving': [],
                                       'holders': {v: {'pid': p, 'start': t} for v, (p, t) in st[1].items()}}})
            else:
                out += recs[k:k + 2]
                k += 2
        out.append(recs[-1])
        for i, r in enumerate(out, 1):
            r['mono_ns'] = i
        return hc.check(dump(renumber(out)))['DUR']

    def test_red_a_holder_restarted_with_no_recorded_kill_is_a_kill(self):
        h1, h2 = ('healthy', {'v1': (100, 's100')}), ('healthy', {'v1': (200, 's200')})
        # B unflushed; the holder's pid changes between two healthy records
        # with no kill: B may be lost (a PASS), and the finding says so.
        v = self.durh(w(0, A), h1, w(0, B, flush=False), h2, rd(0, A))
        self.assert_pass(v, lost=1)
        self.assertIn('holder of v1 restarted with no recorded kill (healthy seq 3 -> 6)', v['findings'])
        # Its settling applies too: A then B with no write between is a change.
        self.assert_fail(self.durh(w(0, A), h1, w(0, B, flush=False), h2, rd(0, A), rd(0, B)), 'restarted with no recorded kill')
        # The same start time with another pid, or the same pid with another
        # start time (a recycled pid), is a restart as well.
        for h in (('healthy', {'v1': (200, 's100')}), ('healthy', {'v1': (100, 's999')})):
            v = self.durh(w(0, A), h1, w(0, B, flush=False), h, rd(0, A))
            self.assertTrue(any('restarted with no recorded kill' in x for x in v['findings']), v)

    def test_red_the_same_holder_or_a_recorded_kill_is_no_silent_restart(self):
        h1 = ('healthy', {'v1': (100, 's100')})
        # Same holder: nothing restarted, B must read back.
        v = self.durh(w(0, A), h1, w(0, B, flush=False), h1, rd(0, A))
        self.assert_fail(v, 'not one of')
        self.assertFalse(any('restarted' in x for x in v['findings']), v)
        # A recorded kill and restart of v1 explain the new pid: no finding.
        restart = ('restart', '', {'process': 'blockvolume:0', 'volume': 'v1'}, 'ok', {})
        v = self.durh(w(0, A), h1, w(0, B, flush=False), HKILL, restart, ('healthy', {'v1': (200, 's200')}), rd(0, A))
        self.assert_pass(v, lost=1)
        self.assertFalse(any('restarted with no recorded kill' in x for x in v['findings']), v)
        # A kill of v2 does not explain a new holder of v1.
        k2 = ('kill', '', {'process': 'blockvolume:1', 'volume': 'v2', 'pid': 7, 'signal': 'KILL'}, 'ok', {})
        v = self.durh(w(0, A), h1, w(0, B, flush=False), k2, ('healthy', {'v1': (200, 's200')}), rd(0, A))
        self.assertTrue(any('holder of v1 restarted with no recorded kill' in x for x in v['findings']), v)

    # Review r1, change 3: the scope is stated.
    def test_the_scope_is_stated_on_pass(self):
        v = self.dur(w(0, A), w(0, B, flush=False), HKILL, rd(0, A))
        self.assert_pass(v, lost=1)
        self.assertIn("scope: same holder, same store, one replica (M2); replicated volumes are M3's and not judged here", v['findings'])
        self.assertIn('one replica', hc.dur.__doc__)

def m2(*steps):
    """A one-at-a-time history with healthy points: ('healthy', bool) is a
    healthy observation, ('obs', volume, result) a volume observation, any
    other step (verb, volume, args, outcome, result) an invoke and its
    complete. Closed by the end record."""
    recs = []
    for st in steps:
        n = len(recs)
        if st[0] == 'healthy':
            recs.append({'seq': n + 1, 'kind': 'observe', 'client': 'runner', 'verb': 'healthy', 'volume': '', 'mono_ns': n + 1,
                         'result': {'healthy': st[1], 'down': [] if st[1] else ['blockvolume:0'], 'not_serving': []}})
        elif st[0] == 'obs':
            recs.append({'seq': n + 1, 'kind': 'observe', 'client': 'c1', 'verb': 'observe', 'volume': st[1], 'mono_ns': n + 1, 'result': st[2]})
        elif st[0] == 'settled':   # ('settled', volume, state)
            recs.append({'seq': n + 1, 'kind': 'observe', 'client': 'runner', 'verb': 'settled', 'volume': st[1], 'mono_ns': n + 1,
                         'result': {'volume': st[1], 'state': st[2], 'evidence': 'status and listing'}})
        else:
            verb, vol, args, outcome, result = st
            recs.append({'seq': n + 1, 'kind': 'invoke', 'op': 'X.Y', 'verb': verb, 'volume': vol, 'args': args, 'mono_ns': n + 1})
            recs.append({'seq': n + 2, 'kind': 'complete', 'of': n + 1, 'op': 'X.Y', 'verb': verb, 'volume': vol, 'mono_ns': n + 2, 'outcome': outcome, 'result': result})
    recs.append({'seq': len(recs) + 1, 'kind': 'end', 'mono_ns': len(recs) + 1})
    return recs


def live(files=('HEAD', 'VOLUME'), holder='a-v1/r1', epoch=1, state=''):
    return {'head': {'gen': 3, 'epoch': epoch, 'holder': holder, 'state': state}, 'files': list(files)}


ROOT1, ROOT2 = 'root/' + '1' * 64, 'root/' + '2' * 64
KILL = ('kill', '', {'process': 'blockvolume:0', 'pid': 42, 'signal': 'KILL'}, 'ok', {})
RESTART = ('restart', '', {'process': 'blockvolume:0'}, 'ok', {})
MOVE = {'from': 'a-v1', 'to': 'b-v1', 'epoch': 1, 'request': 'v1-move-1'}


class C6(unittest.TestCase):
    def c6(self, *steps):
        return hc.check(dump(m2(*steps)))['C6']

    def assert_c6(self, want, *steps, contains=None):
        v = self.c6(*steps)
        self.assertEqual(v['verdict'], want, v)
        if contains:
            self.assertTrue(any(contains in x for x in v['findings']), v)
        return v

    # writes
    def test_an_unknown_write_read_back_either_way_after_a_healthy_point_passes(self):
        for got in (A, hc.ZERO):
            self.assert_c6('PASS', ('write', 'v1', {'lba': 0, 'sha256': A}, 'unknown', {}), KILL, RESTART, ('healthy', True),
                           ('read', 'v1', {'lba': 0}, 'ok', {'sha256': got}))

    def test_red_an_unknown_write_reading_a_third_value(self):
        self.assert_c6('FAIL', ('write', 'v1', {'lba': 0, 'sha256': S}, 'ok', {}), ('write', 'v1', {'lba': 0, 'sha256': A}, 'unknown', {}),
                       KILL, RESTART, ('healthy', True), ('read', 'v1', {'lba': 0}, 'ok', {'sha256': hc.ZERO}), contains='third state')

    def test_red_an_unknown_write_read_only_before_the_healthy_point(self):
        self.assert_c6('FAIL', ('write', 'v1', {'lba': 0, 'sha256': A}, 'unknown', {}), KILL,
                       ('read', 'v1', {'lba': 0}, 'ok', {'sha256': A}), RESTART, ('healthy', True), contains='not resolved')

    def test_red_an_unknown_write_overwritten_before_its_read(self):
        self.assert_c6('FAIL', ('write', 'v1', {'lba': 0, 'sha256': A}, 'unknown', {}), KILL, RESTART, ('healthy', True),
                       ('write', 'v1', {'lba': 0, 'sha256': T}, 'ok', {}), ('read', 'v1', {'lba': 0}, 'ok', {'sha256': T}),
                       contains='changed the block')

    def test_an_unknown_write_without_a_fault_is_resolved_at_the_end(self):
        self.assert_c6('PASS', ('write', 'v1', {'lba': 0, 'sha256': A}, 'unknown', {}), ('read', 'v1', {'lba': 0}, 'ok', {'sha256': A}))
        self.assert_c6('FAIL', ('write', 'v1', {'lba': 0, 'sha256': A}, 'unknown', {}), contains='no ok read')

    # snapshots
    def test_an_unknown_snapshot_placed_by_its_roots_passes(self):
        for after in ([ROOT1, ROOT2], [ROOT1]):
            self.assert_c6('PASS', ('obs', 'v1', live(['HEAD', 'VOLUME', ROOT1])), ('snapshot', 'v1', {'name': 's1'}, 'unknown', {}),
                           KILL, RESTART, ('healthy', True), ('obs', 'v1', live(['HEAD', 'VOLUME'] + after)))

    def test_red_an_unknown_snapshot_that_lost_a_root(self):
        self.assert_c6('FAIL', ('obs', 'v1', live(['HEAD', 'VOLUME', ROOT1])), ('snapshot', 'v1', {'name': 's1'}, 'unknown', {}),
                       KILL, RESTART, ('healthy', True), ('obs', 'v1', live(['HEAD', 'VOLUME', ROOT2])), contains='root objects lost')

    def test_red_an_unknown_snapshot_without_an_observation_before_it(self):
        self.assert_c6('FAIL', ('snapshot', 'v1', {'name': 's1'}, 'unknown', {}), KILL, RESTART, ('healthy', True),
                       ('obs', 'v1', live(['HEAD', 'VOLUME', ROOT1])), contains='no clean observation')

    def test_the_root_rule_is_named_when_it_judged(self):
        v = self.assert_c6('PASS', ('obs', 'v1', live(['HEAD', 'VOLUME', ROOT1])), ('snapshot', 'v1', {'name': 's1'}, 'unknown', {}),
                           KILL, RESTART, ('healthy', True), ('obs', 'v1', live(['HEAD', 'VOLUME', ROOT1, ROOT2])))
        self.assertTrue(any('placed took effect: by root objects' in x for x in v['findings']), v)

    def snap(self, before, after, roots_after):
        return self.c6(('obs', 'v1', dict(live(['HEAD', 'VOLUME', ROOT1]), snapshots=before)), ('snapshot', 'v1', {'name': 's1'}, 'unknown', {}),
                       KILL, RESTART, ('healthy', True), ('obs', 'v1', dict(live(['HEAD', 'VOLUME'] + roots_after), snapshots=after)))

    def test_an_unknown_snapshot_placed_by_its_name_passes(self):
        for after in (['s0', 's1'], ['s0']):
            v = self.snap(['s0'], after, [ROOT1, ROOT2])
            self.assertEqual(v['verdict'], 'PASS', v)
            self.assertTrue(any('by snapshot names' in x for x in v['findings']), v)

    def test_red_names_judge_over_roots(self):
        # A new root but the name absent: by names it did not take effect;
        # no new root but the name present: by names it took effect. The
        # notes prove the names decided, not the roots.
        v = self.snap([], [], [ROOT1, ROOT2])
        self.assertEqual(v['verdict'], 'PASS', v)
        self.assertTrue(any("placed did not: by snapshot names: 's1' absent" in x for x in v['findings']), v)
        v = self.snap([], ['s1'], [ROOT1])
        self.assertTrue(any("placed took effect: by snapshot names: 's1' present" in x for x in v['findings']), v)
        # a root lost, which the root rule fails, is not what names judge
        self.assertEqual(self.snap([], ['s1'], [ROOT2])['verdict'], 'PASS')

    def test_red_an_unknown_snapshot_that_lost_another_name(self):
        v = self.snap(['s0'], ['s1'], [ROOT1, ROOT2])
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('other snapshots lost: s0', v['findings'][0])

    def test_red_snapshot_names_in_one_observation_only_or_already_present(self):
        recs = m2(('obs', 'v1', live(['HEAD', 'VOLUME', ROOT1])), ('snapshot', 'v1', {'name': 's1'}, 'unknown', {}),
                  KILL, RESTART, ('healthy', True), ('obs', 'v1', dict(live(['HEAD', 'VOLUME', ROOT1, ROOT2]), snapshots=['s1'])))
        v = hc.check(dump(recs))['C6']
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('recorded by one observation only', v['findings'][0])
        v = self.snap(['s1'], ['s1'], [ROOT1, ROOT2])
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('already present before it', v['findings'][0])

    # deletes
    def test_an_unknown_delete_placed_by_status_and_listing_passes(self):
        # a delete is placed by its observation alone: no settled record
        for after in (live(['HEAD', 'VOLUME'], state='deleted'), live(['HEAD', 'VOLUME', ROOT1])):
            v = self.assert_c6('PASS', ('delete', 'v1', {'request': 'd1'}, 'unknown', {}), KILL, RESTART, ('healthy', True), ('obs', 'v1', after))
            self.assertIn('unknowns resolved: 1', v['findings'])

    def test_red_an_unknown_delete_stuck_half_way(self):
        for after, why in ((live(['HEAD', 'VOLUME'], state='deleting'), 'state deleting'),
                           (live(['HEAD', 'VOLUME', ROOT1], state='deleted'), 'deleted but'),
                           ({'show_error': {'exit': 1, 'code': 'volume_not_found'}}, 'absent instead')):
            self.assert_c6('FAIL', ('delete', 'v1', {'request': 'd1'}, 'unknown', {}), KILL, RESTART, ('healthy', True), ('obs', 'v1', after),
                           contains=why)

    def test_red_an_unknown_delete_never_observed(self):
        self.assert_c6('FAIL', ('delete', 'v1', {'request': 'd1'}, 'unknown', {}), ('obs', 'v1', live(state='deleted')), KILL, RESTART,
                       ('healthy', True), contains='no observation of v1 after healthy point')

    # moves
    def test_an_unknown_move_placed_by_holder_and_epoch_passes(self):
        for after in (live(holder='b-v1/r1', epoch=2), live(holder='a-v1/r1', epoch=1)):
            self.assert_c6('PASS', ('move', 'v1', MOVE, 'unknown', {}), KILL, RESTART, ('healthy', True), ('settled', 'v1', 'not_held'), ('obs', 'v1', after))

    def test_red_an_unknown_move_in_a_third_state(self):
        for after in (live(holder='b-v1/r1', epoch=1), live(holder='a-v1/r1', epoch=2)):
            self.assert_c6('FAIL', ('move', 'v1', MOVE, 'unknown', {}), KILL, RESTART, ('healthy', True), ('settled', 'v1', 'not_held'),
                           ('obs', 'v1', after), contains='third state')

    # settled (lifecycle unknowns)
    def timed_settle(self, op, kill_at, op_end_at, settle_at):
        """An unknown lifecycle op; times in seconds on mono_ns: a holder kill
        of v1 completing at kill_at (None: no kill), the op completing at
        op_end_at, a settled record of v1 at settle_at (None: none)."""
        steps = []
        if kill_at is not None:
            steps.append(('kill', '', {'process': 'blockvolume:0', 'volume': 'v1', 'pid': 42, 'signal': 'KILL'}, 'ok', {}))
        steps.append(op)
        if kill_at is not None:
            steps.append(RESTART)
        steps.append(('healthy', True))
        if settle_at is not None:
            steps.append(('settled', 'v1', 'not_held'))
        recs = m2(*steps)
        s, base = 10**9, 1000 * 10**9   # t = 0 is `base` on the clock
        times = {}
        if kill_at is not None:
            times[recs[1]['seq']] = base + kill_at * s
        op_c = next(r for r in recs if r['kind'] == 'complete' and r['verb'] == op[0])
        times[op_c['seq']] = base + op_end_at * s
        if settle_at is not None:
            times[next(r for r in recs if r.get('verb') == 'settled')['seq']] = base + settle_at * s
        clock = 0
        for r in recs:   # the named moments; every other record 1 ns after its predecessor
            clock = times.get(r['seq'], clock + 1)
            r['mono_ns'] = clock
        return hc.check(dump(recs))['C6']

    STOP = ('stop', 'v1', {}, 'unknown', {})

    def test_red_a_third_state_entry_fails(self):
        recs = m2(KILL, RESTART, ('healthy', True))
        recs[4]['result']['third_state'] = [{'volume': 'v2', 'why': 'holder alive but not serving'}]
        v = hc.check(dump(recs))['C6']
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('third state: v2 at healthy seq 5: holder alive but not serving', v['findings'][0])

    def test_red_an_unknown_stop_never_settled(self):
        v = self.timed_settle(self.STOP, None, 1, None)
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('unsettled: seq 1 stop v1: never settled within the deadline 95.000 s after its end = the end + 65 s + 30 s (no kill)', v['findings'][0])

    def test_red_an_unknown_stop_settled_after_the_deadline(self):
        # no kill: deadline = end + 65 + 30 = 96 s
        v = self.timed_settle(self.STOP, None, 1, 97)
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('settled only at seq 4, 96.000 s after its end, past the deadline 95.000 s', v['findings'][0])
        # with a kill at 0 and the op ending at 80: deadline max(65, 80) + 30 = 110 s
        v = self.timed_settle(self.STOP, 0, 80, 111)
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('past the deadline 30.000 s = max(kill seq 1 + 65 s, the end) + 30 s', v['findings'][0])

    def test_an_unknown_stop_settled_in_time_passes_and_is_counted(self):
        for kill_at, end, settle in ((None, 1, 95), (0, 80, 109), (0, 10, 95)):
            v = self.timed_settle(self.STOP, kill_at, end, settle)
            self.assertEqual(v['verdict'], 'PASS', (kill_at, end, settle, v))
            self.assertIn('unknowns resolved: 1', v['findings'])

    def test_a_settled_record_of_another_volume_resolves_nothing(self):
        recs = m2(self.STOP, ('healthy', True), ('settled', 'v2', 'not_held'))
        v = hc.check(dump(recs))['C6']
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('never settled', v['findings'][0])

    def test_red_an_unknown_move_and_a_later_move_before_the_observation(self):
        self.assert_c6('FAIL', ('move', 'v1', MOVE, 'unknown', {}), KILL, RESTART, ('healthy', True),
                       ('move', 'v1', dict(MOVE, epoch=2), 'ok', {}), ('obs', 'v1', live(holder='b-v1/r1', epoch=3)), contains='changed v1')

    # restores
    def test_an_unknown_restore_placed_by_the_target_passes(self):
        for after in (live(holder='a-v2/r1'), {'show_error': {'exit': 1, 'code': 'volume_not_found'}}):
            self.assert_c6('PASS', ('restore', 'v2', {'source': 'v1', 'snapshot': 's1'}, 'unknown', {}), KILL, RESTART, ('healthy', True),
                           ('obs', 'v2', after))

    def test_red_an_unknown_restore_leaving_volume_without_head(self):
        self.assert_c6('FAIL', ('restore', 'v2', {'source': 'v1', 'snapshot': 's1'}, 'unknown', {}), KILL, RESTART, ('healthy', True),
                       ('obs', 'v2', {'show_error': {'exit': 1, 'code': 'volume_not_found'}, 'files': ['VOLUME']}), contains='no HEAD but objects')

    # health
    def test_a_kill_restored_to_health_passes(self):
        self.assert_c6('PASS', KILL, ('healthy', False), RESTART, ('healthy', True))

    def test_unknowns_resolved_is_reported_in_its_exact_form(self):
        # zero unknowns with healthy points: PASS, n = 0 stated, not silent
        v = self.assert_c6('PASS', KILL, RESTART, ('healthy', True))
        self.assertIn('unknowns resolved: 0', v['findings'])
        # two placed unknowns (one took effect, one did not)
        v = self.assert_c6('PASS', ('write', 'v1', {'lba': 0, 'sha256': A}, 'unknown', {}), ('write', 'v1', {'lba': 1, 'sha256': T}, 'unknown', {}),
                           KILL, RESTART, ('healthy', True),
                           ('read', 'v1', {'lba': 0}, 'ok', {'sha256': A}), ('read', 'v1', {'lba': 1}, 'ok', {'sha256': hc.ZERO}))
        self.assertIn('unknowns resolved: 2', v['findings'])
        self.assertEqual([x for x in v['findings'] if x.startswith('unknowns resolved')], ['unknowns resolved: 2'])
        # a third state is not counted as resolved
        v = self.assert_c6('FAIL', ('write', 'v1', {'lba': 0, 'sha256': A}, 'unknown', {}), KILL, RESTART, ('healthy', True),
                           ('read', 'v1', {'lba': 0}, 'ok', {'sha256': JUNK}))
        self.assertIn('unknowns resolved: 0', v['findings'])

    def test_red_the_run_ends_unhealthy(self):
        self.assert_c6('FAIL', KILL, RESTART, ('healthy', True), KILL, ('healthy', False), contains='ended unhealthy')

    def test_red_a_kill_never_followed_by_a_healthy_point(self):
        self.assert_c6('FAIL', ('healthy', True), KILL, RESTART, contains='no healthy point after it')

    # recovery bound
    def healthy_after(self, secs, *steps):
        """The history of steps, its first healthy=true record moved to `secs`
        seconds after the last restart completion before it (later records with it)."""
        recs = m2(*steps)
        h = next(r for r in recs if r.get('verb') == 'healthy' and r['result']['healthy'])
        rc = [r for r in recs if r['kind'] == 'complete' and r['verb'] == 'restart' and r['seq'] < h['seq']][-1]
        delta = int(secs * 10**9) - (h['mono_ns'] - rc['mono_ns'])
        for r in recs:
            if r['seq'] >= h['seq']:
                r['mono_ns'] += delta
        return hc.check(dump(recs))['C6']

    def test_the_recovery_terms_are_the_pages_two_constants(self):
        self.assertEqual(hc.RECOVERY['regrant'], (65, 'docs/volume-lifecycle.md:352-356'))
        self.assertEqual(hc.RECOVERY['start_wait'], (30, 'docs/volume-lifecycle.md:368-370'))

    def timed(self, restart_at, healthy_at, *between):
        """Kill completed at t=0, restart completed at t=restart_at, the
        healthy=true point at t=healthy_at (seconds on mono_ns)."""
        recs = m2(KILL, RESTART, *between, ('healthy', True))
        s = 10**9
        kill_c = recs[1]
        restart_c = recs[3]
        h = [r for r in recs if r.get('verb') == 'healthy'][-1]
        base = 1000 * s
        for r in recs:   # every record keeps its order; the three moments are placed
            if r['seq'] <= kill_c['seq']:
                r['mono_ns'] = base - s + r['seq']
            elif r['seq'] <= restart_c['seq']:
                r['mono_ns'] = base + restart_at * s - (restart_c['seq'] - r['seq'])
            elif r['seq'] < h['seq']:
                r['mono_ns'] = base + restart_at * s + r['seq']
            else:
                r['mono_ns'] = base + healthy_at * s + (r['seq'] - h['seq'])
        kill_c['mono_ns'] = base
        return hc.check(dump(recs))['C6']

    def test_red_restart_at_10_healthy_at_96_is_past_the_deadline_95(self):
        v = self.timed(10, 96)
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('healthy at 96.000 s after the stop', v['findings'][0])
        self.assertIn('past the deadline 95.000 s = max(re-grant: stop + 65 s = 65.000 s, restart completion 10.000 s) + start wait 30 s', v['findings'][0])

    def test_restart_at_80_healthy_at_109_is_within_the_deadline_110(self):
        v = self.timed(80, 109, ('healthy', False))
        self.assertEqual(v['verdict'], 'PASS', v)

    def test_red_restart_at_80_healthy_at_111_is_past_the_deadline_110(self):
        v = self.timed(80, 111)
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('past the deadline 110.000 s = max(re-grant: stop + 65 s = 65.000 s, restart completion 80.000 s)', v['findings'][0])

    def test_a_restart_without_an_ok_kill_before_it_is_not_judged(self):
        # not judged, and nothing else judged: not exercised (never a PASS on 0)
        v = self.assert_c6('FAIL', ('healthy', True), RESTART, ('healthy', True), contains='C6 not exercised')
        self.assertTrue(any('the stop is not known' in x for x in v['findings']), v)
        self.assertEqual(v['judged'], 0)

    def test_red_a_holder_restart_never_healthy_again(self):
        # healthy=true only before the restart; the run then ends unhealthy too
        self.assert_c6('FAIL', KILL, ('healthy', True), KILL, RESTART, ('healthy', False), contains='never healthy afterwards')

    def test_a_master_restart_is_not_judged_for_recovery(self):
        mkill = ('kill', '', {'process': 'blockmaster', 'pid': 7, 'signal': 'KILL'}, 'ok', {})
        mrestart = ('restart', '', {'process': 'blockmaster'}, 'ok', {})
        # not judged; with nothing else judged the verdict is not exercised, not a failure of the rule
        v = self.healthy_after(300, mkill, mrestart, ('healthy', True))
        self.assertEqual((v['verdict'], v['judged']), ('FAIL', 0), v)
        self.assertIn('C6 not exercised', v['findings'][0])
        self.assertFalse(any('recovery bound' in x or 'past the deadline' in x for x in v['findings']), v)
        self.assertTrue(any('recovery not judged: restart seq 3 blockmaster' in x for x in v['findings']), v)
        # a holder restart a master fault overlaps is not judged either
        v = self.healthy_after(300, KILL, mkill, mrestart, RESTART, ('healthy', True))
        self.assertEqual((v['verdict'], v['judged']), ('FAIL', 0), v)
        self.assertIn('C6 not exercised', v['findings'][0])
        self.assertTrue(any('blockmaster kill seq 3 before its healthy point' in x for x in v['findings']), v)

    def test_a_healthy_record_with_an_operation_in_flight_is_no_point(self):
        recs = m2(('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}), ('healthy', True))
        inv, comp, h = recs[0], recs[1], recs[2]
        recs[:3] = [inv, h, comp]
        for i, r in enumerate(recs, 1):
            r['seq'] = r['mono_ns'] = i
        comp['of'] = inv['seq']
        self.assertEqual(hc.healthy_points(hc.read_history(dump(recs))), [])
        self.assertEqual(hc.healthy_points(hc.read_history(dump(m2(KILL, ('healthy', True))))), [3])

    # verdict shape
    def test_no_unknown_and_no_healthy_point_is_not_exercised(self):
        v = self.assert_c6('FAIL', ('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}), ('read', 'v1', {'lba': 0}, 'unknown', {}))
        self.assertIn('not exercised', v['findings'][0])

    def test_an_unknown_without_a_resolution_rule_is_unknown(self):
        self.assert_c6('UNKNOWN', ('create', 'v1', {}, 'unknown', {}), contains='no resolution rule')

    def test_no_stated_bound_is_listed_not_judged(self):
        v = self.assert_c6('PASS', ('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}), KILL, RESTART, ('healthy', True))
        self.assertIn('no stated bound: write (latency not judged)', v['findings'])
        self.assertEqual(hc.BOUNDS, {}, 'no operator page states an operation bound: nothing invented')

    def test_red_a_stated_bound_exceeded(self):
        recs = m2(('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}), ('healthy', True))
        recs[1]['mono_ns'], recs[2]['mono_ns'], recs[3]['mono_ns'] = 2 * 10**9, 2 * 10**9, 2 * 10**9
        with mock.patch.dict(hc.BOUNDS, {'write': (1, 'page.md:1')}):
            v = hc.check(dump(recs))['C6']
            self.assertEqual(v['verdict'], 'FAIL', v)
            self.assertIn('above the 1 s bound (page.md:1)', v['findings'][0])
            # under a fault the fault-free bound is not applied
            kv = hc.check(dump(m2(KILL, ('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}), RESTART, ('healthy', True))))['C6']
            self.assertEqual(kv['verdict'], 'PASS', kv)
            self.assertTrue(any('latency not judged' in x for x in kv['findings']), kv)


HOLDERS = {'v1': {'pid': 101, 'start': 's101'}, 'v2': {'pid': 102, 'start': 's102'}}
MRESTART = ('restart', '', {'process': 'blockmaster'}, 'ok', {})


def mo_history(window, secs=5, holders=HOLDERS, pre=()):
    """A short master outage: a healthy record naming the holders, an ok
    master kill, the window's steps, the master's ok restart `secs` seconds
    after the kill's completion, a healthy point. Window steps: an operation
    (verb, volume, args, outcome, result), or ('look', not_serving, holders)
    for a single-look healthy record."""
    recs = []

    def add(r):
        r['seq'] = len(recs) + 1
        recs.append(r)

    def op(verb, vol, args, outcome, result):
        add({'kind': 'invoke', 'op': 'X.Y', 'verb': verb, 'volume': vol, 'args': args})
        add({'kind': 'complete', 'of': recs[-1]['seq'], 'op': 'X.Y', 'verb': verb, 'volume': vol, 'outcome': outcome, 'result': result})

    def healthy(ok, ns, hs, look=None):
        res = {'healthy': ok, 'down': [] if ok else ['blockmaster'], 'not_serving': ns, 'holders': hs}
        if look:
            res['look'] = look
        add({'kind': 'observe', 'client': 'runner', 'verb': 'healthy', 'volume': '', 'result': res})

    s = 10**9
    healthy(True, [], holders)
    for st in pre:   # operations before the kill
        op(*st)
    op(*MKILL)
    kill_c = recs[-1]
    for st in window:
        if st[0] == 'look':
            healthy(False, st[1], st[2], look='single')
        else:
            op(*st)
    op(*MRESTART)
    restart_c = recs[-1]
    healthy(True, [], holders)
    add({'kind': 'end'})
    for r in recs:   # one clock: 1 ns per record, the restart's completion `secs` after the kill's
        r['mono_ns'] = r['seq'] if r['seq'] <= restart_c['seq'] - 1 else r['seq'] + secs * s
    restart_c['mono_ns'] = kill_c['mono_ns'] + secs * s
    return recs


class MO(unittest.TestCase):
    def mo(self, window, **kw):
        return hc.check(dump(mo_history(window, **kw)))['MO']

    def assert_mo(self, want, window, contains=None, **kw):
        v = self.mo(window, **kw)
        self.assertEqual(v['verdict'], want, v)
        if contains:
            self.assertTrue(any(contains in x for x in v['findings']), v)
        return v

    GOOD = [('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}), ('write', 'v2', {'lba': 0, 'sha256': A}, 'ok', {}),
            ('look', [], HOLDERS), ('look', [], HOLDERS)]

    def test_the_bound_is_the_pages_number_from_the_data(self):
        # The page line moves with the product pin; the data names it, and the
        # checker takes the number and the cite from there.
        data = json.loads((Path(__file__).resolve().parent.parent / 'packs' / 'v3block' / 'product' / 'checks.json').read_text(encoding='utf-8'))
        self.assertEqual(hc.MASTER_OUTAGE, (10, data['master_outage']['cite']))
        self.assertRegex(data['master_outage']['cite'], r'^docs/standalone-deployment\.md:\d+$')

    def test_a_short_outage_with_writes_ok_and_volumes_serving_passes(self):
        v = self.assert_mo('PASS', self.GOOD)
        self.assertEqual(v['findings'], ['short master outages judged: 1'])
        self.assert_mo('PASS', self.GOOD, secs=10)   # the page's number itself is short

    def test_red_a_refused_write_in_the_window(self):
        w = list(self.GOOD)
        w[1] = ('write', 'v2', {'lba': 0, 'sha256': A}, 'refused', {})
        self.assert_mo('FAIL', w, contains='write seq 6 v2 lba 0 answered refused inside the window')

    def test_red_an_unknown_write_in_the_window(self):
        w = list(self.GOOD)
        w[0] = ('write', 'v1', {'lba': 0, 'sha256': A}, 'unknown', {})
        self.assert_mo('FAIL', w, contains='answered unknown inside the window')

    def test_red_a_look_showing_a_volume_not_serving(self):
        w = list(self.GOOD)
        w[3] = ('look', ['v2'], HOLDERS)
        self.assert_mo('FAIL', w, contains='shows not serving: v2')

    def test_red_a_holder_pid_change(self):
        w = list(self.GOOD)
        w[3] = ('look', [], dict(HOLDERS, v1={'pid': 999, 'start': 's999'}))
        self.assert_mo('FAIL', w, contains="holder of v1 is (999, 's999'), was (101, 's101') before the kill")

    def test_red_a_12_s_outage_is_listed_not_judged(self):
        w = list(self.GOOD)
        w[1] = ('write', 'v2', {'lba': 0, 'sha256': A}, 'refused', {})   # would fail if judged
        v = self.assert_mo('UNKNOWN', w, secs=12, contains='12.000 s to the restart, above the 10 s short outage')
        self.assertIn('short master outages judged: 0', v['findings'])

    def test_red_a_window_with_no_write_is_unknown(self):
        v = self.assert_mo('UNKNOWN', [('look', [], HOLDERS)], contains='short master outage not exercised in window')
        self.assertIn('(0 write(s), 1 look(s))', v['findings'][0])

    def test_a_window_with_no_look_is_unknown(self):
        self.assert_mo('UNKNOWN', self.GOOD[:2], contains='(2 write(s), 0 look(s))')

    def test_each_look_is_evidence_and_a_waiting_record_is_no_look(self):
        # the bad look is the first of two: not folded into the good second one
        w = self.GOOD[:2] + [('look', ['v1'], HOLDERS), ('look', [], HOLDERS)]
        self.assert_mo('FAIL', w, contains='shows not serving: v1')
        # a healthy record without look "single" is not a look
        recs = mo_history(self.GOOD[:2] + [('look', [], HOLDERS)])
        for r in recs:
            r.get('result', {}).pop('look', None) if isinstance(r.get('result'), dict) else None
        self.assertIn('0 look(s)', hc.check(dump(recs))['MO']['findings'][0])

    def test_a_kill_never_restarted_is_not_judged(self):
        recs = synth(MKILL, ('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}))
        v = hc.check(dump(recs))['MO']
        self.assertEqual(v['verdict'], 'UNKNOWN', v)
        self.assertTrue(any('no ok restart of blockmaster after it' in x for x in v['findings']), v)

    def test_lifecycle_an_ok_move_is_not_held(self):
        self.assertEqual(hc.PRODUCT.LIFECYCLE, {'attach': 'held', 'stop': 'not_held', 'move': 'not_held', 'delete': 'not_held'})
        self.assertNotIn('delete', hc.PRODUCT.NEEDS_SETTLED)
        recs = hc.read_history(dump(mo_history(self.GOOD, pre=[('attach', 'v2', {}, 'ok', {}), ('move', 'v2', MOVE, 'ok', {})])))
        self.assertEqual(hc.PRODUCT.intended_states(recs, len(recs))['v2'], 'not_held')

    def test_red_an_ok_move_then_nothing_serving_the_volume_is_healthy(self):
        # v2 was moved away (a hand-off): nothing serves it until the next
        # attach, so a look showing it not serving is no failure.
        one = {'v1': HOLDERS['v1']}
        w = [('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {}), ('look', ['v2'], one)]
        v = self.assert_mo('PASS', w, pre=[('attach', 'v2', {}, 'ok', {}), ('move', 'v2', MOVE, 'ok', {})],
                           contains='not serving, not intended to be held: v2 (not_held)')
        self.assertIn('short master outages judged: 1', v['findings'])
        # the same look after an ok attach (held) is a failure
        self.assert_mo('FAIL', w, pre=[('move', 'v2', MOVE, 'ok', {}), ('attach', 'v2', {}, 'ok', {})], contains='shows not serving: v2')

    def test_no_master_kill_is_not_exercised(self):
        v = hc.check(dump(synth(KILL, RESTART)))['MO']
        self.assertEqual(v['verdict'], 'FAIL', v)
        self.assertIn('MO not exercised', v['findings'][0])



class Judged(unittest.TestCase):
    """Every checker reports its own count, "judged": the items it actually
    judged. A PASS with judged 0 is impossible: such a checker says "not
    exercised" itself. Checked on each checker's raw verdict (not through
    check(), whose own guard would hide a checker that forgot)."""

    def corpus(self):
        both = [('write', 'v1', {'lba': 0, 'sha256': A}, 'unknown', {}), KILL, RESTART, ('healthy', True),
                ('read', 'v1', {'lba': 0}, 'ok', {'sha256': A})]
        return {
            'm0 fixture': load(),
            'm0 with a fault': with_fault(load()),
            'only the end': synth(),
            'one write': synth(('write', 'v1', {'lba': 0, 'sha256': A}, 'ok', {})),
            'restored': synth(*RESTORED, ('write', 'v1', {'lba': 0, 'sha256': S}, 'ok', {}),
                              ('read', 'v2', {'lba': 0}, 'ok', {'sha256': A}),
                              ('write', 'v2', {'lba': 0, 'sha256': T}, 'ok', {}),
                              ('read', 'v1', {'lba': 0}, 'ok', {'sha256': S})),
            'C5 refusal': history(*(BASE + (('obs', 'v1', 4, FILES, WS), ('write', 'v1', NEG, 'refused'), ('obs', 'v1', 4, FILES, WS)))),
            'C6 unknown resolved': m2(*both),
            'C6 healthy only': m2(('healthy', True)),
            'C6 recovery timed': m2(KILL, RESTART, ('healthy', True)),
            'DUR kill': synth(w(0, A), w(0, B, flush=False), HKILL, rd(0, A)),
            'DUR no kill': synth(w(0, A, flush=False), rd(0, A)),
            'MO outage': mo_history(MO.GOOD),
        }

    def test_no_checker_passes_on_nothing(self):
        passed = {name: [] for name in hc.CHECKERS}
        for label, recs in self.corpus().items():
            rs = hc.read_history(dump(recs))
            for name, fn in hc.CHECKERS.items():
                try:
                    v = fn(rs)
                except (KeyError, TypeError):
                    continue
                if v['verdict'] == 'PASS':
                    self.assertIn('judged', v, (label, name, v))
                    self.assertGreater(v['judged'], 0, (label, name, v))
                    passed[name].append(label)
        # the test means something only if every checker PASSes somewhere here
        self.assertEqual([n for n, labels in passed.items() if not labels], [], passed)

    def test_every_verdict_carries_its_count(self):
        for label, recs in self.corpus().items():
            for name, v in hc.check(dump(recs)).items():
                self.assertIsInstance(v.get('judged'), int, (label, name, v))
        for lines in ([], dump(load())[:-1], ['not json']):
            for name, v in hc.check(lines).items():
                self.assertEqual(v['judged'], 0, (name, v))

    def test_the_counts_are_what_each_checker_judged(self):
        v = hc.check(dump(load()))
        rs = hc.read_history(dump(load()))
        ops_ = hc.operations(rs)
        self.assertEqual(v['C0']['judged'], sum(1 for _, c in ops_ if c is not None and c['outcome'] != 'unknown'))
        self.assertEqual(v['C1']['judged'], sum(1 for i, c in ops_ if i['verb'] == 'read' and c and c['outcome'] == 'ok'))
        self.assertEqual(v['C7']['judged'], 2, 'v1 and v2 deleted and observed')
        self.assertEqual(hc.check(dump(mo_history(MO.GOOD)))['MO']['judged'], 1)
        c6 = hc.check(dump(m2(('write', 'v1', {'lba': 0, 'sha256': A}, 'unknown', {}), KILL, RESTART, ('healthy', True),
                              ('read', 'v1', {'lba': 0}, 'ok', {'sha256': A}))))['C6']
        self.assertEqual(c6['judged'], 2, c6)   # one unknown resolved + the holder's recovery timed

    def test_red_a_c6_with_healthy_points_only_is_not_exercised(self):
        v = hc.check(dump(m2(('healthy', True))))['C6']
        self.assertEqual((v['verdict'], v['judged']), ('FAIL', 0), v)
        self.assertIn('C6 not exercised', v['findings'][0])

    def test_red_check_guards_a_checker_that_passes_on_nothing(self):
        with mock.patch.dict(hc.CHECKERS, {'ZZ': lambda recs: {'verdict': 'PASS', 'findings': []}}):
            v = hc.check(dump(load()))['ZZ']
        self.assertEqual((v['verdict'], v['judged']), ('FAIL', 0), v)
        self.assertIn('PASS with nothing judged: ZZ not exercised', v['findings'][0])

    def test_the_json_output_carries_judged(self):
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            hist, out = Path(d) / 'h.jsonl', Path(d) / 'v.json'
            hist.write_text('\n'.join(dump(load())) + '\n', encoding='utf-8')
            with mock.patch('sys.stdout'):
                hc.main([str(hist), '--json', str(out)])
            got = json.loads(out.read_text(encoding='utf-8'))
        self.assertEqual(set(got), set(hc.CHECKERS))
        for name, v in got.items():
            self.assertIsInstance(v['judged'], int, (name, v))
        self.assertGreater(got['C1']['judged'], 0)


HORIZON = 'not_admitted: layout: delete reservation: workspace: deletion refused before persistence: lease horizon'


class Attempts(unittest.TestCase):
    """A rerun is never folded: each attempt is its own invoke and complete,
    its args carrying the request and "attempt": n, and the request's
    outcome is its last attempt's."""

    def attempts(self, vol, outcomes, expect=False, effect=None):
        """Steps of one delete request answered `outcomes` in turn."""
        out = []
        for n, o in enumerate(outcomes, 1):
            args = {'request': 'delete-' + vol, 'layout_id': '0' * 32, 'attempt': n}
            if expect:
                args['expect'] = 'refused'
            out.append(('delete', vol, args, o))
        return out

    def finish(self, recs, details):
        """Set each delete completion's result: {seq of its invoke: result}."""
        for r in recs:
            if r['kind'] == 'complete' and r['verb'] == 'delete':
                r['result'] = details(r)
        return recs

    def case_ordinary(self):
        # v1: an ordinary delete refused twice by the lease horizon, then ok;
        # then its tombstone and a refused re-create. v2: a judged negative so
        # C5 has a limit to judge.
        tomb = dict(WS, files=['HEAD', 'VOLUME'])
        steps = (BASE + (('obs', 'v1', 4, FILES, WS),)
                 + tuple(self.attempts('v1', ['refused', 'refused', 'ok']))
                 + (('obs', 'v1', 5, ['HEAD', 'VOLUME'], tomb),
                    ('create', 'v1', NEG, 'refused'), ('obs', 'v1', 5, ['HEAD', 'VOLUME'], tomb),
                    ('obs', 'v2', 4, FILES, WS), ('write', 'v2', NEG, 'refused'), ('obs', 'v2', 4, FILES, WS)))
        recs = history(*steps)
        for r in recs:
            if r['kind'] == 'observe' and r.get('volume') == 'v1' and r['result'].get('files') == ['HEAD', 'VOLUME']:
                r['result']['head']['state'] = 'deleted'
            if r['kind'] == 'complete' and r['verb'] == 'create':
                r['result'] = {'detail': 'rpc error: code = FailedPrecondition desc = volume v1 %s' % hc.PRODUCT.RESERVED_NAME}
        return self.finish(recs, lambda r: {'detail': HORIZON} if r['outcome'] == 'refused' else {})

    def test_superseded_groups_attempts_by_request(self):
        recs = hc.read_history(dump(self.case_ordinary()))
        inv = [r for r in recs if r['kind'] == 'invoke' and r['verb'] == 'delete']
        self.assertEqual(hc.superseded(recs), {inv[0]['seq'], inv[1]['seq']})
        # attempt 1 again starts a new request; an invoke without attempt is a request of one
        again = history(*(self.attempts('v1', ['refused', 'ok']) + self.attempts('v1', ['refused', 'ok'])))
        rs = hc.read_history(dump(again))
        firsts = [r['seq'] for r in rs if r['kind'] == 'invoke' and r['args']['attempt'] == 1]
        self.assertEqual(hc.superseded(rs), set(firsts))

    def test_an_ordinary_delete_refused_twice_by_the_horizon_then_ok_counts_no_refusal(self):
        v = hc.check(dump(self.case_ordinary()))
        self.assertEqual(v['C5']['verdict'], 'PASS', v['C5'])
        self.assertIn('limit ordinary write: 1 of 1 refusal(s) judged', v['C5']['findings'])
        self.assertIn('limit ordinary create: 1 of 1 refusal(s) judged', v['C5']['findings'])
        self.assertFalse(any(x.startswith('limit ordinary delete') for x in v['C5']['findings']), v['C5'])
        self.assertTrue(any('refused attempts a later attempt of the same request replaced: 2' in x for x in v['C5']['findings']), v['C5'])
        self.assertEqual(v['C5']['judged'], 2)
        # C0: the request answered ok; its refused attempts are no refusal not expected
        self.assertEqual(v['C0']['verdict'], 'PASS', v['C0'])
        # C7 sees its ok, the last attempt
        self.assertEqual((v['C7']['verdict'], v['C7']['judged']), ('PASS', 1), v['C7'])

    def test_red_a_mutant_counting_every_attempt_makes_it_unknown(self):
        with mock.patch.object(hc, 'superseded', lambda recs: set()):
            v = hc.check(dump(self.case_ordinary()))
        self.assertEqual(v['C5']['verdict'], 'UNKNOWN', v['C5'])
        self.assertEqual(v['C5']['findings'][0], 'limit(s) with no judged refusal: ordinary delete', v['C5'])
        self.assertEqual(v['C0']['verdict'], 'FAIL', v['C0'])   # and C0 takes the horizon for a refusal not expected

    def test_the_delete_source_negative_with_three_horizon_attempts_then_refused_effect_counts_1_of_1(self):
        lim = 'delete a source whose snapshot has a restore target'
        steps = BASE + (('obs', 'v1', 4, FILES, WS),) + tuple(self.attempts('v1', ['refused'] * 3 + ['refused'], expect=True)) \
            + (('obs', 'v1', 4, FILES, C5.REFUSED_WS),)
        recs = history(*steps)
        last = [r for r in recs if r['kind'] == 'complete' and r['verb'] == 'delete'][-1]
        for r in recs:
            if r['kind'] == 'complete' and r['verb'] == 'delete':
                r['result'] = {'detail': HORIZON}
        last['outcome'] = 'refused_effect'
        last['result'] = {'detail': 'snapshot_has_dependents: layout: a snapshot has dependents', 'effect': C5.EFFECT}
        at = next(i for i, r in enumerate(recs) if r['kind'] == 'invoke' and r['verb'] == 'delete')
        recs.insert(at, {'seq': 0, 'kind': 'observe', 'client': 'gen', 'verb': 'gen_step', 'volume': 'v1', 'mono_ns': recs[at]['mono_ns'],
                         'result': {'action': 'v3_op_delete', 'negative': lim, 'params': {'expect': 'refused', 'volume': 'v1'}}})
        v = hc.check(dump(renumber(recs)))
        self.assertEqual(v['C5']['verdict'], 'PASS', v['C5'])
        self.assertIn('limit %s: 1 of 1 refusal(s) judged' % lim, v['C5']['findings'])
        self.assertTrue(any('replaced: 3' in x for x in v['C5']['findings']), v['C5'])
        self.assertEqual(v['C0']['verdict'], 'PASS', v['C0'])

    def test_an_unknown_resolves_per_request_by_its_last_attempt(self):
        # An unknown first attempt rerun to a refused second: C6 has no
        # unknown to place (the request was answered), C0 no unknown.
        recs = history(*(self.attempts('v1', ['unknown', 'refused'])))
        v = hc.check(dump(recs))
        self.assertFalse(any('outcome unknown' in x for x in v['C0']['findings']), v['C0'])
        self.assertIn('C6 not exercised', v['C6']['findings'][0])

if __name__ == '__main__':
    unittest.main()
