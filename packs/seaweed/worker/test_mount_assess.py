"""Stage 1 mount soak oracle: plan r4's listed positives and causal negatives, on synthetic journals."""
import hashlib, unittest

import mount_assess as A


def sha(seed, n):
    return hashlib.sha256(A.payload(seed, n)).hexdigest()


class J:
    """A minimal complete, valid run; tests add or remove one cause."""

    def __init__(self):
        self.op, self.ev = 0, []
        self.cohort, self.faults = {}, {}
        self.t = 100.0
        for ep in ('E60', 'E180'):
            self.episode(ep)
        for i in range(1000):  # coverage: chmods overlapping an in-flight append on one file
            self.add(t='attr', file='w0', mode=0o600 + i % 8, xattr='v%d' % i, rc=0, start=10.0 + i, end=10.5 + i, mount='m1')
        for i in range(1000):
            self.add(t='app', file='w0', seq=i, full=True, fsync=0, close=0, start=10.0 + i, end=10.9 + i, mount='m0')
        self.barrier('w0', 1000)
        self.mut('create', 'a/keep', 'g1', 'seed1', 4096, mount='m2')
        self.final('a/keep', 'seed1', 4096)

    def add(self, **r):
        self.op += 1
        r.setdefault('op', str(self.op))
        self.ev.append(r)
        return r

    def episode(self, ep):
        self.faults[ep] = dict(rule_confirmed=True, counter_rose=True, filer_ok=True, rule_gone=True, argv_direct=True,
                               apply=5000.0, heal=5060.0)
        files = []
        for i in range(21):
            path, seed = '%s/f%d' % (ep, i), '%s-%d' % (ep, i)
            files.append(dict(path=path, sha=sha(seed, 1 << 20), size=1 << 20))
            self.add(t='w3final', ep=ep, path=path, chunks=True, sha=files[-1]['sha'], size=1 << 20, after_heal_s=30.0, start=5100.0, end=5100.1)
        self.cohort[ep] = dict(files=files, frozen_before_heal=True)

    def mut(self, kind, path, gen, seed, size, full=True, fsync=0, close=0, start=None, mount='m2', **kw):
        start = self.next() if start is None else start
        return self.add(t='mut', kind=kind, path=path, gen=gen, seed=seed, size=size, sha=sha(seed, size), full=full,
                        fsync=fsync, close=close, rc=kw.pop('rc', 0), start=start, end=start + 0.1, mount=mount, **kw)

    def next(self):
        self.t += 1
        return self.t

    def final(self, path, seed, size, rc=0):
        return self.add(t='final', path=path, rc=rc, sha=sha(seed, size), size=size, start=9000.0, end=9000.1)

    def barrier(self, file, n, seen=None, seqs=None):
        return self.add(t='barrier', file=file, seqs=list(range(n)) if seqs is None else seqs, valid=True, sentinel_acked=True,
                        sentinel_mode=0o640, sentinel_xattr='s', seen_mode=0o640 if seen is None else seen, seen_xattr='s',
                        start=9000.0, end=9000.1)

    def drop(self, pred):
        self.ev = [e for e in self.ev if not pred(e)]

    def verdict(self, **over):
        self.ev.append(dict(t='end', dispatched=sum(1 for e in self.ev if 'op' in e)))
        d = dict(events=self.ev, journal_complete=True, env=dict(fuse_ok=True, mounts_started=True, mounts=['m0', 'm1', 'm2']),
                 faults=self.faults, cohorts=self.cohort, cleanup='PROVEN')
        d['env']['mounts'] = ['m0', 'm2']
        d.update(over)
        return A.assess(d)


class Oracle(unittest.TestCase):
    def status(self, j, **over):
        return j.verdict(**over)

    def failing(self, v):
        return [k for k, r in v['rows'].items() if r['outcome'] == 'FAIL']

    def passes(self, j):
        v = j.verdict()
        self.assertEqual((v['status'], self.failing(v)), ('PASS', []), v)

    def fails(self, j, row):
        v = j.verdict()
        self.assertEqual((v['status'], self.failing(v)), ('FAIL', [row]), v)

    def test_baseline_passes(self):
        self.passes(J())

    # --- Positives
    def test_overwrite_delete_rename_survivors(self):
        j = J()
        j.mut('create', 'b/x', 'g2', 's2', 100)
        j.mut('replace', 'b/x', 'g3', 's3', 200)
        j.mut('create', 'b/gone', 'g4', 's4', 100)
        j.mut('delete', 'b/gone', 'g4', 's4', 0)
        j.mut('create', 'b/old', 'g5', 's5', 100)
        j.mut('rename', 'b/old', 'g5', 's5', 100, to='b/new')
        j.final('b/x', 's3', 200)
        j.final('b/new', 's5', 100)
        self.passes(j)

    def test_stale_read_of_an_earlier_generation_passes(self):
        j = J()
        j.mut('create', 'b/x', 'g2', 's2', 100, start=200.0)
        j.mut('replace', 'b/x', 'g3', 's3', 200, start=210.0)
        j.add(t='read', path='b/x', rc=0, sha=sha('s2', 100), size=100, start=220.0, end=220.1)
        j.final('b/x', 's3', 200)
        self.passes(j)

    def test_enoent_on_the_old_name_after_a_rename_passes(self):  # found in rehearsal: the old name is really gone
        j = J()
        j.mut('create', 'b/old', 'g2', 's2', 100, start=200.0)
        j.mut('replace', 'b/old', 'g3', 's3', 100, start=205.0)
        j.mut('rename', 'b/old', 'g3', 's3', 100, to='b/new', start=210.0)
        j.add(t='read', path='b/old', rc=-1, errno='ENOENT', sha='', size=0, start=220.0, end=220.1)
        j.final('b/new', 's3', 100)
        self.passes(j)

    def test_overlapping_chmod_lost_to_last_writer_wins_passes(self):
        j = J()
        j.add(t='wread', file='w0', valid=True, seqs=[0, 1], mode=0o601, initial_mode=0o600, start=20.0, end=20.1)
        self.passes(j)

    def test_partial_write_leaving_a_prefix_passes(self):
        j = J()
        j.mut('create', 'b/p', 'g2', 's2', 4096, full=False, fsync=None)
        j.add(t='final', path='b/p', rc=0, sha=sha('s2', 1000), size=1000, start=9000.0, end=9000.1)
        self.passes(j)

    def test_no_prior_fsync_buffered_write_then_close_error_absent_passes(self):
        j = J()
        j.mut('create', 'b/p', 'g2', 's2', 4096, fsync=None, close=5)
        self.passes(j)

    def test_buffered_write_then_fsync_error_absent_passes(self):
        j = J()
        j.mut('create', 'b/p', 'g2', 's2', 4096, fsync=5)
        self.passes(j)

    def test_fsync_ack_with_close_eio_exact_data_passes_and_records_the_error(self):
        j = J()
        r = j.mut('create', 'b/p', 'g2', 's2', 4096, fsync=0, close=5)
        j.final('b/p', 's2', 4096)
        self.passes(j)
        self.assertEqual(r['close'], 5)

    def test_sync_control_buffered_then_fsync_error_absent_passes(self):
        j = J()
        j.add(t='w3', mount='sync', path='s/p', full=True, fsync=5, close=0, sha=sha('x', 10), size=10, start=5010.0, end=5020.0)
        self.passes(j)

    # --- Causal negatives
    def test_dropped_acked_record_fails(self):
        j = J()
        j.drop(lambda e: e['t'] == 'barrier')
        j.barrier('w0', 1000, seqs=list(range(999)))
        self.fails(j, 'no_lost_update')

    def test_gap_in_final_records_fails(self):
        j = J()
        j.drop(lambda e: e['t'] == 'barrier')
        j.barrier('w0', 1000, seqs=[s for s in range(1000) if s != 500])
        self.fails(j, 'no_lost_update')

    def test_gap_after_all_acked_records_fails(self):
        j = J()
        j.drop(lambda e: e['t'] == 'barrier')
        j.barrier('w0', 1000, seqs=list(range(1000)) + [1001])
        self.fails(j, 'no_lost_update')

    def test_sentinel_not_shown_fails(self):
        j = J()
        j.drop(lambda e: e['t'] == 'barrier')
        j.barrier('w0', 1000, seen=0o600)
        self.fails(j, 'no_lost_update')

    def test_never_set_mode_fails(self):
        j = J()
        j.add(t='wread', file='w0', valid=True, seqs=[0], mode=0o777, initial_mode=0o600, start=20.0, end=20.1)
        self.fails(j, 'no_silent_corruption')

    def test_torn_middle_record_fails(self):
        j = J()
        j.add(t='wread', file='w0', valid=False, seqs=[0, 2], start=20.0, end=20.1)
        self.fails(j, 'no_silent_corruption')

    def test_missing_survivor_fails(self):
        j = J()
        j.drop(lambda e: e['t'] == 'final')
        self.fails(j, 'no_lost_ack')

    def test_read_matching_no_generation_fails(self):
        j = J()
        j.mut('create', 'b/x', 'g2', 's2', 100, start=200.0)
        j.add(t='read', path='b/x', rc=0, sha='0' * 64, size=100, start=220.0, end=220.1)
        j.final('b/x', 's2', 100)
        self.fails(j, 'no_silent_corruption')

    def test_fsync_ack_close_eio_missing_survivor_fails(self):
        j = J()
        j.mut('create', 'b/p', 'g2', 's2', 4096, fsync=0, close=5)  # the ack stands; a later close error does not erase it
        self.fails(j, 'no_lost_ack')

    def test_sync_control_persisted_ack_missing_fails(self):
        j = J()
        j.add(t='w3', mount='sync', path='s/p', full=True, fsync=0, close=0, sha=sha('x', 10), size=10, start=5010.0, end=5020.0)
        self.fails(j, 'sync_control_honest')

    def test_21_file_cohort_one_lost_fails(self):
        j = J()
        j.drop(lambda e: e['t'] == 'w3final' and e['path'] == 'E60/f7')
        self.fails(j, 'writeback_outage')

    def test_cohort_file_not_exact_fails(self):
        j = J()
        next(e for e in j.ev if e['t'] == 'w3final' and e['path'] == 'E60/f7')['sha'] = '0' * 64
        self.fails(j, 'writeback_outage')

    def test_cohort_file_recovered_too_late_fails(self):
        j = J()
        next(e for e in j.ev if e['t'] == 'w3final' and e['path'] == 'E60/f7')['after_heal_s'] = 121.0
        self.fails(j, 'writeback_outage')

    def test_mount_exit_fails(self):
        j = J()
        j.add(t='mount_exit', mount='m0', after_start=True, start=300.0, end=300.0)
        self.fails(j, 'mounts_alive')

    def test_eio_outside_a_window_fails_inside_passes(self):
        j = J()
        j.add(t='read', path='z', rc=-1, errno='EIO', sha='', size=0, start=5040.0, end=5041.0)
        self.passes(j)
        j = J()
        j.add(t='read', path='z', rc=-1, errno='EIO', sha='', size=0, start=300.0, end=301.0)
        self.fails(j, 'mounts_alive')

    def test_long_op_outside_a_window_fails(self):
        j = J()
        j.add(t='read', path='z', rc=0, sha='', size=0, start=300.0, end=361.0)
        self.fails(j, 'bounded_ops')

    def test_op_outlasting_heal_grace_fails(self):
        j = J()
        j.add(t='read', path='z', rc=0, sha='', size=0, start=5050.0, end=5190.0)
        self.fails(j, 'bounded_ops')

    def test_an_op_of_a_later_episode_is_not_charged_to_an_earlier_heal(self):  # found in rehearsal
        j = J()
        j.faults['E60'].update(apply=5000.0, heal=5060.0)
        j.faults['E180'].update(apply=5400.0, heal=5580.0)
        j.add(t='w3', mount='sync', path='s/q', full=False, fsync=5, close=5, sha='', size=0, errno='EIO', start=5100.0, end=5579.0)
        self.passes(j)

    # --- Gates: never a product verdict
    def invalid(self, j, **over):
        v = j.verdict(**over)
        self.assertEqual(v['status'], 'INVALID', v)
        return v

    def test_truncated_journal_is_invalid(self):
        self.invalid(J(), journal_complete=False)

    def test_fuse_missing_is_environment(self):
        v = J().verdict(env=dict(fuse_ok=False, mounts_started=False))
        self.assertEqual(v['status'], 'ENVIRONMENT')

    def test_unwitnessed_fault_is_invalid(self):
        j = J()
        j.faults['E60']['counter_rose'] = False
        self.invalid(j)

    def test_proxy_path_is_invalid(self):
        j = J()
        j.faults['E180']['argv_direct'] = False
        self.invalid(j)

    def test_cohort_below_floor_is_invalid(self):
        j = J()
        j.cohort['E60']['files'] = j.cohort['E60']['files'][:19]
        self.invalid(j)

    def test_cohort_frozen_after_heal_is_invalid(self):
        j = J()
        j.cohort['E60']['frozen_before_heal'] = False
        self.invalid(j)

    def test_chmod_coverage_short_is_invalid(self):
        j = J()
        j.drop(lambda e: e['t'] == 'attr' and int(e['op']) % 2 == 0)
        self.invalid(j)

    def test_w2_file_without_barrier_is_invalid(self):
        j = J()
        j.drop(lambda e: e['t'] == 'barrier')
        self.invalid(j)

    def test_a_full_test_cluster_is_invalid_not_a_product_verdict(self):  # found in rehearsal
        j = J()
        j.add(t='mut', kind='create', path='b/x', gen='g9', seed='s9', size=10, sha='', full=False, fsync=None, close=None, rc=0,
              errno='ENOSPC', start=300.0, end=300.1, mount='m2')
        v = self.invalid(j)
        self.assertIn('ENOSPC', ' '.join(v['gate_reasons']))

    def test_invalid_with_unknown_cleanup_shows_both(self):
        v = self.invalid(J(), journal_complete=False, cleanup='UNKNOWN')
        self.assertEqual(v['cleanup'], 'UNKNOWN')

    def test_clean_run_unknown_cleanup_is_not_pass(self):
        v = J().verdict(cleanup='UNKNOWN')
        self.assertEqual((v['status'], v['cleanup']), ('CLEANUP_UNKNOWN', 'UNKNOWN'))

    def test_fail_with_unknown_cleanup_stays_fail_and_shows_it(self):
        j = J()
        j.drop(lambda e: e['t'] == 'final')
        v = j.verdict(cleanup='UNKNOWN')
        self.assertEqual((v['status'], v['cleanup']), ('FAIL', 'UNKNOWN'))

    def test_proven_violation_is_reported_when_coverage_is_short(self):
        j = J()
        j.drop(lambda e: e['t'] == 'final')
        j.cohort['E60']['files'] = j.cohort['E60']['files'][:19]
        v = j.verdict()
        self.assertEqual(v['status'], 'INVALID')
        self.assertIn('no_lost_ack', v['violations_proven'])


if __name__ == '__main__':
    unittest.main()
