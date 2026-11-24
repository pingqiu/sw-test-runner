"""Whole-pattern observations are reconstructed, never compared as tag strings."""
import copy
import hashlib
import unittest
import types
from pathlib import Path
from test_power_loss import fixture
from power_loss import power_loss_image_exact


def pattern(tag):
    line = ("WALFS<" + tag + ">").encode().ljust(63, b".") + b"\n"
    return line * 64


def tag_fixture():
    case, _, local, published = fixture()
    case.update(block_bytes=4096, chunk_bytes=8192, volume_bytes=16384,
                observation_purpose='legacy_parity')
    workload = {'baseline': {}, 'writes': [
        {'block': 0, 'new': pattern('new'), 'flushed': False}], 'operations': ['write']}
    for view in (local, published):
        view['encoding'] = 'pattern-tags/v1'
        view['coverage'] = {'chunks': 2, 'count': 2, 'bytes': 16384, 'first': 0, 'last': 16383}
        view['tag_blocks'] = [{'block': 0, 'tag': 'new'}]
        view.pop('blocks')
        view['chunks'] = [dict(chunk_index=i, offset=i*8192, length=8192,
                              source=view['source'], binding=dict(view['binding']),
                              nonzero_blocks=1 if i == 0 else 0) for i in range(2)]
    return case, workload, local, published


class TagScan(unittest.TestCase):
    def test_tags_require_explicit_legacy_parity_purpose(self):
        for purpose in (None, 'qualification', 'legacy_parity_typo'):
            args = tag_fixture()
            if purpose is None:
                args[0].pop('observation_purpose')
            else:
                args[0]['observation_purpose'] = purpose
            with self.subTest(purpose=purpose):
                self.assertEqual(power_loss_image_exact(*args)['verdict'], 'UNKNOWN')

    def test_patterns_rebuild_bytes_and_equal_views_pass(self):
        result = power_loss_image_exact(*tag_fixture())
        self.assertEqual(result['verdict'], 'PASS')
        self.assertEqual(result['rows']['TAG_SCAN']['verdict'], 'PASS')

    def test_same_tag_cannot_override_expected_bytes(self):
        args = tag_fixture()
        args[1]['writes'][0]['new'] = pattern('different')
        result = power_loss_image_exact(*args)
        self.assertEqual(result['rows']['LOCAL_HISTORY']['labels'], ['UNEXPECTED'])

    def test_other_without_raw_is_unknown(self):
        args = tag_fixture()
        args[2]['tag_blocks'][0]['tag'] = 'other:' + '78'*16
        self.assertEqual(power_loss_image_exact(*args)['verdict'], 'UNKNOWN')

    def test_other_with_raw_third_value_is_fail(self):
        args = tag_fixture()
        args[2]['tag_blocks'][0].update(tag='other:'+'78'*16, data=b'x'*4096)
        result = power_loss_image_exact(*args)
        self.assertEqual(result['rows']['LOCAL_HISTORY']['verdict'], 'FAIL')
        self.assertEqual(result['rows']['PUBLISHED_EQUALS_LOCAL']['labels'], ['DIVERGED'])

    def test_counts_coverage_duplicate_and_bad_raw_are_unknown(self):
        changes = [lambda v: v['chunks'][0].update(nonzero_blocks=2),
                   lambda v: v['chunks'].pop(),
                   lambda v: v['coverage'].update(bytes=8192),
                   lambda v: v['tag_blocks'].append(dict(v['tag_blocks'][0])),
                   lambda v: v['tag_blocks'][0].update(data=b'x'*4096),
                   lambda v: v['tag_blocks'][0].update(block=4)]
        for change in changes:
            args = tag_fixture()
            change(args[2])
            with self.subTest(change=change):
                self.assertEqual(power_loss_image_exact(*args)['verdict'], 'UNKNOWN')

    def test_full_hash_crosscheck_mismatch_is_harness_fail(self):
        args = tag_fixture()
        args[2]['chunks'][0]['scan_sha256'] = '0'*64
        result = power_loss_image_exact(*args)
        self.assertEqual(result['image_verdict'], 'PASS')
        self.assertEqual(result['rows']['TAG_SCAN']['labels'], ['HARNESS-HASH-MISMATCH'])
        self.assertEqual(result['verdict'], 'FAIL')

    def test_legacy_prefix_and_full_hash_crosschecks(self):
        for width in (16, 64):
            args = tag_fixture()
            args[2]['chunks'][0]['scan_sha256'] = hashlib.sha256(pattern('new')+bytes(4096)).hexdigest()[:width]
            self.assertEqual(power_loss_image_exact(*args)['verdict'], 'PASS')

    def test_quiet_chunk_other_is_not_silently_zero(self):
        args = tag_fixture()
        args[2]['tag_blocks'].append({'block': 3, 'tag': 'other:'+'78'*16, 'data': b'x'*4096})
        args[2]['chunks'][1]['nonzero_blocks'] = 1
        self.assertEqual(power_loss_image_exact(*args)['image_verdict'], 'FAIL')

    def test_input_records_are_not_mutated(self):
        args = tag_fixture()
        before = copy.deepcopy(args)
        power_loss_image_exact(*args)
        self.assertEqual(args, before)


class TagMutants(unittest.TestCase):
    def mutant(self, before, after):
        source = (Path(__file__).parent / 'power_loss.py').read_text()
        self.assertEqual(source.count(before), 1)
        module = types.ModuleType('tag_mutant')
        exec(compile(source.replace(before, after), '<tag-mutant>', 'exec'), module.__dict__)
        return module.power_loss_image_exact

    def test_nonzero_count_mutant(self):
        judge = self.mutant("record['nonzero_blocks'] == nonzero", 'True')
        args = tag_fixture()
        args[2]['chunks'][0]['nonzero_blocks'] = 2
        self.assertEqual(power_loss_image_exact(*args)['verdict'], 'UNKNOWN')
        self.assertEqual(judge(*args)['verdict'], 'PASS')

    def test_crosscheck_mutant(self):
        judge = self.mutant('if digest[:len(observed)] != observed:', 'if False:')
        args = tag_fixture()
        args[2]['chunks'][0]['scan_sha256'] = '0'*64
        self.assertEqual(power_loss_image_exact(*args)['verdict'], 'FAIL')
        self.assertEqual(judge(*args)['verdict'], 'PASS')

    def test_pattern_reconstruction_mutant(self):
        judge = self.mutant('data = line * 64', 'data = bytes(4096)')
        args = tag_fixture()
        self.assertEqual(power_loss_image_exact(*args)['verdict'], 'PASS')
        self.assertNotEqual(judge(*args)['verdict'], 'PASS')

    def test_qualification_tag_admission_mutant(self):
        judge = self.mutant("case.get('observation_purpose') == 'legacy_parity'", 'True')
        args = tag_fixture()
        args[0]['observation_purpose'] = 'qualification'
        self.assertEqual(power_loss_image_exact(*args)['verdict'], 'UNKNOWN')
        self.assertEqual(judge(*args)['verdict'], 'PASS')
