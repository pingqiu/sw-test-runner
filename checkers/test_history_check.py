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
        self.assertEqual(v, {'C0': 'FAIL', 'C1': 'FAIL', 'C2': 'FAIL', 'C3': 'FAIL', 'C7': 'FAIL'})
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


if __name__ == '__main__':
    unittest.main()
