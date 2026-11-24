import unittest
from pathlib import Path
import types

from oss_kernel import acknowledged_writes_synced


def log_trace():
    binding = {'run': 'synthetic', 'volume': 'v', 'process': 'p1'}
    scope = {'binding': binding, 'writes': ['w1']}
    write = {'write_id': 'w1', 'lsn': 7, 'sha256': 'a' * 64}
    observation = {
        'binding': dict(binding), 'producer_exit': 0,
        'coverage': {'first': 0, 'last': 2, 'count': 3},
        'events': [dict(write, ordinal=0, kind='log_append'),
                   {'ordinal': 1, 'kind': 'log_sync', 'through_lsn': 7},
                   dict(write, ordinal=2, kind='guest_ack')],
    }
    return scope, observation


class LogAckOrder(unittest.TestCase):
    def test_ack_after_record_and_covering_sync_passes(self):
        result = acknowledged_writes_synced(*log_trace())
        self.assertEqual(result['verdict'], 'PASS')
        self.assertEqual(result['checked'], ['w1'])

    def test_missing_or_unbound_sync_capture_cannot_pass(self):
        for field, value in [('producer_exit', 1), ('binding', {'run': 'other'}),
                             ('coverage', {'first': 0, 'last': 3, 'count': 4})]:
            with self.subTest(field=field):
                scope, observation = log_trace()
                observation[field] = value
                self.assertEqual(acknowledged_writes_synced(scope, observation)['verdict'], 'UNKNOWN')
        scope, observation = log_trace()
        scope['writes'].append('unobserved')
        self.assertEqual(acknowledged_writes_synced(scope, observation)['verdict'], 'UNKNOWN')

    def test_ack_before_sync_is_not_repaired_by_a_later_sync(self):
        scope, observation = log_trace()
        observation['events'][1], observation['events'][2] = observation['events'][2], observation['events'][1]
        for ordinal, event in enumerate(observation['events']):
            event['ordinal'] = ordinal
        result = acknowledged_writes_synced(scope, observation)
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['violations'][0]['ordinal'], 1)

    def test_sync_must_cover_matching_appended_bytes(self):
        for index, field, value in [(1, 'through_lsn', 6), (2, 'lsn', 8),
                                     (2, 'sha256', 'b' * 64)]:
            with self.subTest(field=field):
                scope, observation = log_trace()
                observation['events'][index][field] = value
                self.assertEqual(acknowledged_writes_synced(scope, observation)['verdict'], 'FAIL')

    def test_missing_append_or_missing_sync_fails(self):
        for omitted in (0, 1):
            with self.subTest(omitted=omitted):
                scope, observation = log_trace()
                observation['events'].pop(omitted)
                for ordinal, event in enumerate(observation['events']):
                    event['ordinal'] = ordinal
                observation['coverage'] = {'first': 0, 'last': 1, 'count': 2}
                self.assertEqual(acknowledged_writes_synced(scope, observation)['verdict'], 'FAIL')

    def test_sync_before_append_is_not_its_sync(self):
        scope, observation = log_trace()
        observation['events'][0], observation['events'][1] = observation['events'][1], observation['events'][0]
        for ordinal, event in enumerate(observation['events']):
            event['ordinal'] = ordinal
        self.assertEqual(acknowledged_writes_synced(scope, observation)['verdict'], 'FAIL')

    def test_unknown_event_or_process_incarnation_is_unknown(self):
        for field, value in [('kind', 'sync_requested'), ('through_lsn', True)]:
            with self.subTest(field=field):
                scope, observation = log_trace()
                observation['events'][1][field] = value
                self.assertEqual(acknowledged_writes_synced(scope, observation)['verdict'], 'UNKNOWN')
        scope, observation = log_trace()
        del scope['binding']['process']
        del observation['binding']['process']
        self.assertEqual(acknowledged_writes_synced(scope, observation)['verdict'], 'UNKNOWN')

    def test_unsynced_record_presence_mutant(self):
        source = (Path(__file__).parent / 'oss_kernel.py').read_text()
        anchor = 'if record not in durable:'
        self.assertEqual(source.count(anchor), 1)
        module = types.ModuleType('log_ack_mutant')
        exec(compile(source.replace(anchor, 'if record not in records:'), '<mutant>', 'exec'), module.__dict__)
        scope, observation = log_trace()
        observation['events'][1]['through_lsn'] = 6
        self.assertEqual(acknowledged_writes_synced(scope, observation)['verdict'], 'FAIL')
        self.assertEqual(module.acknowledged_writes_synced(scope, observation)['verdict'], 'PASS')


if __name__ == '__main__':
    unittest.main()
