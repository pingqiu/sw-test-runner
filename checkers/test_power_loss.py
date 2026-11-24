"""Literal images at the public power-loss checker boundary."""
import copy
import hashlib
from pathlib import Path
import types
import unittest

from power_loss import power_loss_image_exact


def digest(data):
    return hashlib.sha256(data).hexdigest()


def fixture(local=None, published=None, fixed=True):
    case = {
        'run_id': 'run', 'image_id': 'image', 'volume_id': 'volume',
        'revision': 'revision', 'local_view_id': 'recovered',
        'published_view_id': 'restored', 'block_bytes': 4,
        'chunk_bytes': 8, 'volume_bytes': 32, 'write_ahead_fixed': fixed,
        'snapshot': {'attempt': 'attempt', 'head_gen': 3, 'cut_lsn': 2},
        'window': {'recovery_complete': 0, 'local_complete': 1,
                   'snapshot_complete': 2, 'guest_writes': 0},
        'proof': {'revision': 'revision', 'clock_id': 'host-boot',
                  'binding': {'run_id': 'run', 'image_id': 'image',
                              'volume_id': 'volume', 'revision': 'revision'},
                  'write_ack_ns': 1, 'cut_mark_ns': 300000001,
                  'status_poll_ns': 200000001, 'status_exit': 0,
                  'extent_writes': 0 if fixed else 1,
                  'extent_write_ns': None if fixed else 100000001},
    }
    workload = {'baseline': {0: b'AAAA', 2: b'BBBB', 4: b'CCCC'},
                'writes': [{'block': 0, 'new': b'ZZZZ', 'flushed': False},
                           {'block': 2, 'new': b'DDDD', 'flushed': True},
                           {'block': 4, 'new': b'EEEE', 'flushed': False}],
                'operations': ['write']}
    data = b'AAAA' + bytes(4) + b'DDDD' + bytes(4) + b'EEEE' + bytes(12)
    local = data if local is None else local
    published = local if published is None else published

    def observation(source, data):
        binding = {k: case[k] for k in ('run_id', 'image_id', 'volume_id', 'revision')}
        binding['view_id'] = case[source + '_view_id']
        return {
            'binding': binding, 'source': source, 'producer_exit': 0,
            'snapshot': dict(case['snapshot']) if source == 'published' else None,
            'coverage': {'chunks': 4, 'count': 4, 'bytes': 32, 'first': 0, 'last': 31},
            'chunks': [{'chunk_index': c, 'offset': c * 8, 'length': 8,
                        'sha256': digest(data[c * 8:c * 8 + 8]),
                        'source': source, 'binding': dict(binding),
                        'data': data[c * 8:c * 8 + 8]} for c in range(4)],
            'blocks': [{'block': w['block'],
                        'sha256': digest(data[w['block'] * 4:w['block'] * 4 + 4]),
                        'data': data[w['block'] * 4:w['block'] * 4 + 4]}
                       for w in workload['writes']],
        }
    return case, workload, observation('local', local), observation('published', published)


class PowerLoss(unittest.TestCase):
    def test_legal_unflushed_values_still_fail_when_published_differs(self):
        case, workload, local, published = fixture()
        old = b'AAAA' + bytes(4) + b'DDDD' + bytes(4) + b'CCCC' + bytes(12)
        published = fixture(published=old)[3]
        result = power_loss_image_exact(case, workload, local, published)
        self.assertEqual(result['image_verdict'], 'FAIL')
        self.assertEqual(result['rows']['LOCAL_HISTORY']['verdict'], 'PASS')
        self.assertEqual(result['rows']['PUBLISHED_EQUALS_LOCAL']['labels'], ['DIVERGED'])
        self.assertEqual(result['counts']['chunks_compared'], 4)

    def test_complete_nonprefix_survival_is_legal(self):
        result = power_loss_image_exact(*fixture())
        self.assertEqual(result['verdict'], 'PASS')
        for value in (b'AAAA', b'ZZZZ'):
            for later in (b'CCCC', b'EEEE'):
                data = value + bytes(4) + b'DDDD' + bytes(4) + later + bytes(12)
                with self.subTest(first=value, later=later):
                    self.assertEqual(power_loss_image_exact(*fixture(local=data))['image_verdict'], 'PASS')

    def test_flushed_loss_is_distinct_from_divergence(self):
        data = b'AAAA' + bytes(4) + b'BBBB' + bytes(4) + b'EEEE' + bytes(12)
        result = power_loss_image_exact(*fixture(local=data))
        self.assertEqual(result['image_verdict'], 'FAIL')
        self.assertEqual(result['rows']['LOCAL_HISTORY']['labels'], ['ACKED-LOST'])
        self.assertEqual(result['rows']['PUBLISHED_EQUALS_LOCAL']['verdict'], 'PASS')

    def test_torn_and_untouched_corruption_fail(self):
        for offset in (0, 7, 31):
            data = bytearray(b'AAAA' + bytes(4) + b'DDDD' + bytes(4) + b'EEEE' + bytes(12))
            data[offset] = 1
            with self.subTest(offset=offset):
                result = power_loss_image_exact(*fixture(local=bytes(data)))
                self.assertEqual(result['rows']['LOCAL_HISTORY']['labels'], ['UNEXPECTED'])

    def test_incomplete_unbound_or_malformed_observations_are_unknown(self):
        changes = [
            lambda c, w, l, p: l['chunks'].pop(),
            lambda c, w, l, p: l['chunks'].append(copy.deepcopy(l['chunks'][0])),
            lambda c, w, l, p: l['chunks'][0].update(offset=1),
            lambda c, w, l, p: l['chunks'][0].update(length=1),
            lambda c, w, l, p: l['chunks'][0].update(sha256='a' * 16),
            lambda c, w, l, p: l['coverage'].update(bytes=31),
            lambda c, w, l, p: l['coverage'].update(last=30),
            lambda c, w, l, p: l['blocks'].pop(),
            lambda c, w, l, p: l['blocks'].append(copy.deepcopy(l['blocks'][0])),
            lambda c, w, l, p: l['blocks'][0].update(sha256='0' * 64),
            lambda c, w, l, p: l['blocks'][0].update(data=b'x'),
            lambda c, w, l, p: l.update(producer_exit=1),
            lambda c, w, l, p: l['binding'].update(image_id='other'),
            lambda c, w, l, p: l['chunks'][0]['binding'].update(volume_id='other'),
            lambda c, w, l, p: p['snapshot'].update(head_gen=8),
            lambda c, w, l, p: p['blocks'][0].update(data=b'ZZZZ', sha256=digest(b'ZZZZ')),
            lambda c, w, l, p: l['chunks'][0].update(data=b'bad'),
            lambda c, w, l, p: w['writes'].append(copy.deepcopy(w['writes'][0])),
            lambda c, w, l, p: w['writes'][0].update(flushed='yes'),
            lambda c, w, l, p: w.update(operations=['discard']),
            lambda c, w, l, p: w.update(operations=['write-zeroes']),
            lambda c, w, l, p: c.update(block_bytes=0),
            lambda c, w, l, p: c.update(volume_bytes=33),
        ]
        for change in changes:
            inputs = copy.deepcopy(fixture())
            change(*inputs)
            with self.subTest(change=changes.index(change)):
                self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'UNKNOWN')

    def test_campaign_failures_do_not_relabel_the_image(self):
        inputs = fixture()
        inputs[0]['proof'].update(extent_writes=1, extent_write_ns=100000001)
        result = power_loss_image_exact(*inputs)
        self.assertEqual(result['image_verdict'], 'PASS')
        self.assertEqual(result['campaign_verdict'], 'FAIL')
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['rows']['CAMPAIGN_TICK']['labels'], ['WRITE-AHEAD-VIOLATION'])

    def test_revision_specific_tick_proof(self):
        self.assertEqual(power_loss_image_exact(*fixture(fixed=False))['verdict'], 'PASS')
        changes = [
            lambda c: c.pop('proof'),
            lambda c: c['proof'].update(revision='other'),
            lambda c: c['proof'].update(clock_id=''),
            lambda c: c['proof'].update(cut_mark_ns=250000000),
            lambda c: c['proof'].update(status_poll_ns=1),
            lambda c: c['proof'].update(status_poll_ns=300000001),
            lambda c: c['proof'].update(status_exit=1),
            lambda c: c['proof'].update(extent_writes=-1),
            lambda c: c.update(write_ahead_fixed='yes'),
        ]
        for change in changes:
            inputs = fixture()
            change(inputs[0])
            with self.subTest(change=changes.index(change)):
                result = power_loss_image_exact(*inputs)
                self.assertEqual(result['image_verdict'], 'PASS')
                self.assertEqual(result['campaign_verdict'], 'UNKNOWN')
                self.assertEqual(result['verdict'], 'UNKNOWN')
        for count, instant in ((0, None), (1, 0), (1, 300000002)):
            inputs = fixture(fixed=False)
            inputs[0]['proof'].update(extent_writes=count, extent_write_ns=instant)
            with self.subTest(count=count, instant=instant):
                self.assertEqual(power_loss_image_exact(*inputs)['campaign_verdict'], 'UNKNOWN')

    def test_two_period_boundary_is_inclusive(self):
        inputs = fixture()
        inputs[0]['proof']['cut_mark_ns'] = 250000001
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'PASS')

    def test_other_image_tick_receipt_cannot_qualify(self):
        inputs = fixture()
        inputs[0]['proof']['binding']['image_id'] = 'other'
        self.assertEqual(power_loss_image_exact(*inputs)['campaign_verdict'], 'UNKNOWN')

    def test_observed_fixed_revision_extent_write_fails_even_without_tick_wait(self):
        inputs = fixture()
        inputs[0]['proof'].update(extent_writes=1, cut_mark_ns=100000001)
        inputs[0]['proof'].pop('status_poll_ns')
        self.assertEqual(power_loss_image_exact(*inputs)['campaign_verdict'], 'FAIL')

    def test_one_wal_record_does_not_require_group_atomicity(self):
        inputs = fixture()
        for write in inputs[1]['writes']:
            write['write_id'] = 'one-multiblock-record'
        self.assertEqual(power_loss_image_exact(*inputs)['image_verdict'], 'PASS')

    def test_full_hash_divergence_needs_no_raw_chunk_diagnostics(self):
        published = b'AAAA' + bytes(4) + b'DDDD' + bytes(4) + b'CCCC' + bytes(12)
        inputs = fixture(published=published)
        for view in inputs[2:]:
            for chunk in view['chunks']:
                chunk.pop('data')
        inputs[0].pop('proof')
        result = power_loss_image_exact(*inputs)
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['rows']['LOCAL_HISTORY']['verdict'], 'PASS')
        self.assertEqual(result['rows']['PUBLISHED_EQUALS_LOCAL']['labels'], ['DIVERGED', 'RAW-MISSING'])
        self.assertEqual(result['rows']['PUBLISHED_EQUALS_LOCAL']['raw_missing_chunks'], [2])
        self.assertEqual(result['campaign_verdict'], 'UNKNOWN')

    def test_raw_diagnostic_gate_mutant(self):
        before = "    for block in written:\n"
        restored = ("    for index, chunk in local_chunks.items():\n"
                    "        if chunk['sha256'] != published_chunks[index]['sha256']:\n"
                    "            _require('data' in chunk and 'data' in published_chunks[index], 'raw required')\n"
                    + before)
        source = (Path(__file__).parent / 'power_loss.py').read_text()
        self.assertEqual(source.count(before), 1)
        module = types.ModuleType('raw_gate_mutant')
        exec(compile(source.replace(before, restored), '<raw-gate-mutant>', 'exec'), module.__dict__)
        published = b'AAAA' + bytes(4) + b'DDDD' + bytes(4) + b'CCCC' + bytes(12)
        inputs = fixture(published=published)
        for view in inputs[2:]:
            for chunk in view['chunks']:
                chunk.pop('data')
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'FAIL')
        self.assertEqual(module.power_loss_image_exact(*inputs)['verdict'], 'UNKNOWN')

    def test_equal_chunk_evidence_needs_no_whole_volume_raw_stream(self):
        inputs = fixture()
        for observation in inputs[2:]:
            for chunk in observation['chunks']:
                chunk.pop('data')
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'PASS')

    def test_flushed_and_quiet_divergence_remain_distinguishable(self):
        data = b'AAAA' + bytes(4) + b'BBBB' + bytes(4) + b'EEEE' + bytes(12)
        published = b'AAAA' + bytes(4) + b'DDDD' + bytes(4) + b'CCCC' + bytes(12)
        result = power_loss_image_exact(*fixture(local=data, published=published))
        self.assertEqual(result['rows']['LOCAL_HISTORY']['labels'], ['ACKED-LOST'])
        self.assertEqual(result['rows']['PUBLISHED_EQUALS_LOCAL']['labels'], ['DIVERGED'])

    def test_publication_must_follow_recovered_local_reads_without_guest_writes(self):
        changes = [{'snapshot_complete': 1}, {'local_complete': 0}, {'guest_writes': 1}]
        for change in changes:
            inputs = fixture()
            inputs[0]['window'].update(change)
            with self.subTest(change=change):
                self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'UNKNOWN')

    def test_last_partial_chunk_uses_its_actual_range(self):
        inputs = fixture()
        inputs[0]['volume_bytes'] = 28
        for observation in inputs[2:]:
            observation['coverage'].update(bytes=28, last=27)
            observation['chunks'][3].update(length=4, data=bytes(4), sha256=digest(bytes(4)))
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'PASS')

    def test_all_flushed_and_all_unchanged_controls(self):
        data = b'ZZZZ' + bytes(4) + b'DDDD' + bytes(4) + b'EEEE' + bytes(12)
        inputs = fixture(local=data)
        inputs[1]['operations'] = ['write', 'flush', 'fua']
        for write in inputs[1]['writes']:
            write['flushed'] = True
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'PASS')
        inputs[1]['baseline'] = {0: b'ZZZZ', 2: b'DDDD', 4: b'EEEE'}
        for write in inputs[1]['writes']:
            write['flushed'] = False
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'PASS')

    def test_previous_flushed_neighbor_outside_this_window_is_protected(self):
        inputs = fixture()
        inputs[1]['baseline'][1] = b'NEIG'
        for observation in inputs[2:]:
            observation['blocks'].append({'block': 1, 'data': bytes(4), 'sha256': digest(bytes(4))})
        result = power_loss_image_exact(*inputs)
        self.assertEqual(result['rows']['LOCAL_HISTORY']['labels'], ['ACKED-LOST'])
        self.assertEqual(result['rows']['PUBLISHED_EQUALS_LOCAL']['verdict'], 'PASS')

    def test_missing_baseline_written_block_is_not_an_inferred_zero(self):
        inputs = fixture()
        inputs[1]['baseline'][1] = b'NEIG'
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'UNKNOWN')

    def test_quiet_published_chunk_divergence_is_not_skipped(self):
        published = b'AAAA' + bytes(4) + b'DDDD' + bytes(4) + b'EEEE' + bytes(11) + b'x'
        result = power_loss_image_exact(*fixture(published=published))
        self.assertEqual(result['rows']['LOCAL_HISTORY']['verdict'], 'PASS')
        self.assertEqual(result['rows']['PUBLISHED_EQUALS_LOCAL']['chunks'], [3])

    def test_missing_tick_proof_does_not_erase_a_bound_image_failure(self):
        published = b'AAAA' + bytes(4) + b'DDDD' + bytes(4) + b'CCCC' + bytes(12)
        inputs = fixture(published=published)
        inputs[0].pop('proof')
        result = power_loss_image_exact(*inputs)
        self.assertEqual(result['image_verdict'], 'FAIL')
        self.assertEqual(result['campaign_verdict'], 'UNKNOWN')
        self.assertEqual(result['verdict'], 'FAIL')


class Mutants(unittest.TestCase):
    def mutant(self, before, after):
        source = (Path(__file__).parent / 'power_loss.py').read_text()
        self.assertEqual(source.count(before), 1)
        module = types.ModuleType('power_loss_mutant')
        exec(compile(source.replace(before, after), '<controlled-mutant>', 'exec'), module.__dict__)
        return module.power_loss_image_exact

    def test_local_published_comparison_mutant(self):
        compare = self.mutant(
            "if local_chunks[index]['sha256'] != published_chunks[index]['sha256']:", 'if False:')
        data = b'AAAA' + bytes(4) + b'DDDD' + bytes(4) + b'CCCC' + bytes(12)
        inputs = fixture(published=data)
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'FAIL')
        self.assertEqual(compare(*inputs)['verdict'], 'PASS')

    def test_quiet_chunk_mutant(self):
        compare = self.mutant("if local_chunks[index]['sha256'] != expected_sha:",
                              "if index in by_chunk and local_chunks[index]['sha256'] != expected_sha:")
        data = b'AAAA' + bytes(4) + b'DDDD' + bytes(4) + b'EEEE' + bytes(11) + b'x'
        inputs = fixture(local=data)
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'FAIL')
        self.assertEqual(compare(*inputs)['verdict'], 'PASS')

    def test_arbitrary_unflushed_bytes_mutant(self):
        compare = self.mutant("elif not write['flushed'] and actual not in (old, write['new']):", 'elif False:')
        data = b'torn' + bytes(4) + b'DDDD' + bytes(4) + b'EEEE' + bytes(12)
        inputs = fixture(local=data)
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'FAIL')
        self.assertEqual(compare(*inputs)['verdict'], 'PASS')

    def test_flushed_obligation_mutant(self):
        compare = self.mutant("if write['flushed'] and actual != write['new']:", 'if False:')
        data = b'AAAA' + bytes(4) + b'BBBB' + bytes(4) + b'EEEE' + bytes(12)
        inputs = fixture(local=data)
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'FAIL')
        self.assertEqual(compare(*inputs)['verdict'], 'PASS')

    def test_prefix_only_survival_mutant(self):
        before = "image_verdict = 'FAIL' if history_labels or divergence else 'PASS'"
        compare = self.mutant(before, before + "\n    if local_blocks[0] == baseline[0] and local_blocks[4] != baseline[4]: image_verdict = 'FAIL'")
        self.assertEqual(power_loss_image_exact(*fixture())['verdict'], 'PASS')
        self.assertEqual(compare(*fixture())['verdict'], 'FAIL')

    def test_coverage_status_and_binding_mutants(self):
        for anchor, change in [
            ("_require(observation['coverage'] == expected and", lambda x: x['coverage'].update(bytes=31)),
            ("_require(type(observation['producer_exit']) is int", lambda x: x.update(producer_exit=1)),
            ("_require(observation['binding'] == binding", lambda x: x['binding'].update(image_id='other')),
        ]:
            with self.subTest(anchor=anchor):
                compare = self.mutant(anchor, anchor.replace('_require(', '_require(True or ', 1))
                inputs = fixture()
                change(inputs[2])
                self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'UNKNOWN')
                self.assertEqual(compare(*inputs)['verdict'], 'PASS')

    def test_fixed_revision_extent_write_mutant(self):
        compare = self.mutant('if count > 0:', 'if False:')
        inputs = fixture()
        inputs[0]['proof'].update(extent_writes=1, extent_write_ns=100000001)
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'FAIL')
        self.assertEqual(compare(*inputs)['verdict'], 'PASS')

    def test_last_chunk_comparison_mutant(self):
        compare = self.mutant('for index in range(chunks):', 'for index in range(chunks - 1):')
        data = b'AAAA' + bytes(4) + b'DDDD' + bytes(4) + b'EEEE' + bytes(11) + b'x'
        inputs = fixture(published=data)
        self.assertEqual(power_loss_image_exact(*inputs)['verdict'], 'FAIL')
        self.assertEqual(compare(*inputs)['verdict'], 'PASS')


if __name__ == '__main__':
    unittest.main()
