import unittest
from pathlib import Path
from completion import rep_complete


class Completion(unittest.TestCase):
    def decide(self, phases=None, checks=None, required_checks=(), failed=(), ready='1', state='run', hosts=None):
        return rep_complete(
            {'work': 'rc=0'} if phases is None else phases, ['work'],
            {} if checks is None else checks, required_checks, failed,
            ready, state, {'host': 'CLEAN'} if hosts is None else hosts)

    def test_missing_required_phase_policy_cannot_produce_success(self):
        with self.assertRaisesRegex(ValueError, 'requires phase obligations'):
            rep_complete({}, [], {}, [], [], '1', 'run', {'host': 'CLEAN'})

    def test_missing_phase_is_never_success(self):
        result = self.decide(phases={})
        self.assertEqual(result['verdict'], 'INCONCLUSIVE')
        self.assertEqual(result['missing'], ['phase:work'])
        self.assertEqual(result['deciding_receipts'][0]['name'], 'work')


    def test_total_order_for_every_pair_of_receipt_classes(self):
        pairs = [
            ('NOT_RUN', 'FAIL', 'NOT_RUN'),
            ('NOT_RUN', 'INCONCLUSIVE', 'NOT_RUN'),
            ('NOT_RUN', 'PASS_NOT_CLEAN', 'NOT_RUN'),
            ('NOT_RUN', 'PASS', 'NOT_RUN'),
            ('FAIL', 'INCONCLUSIVE', 'FAIL'),
            ('FAIL', 'PASS_NOT_CLEAN', 'FAIL'),
            ('FAIL', 'PASS', 'FAIL'),
            ('INCONCLUSIVE', 'PASS_NOT_CLEAN', 'INCONCLUSIVE'),
            ('INCONCLUSIVE', 'PASS', 'INCONCLUSIVE'),
            ('PASS_NOT_CLEAN', 'PASS', 'PASS_NOT_CLEAN'),
        ]
        kinds = ['NOT_RUN', 'FAIL', 'INCONCLUSIVE', 'PASS_NOT_CLEAN', 'PASS']
        pairs += [(kind, kind, kind) for kind in kinds]
        for first, second, expected in pairs + [(b, a, winner) for a, b, winner in pairs if a != b]:
            with self.subTest(first=first, second=second):
                classes = {first, second}
                result = self.decide(
                    phases={} if 'INCONCLUSIVE' in classes else {'work': 'rc=0'},
                    checks={'explicit-failure': ('FAIL', 'observed')} if 'FAIL' in classes else {},
                    failed=['explicit-failure'] if 'FAIL' in classes else [],
                    ready='0' if 'NOT_RUN' in classes else '1', state='blocked',
                    hosts={'host': 'UNKNOWN' if 'PASS_NOT_CLEAN' in classes else 'CLEAN'})
                self.assertEqual(result['verdict'], 'NOT_RUN(blocked)' if expected == 'NOT_RUN' else expected)
                self.assertTrue(result['deciding_receipts'])
                self.assertEqual({receipt['kind'] for receipt in result['deciding_receipts']}, {expected})

    def test_unparsable_phase_receipt_is_inconclusive(self):
        for value in ['notstarted', 'noresult', 'garbage', '']:
            with self.subTest(value=value):
                self.assertEqual(self.decide(phases={'work': value})['verdict'], 'INCONCLUSIVE')

    def test_nonzero_phase_is_failure_even_when_another_phase_is_missing(self):
        result = self.decide(phases={'extra': 'rc=7'})
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['failed'], ['phase:extra'])
        self.assertEqual(result['deciding_receipts'], [
            dict(kind='FAIL', source='phase', name='extra', value='rc=7')])

    def test_required_check_absence_is_not_success(self):
        self.assertEqual(self.decide(required_checks=['required'])['verdict'], 'INCONCLUSIVE')

    def test_phase_receipts_last_write_wins_despite_failure_prose(self):
        parsed = dict([('work', 'rc=7'), ('work', 'rc=0')])
        self.assertEqual(self.decide(phases=parsed)['verdict'], 'PASS')
        self.assertEqual(self.decide(phases={'work': 'rc=7'})['verdict'], 'FAIL')

    def test_missing_phase_mutant_would_return_false_pass(self):
        source = Path(__file__).with_name('completion.py').read_text()
        condition = "kind = 'PASS' if value is not None and value.startswith('rc=') else 'INCONCLUSIVE'"
        self.assertEqual(source.count(condition), 1)
        namespace = {}
        exec(compile(source.replace(condition, "kind = 'PASS'"), '<missing-phase-mutant>', 'exec'), namespace)
        result = namespace['rep_complete']({}, ['work'], {}, [], [], '1', 'run', {'host': 'CLEAN'})
        self.assertEqual(result['verdict'], 'PASS')
        self.assertEqual(self.decide(phases={})['verdict'], 'INCONCLUSIVE')


if __name__ == '__main__':
    unittest.main()
