import unittest
from pathlib import Path
import types

from oss_kernel import entries_data_acknowledged


def entry_trace():
    binding = {'run': 'synthetic', 'volume': 'v'}
    scope = {'binding': binding, 'entries': ['v/root/h']}
    fields = {'name': 'v/root/h', 'data_id': 'needle-1', 'length': 8, 'sha256': 'a' * 64}
    observation = {
        'binding': dict(binding), 'producer_exit': 0,
        'coverage': {'first': 0, 'last': 1, 'count': 2},
        'events': [dict(fields, ordinal=0, kind='data_ack'),
                   dict(fields, ordinal=1, kind='entry_visible')],
    }
    return scope, observation


class EntryOrder(unittest.TestCase):
    def test_data_ack_precedes_visible_entry(self):
        scope, observation = entry_trace()
        result = entries_data_acknowledged(scope, observation)
        self.assertEqual(result['verdict'], 'PASS')
        self.assertEqual(result['checked'], ['v/root/h'])

    def test_incomplete_or_unbound_trace_is_unknown(self):
        for field, value in [('producer_exit', 1), ('producer_exit', False),
                             ('binding', {'run': 'other', 'volume': 'v'}),
                             ('coverage', {'first': 0, 'last': 2, 'count': 3})]:
            with self.subTest(field=field, value=value):
                scope, observation = entry_trace()
                observation[field] = value
                self.assertEqual(entries_data_acknowledged(scope, observation)['verdict'], 'UNKNOWN')

    def test_entry_before_acknowledgement_fails(self):
        scope, observation = entry_trace()
        observation['events'][0]['kind'] = 'entry_visible'
        observation['events'][1]['kind'] = 'data_ack'
        result = entries_data_acknowledged(scope, observation)
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['violations'][0]['ordinal'], 0)

    def test_different_data_is_not_the_entry_acknowledgement(self):
        for field, value in [('data_id', 'needle-2'), ('sha256', 'b' * 64), ('length', 9)]:
            with self.subTest(field=field):
                scope, observation = entry_trace()
                observation['events'][0][field] = value
                self.assertEqual(entries_data_acknowledged(scope, observation)['verdict'], 'FAIL')

    def test_absent_ack_in_complete_trace_is_a_violation(self):
        scope, observation = entry_trace()
        observation['events'] = [dict(observation['events'][1], ordinal=0)]
        observation['coverage'] = {'first': 0, 'last': 0, 'count': 1}
        self.assertEqual(entries_data_acknowledged(scope, observation)['verdict'], 'FAIL')

    def test_missing_expected_entry_does_not_pass_vacuously(self):
        scope, observation = entry_trace()
        scope['entries'].append('v/root/other')
        result = entries_data_acknowledged(scope, observation)
        self.assertEqual(result['verdict'], 'UNKNOWN')
        self.assertEqual(result['unknown'], ['v/root/other'])
        observation['events'][0]['sha256'] = 'b' * 64
        self.assertEqual(entries_data_acknowledged(scope, observation)['verdict'], 'FAIL')

    def test_incoherent_event_order_or_identity_is_unknown(self):
        for field, value in [('ordinal', 1), ('ordinal', False), ('kind', 'request_sent'),
                             ('sha256', 'a' * 16), ('length', True), ('name', 'other/root/h')]:
            with self.subTest(field=field, value=value):
                scope, observation = entry_trace()
                observation['events'][0][field] = value
                self.assertEqual(entries_data_acknowledged(scope, observation)['verdict'], 'UNKNOWN')

    def test_comparison_and_future_ack_mutants(self):
        source = (Path(__file__).parent / 'oss_kernel.py').read_text()
        for anchor, replacement in [
                ('if identity not in acknowledgements:', 'if False:'),
                ('acknowledgements = set()',
                 "acknowledgements = {(e['name'], e['data_id'], e['length'], e['sha256']) "
                 "for e in observation['events'] if e['kind'] == 'data_ack'}")]:
            with self.subTest(anchor=anchor):
                self.assertEqual(source.count(anchor), 1)
                module = types.ModuleType('entry_order_mutant')
                exec(compile(source.replace(anchor, replacement), '<mutant>', 'exec'), module.__dict__)
                scope, observation = entry_trace()
                observation['events'][0]['kind'] = 'entry_visible'
                observation['events'][1]['kind'] = 'data_ack'
                self.assertEqual(entries_data_acknowledged(scope, observation)['verdict'], 'FAIL')
                self.assertEqual(module.entries_data_acknowledged(scope, observation)['verdict'], 'PASS')


if __name__ == '__main__':
    unittest.main()
