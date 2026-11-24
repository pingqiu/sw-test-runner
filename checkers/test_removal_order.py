import unittest
from pathlib import Path
import types

from oss_kernel import snapshot_removals_safe


def removal_trace():
    binding = {'run': 'synthetic', 'volume': 'v'}
    scope = {'binding': binding, 'names': ['v/pool/h']}
    observation = {
        'binding': dict(binding), 'producer_exit': 0,
        'initial_activity': {'kind': 'snap', 'session': 's1'},
        'coverage': {'first': 0, 'last': 0, 'count': 1},
        'events': [{'ordinal': 0, 'kind': 'remove_name', 'name': 'v/pool/h'}],
    }
    return scope, observation


class RemovalOrder(unittest.TestCase):
    def test_name_removal_while_head_says_snapshot_fails(self):
        result = snapshot_removals_safe(*removal_trace())
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['violations'][0]['name'], 'v/pool/h')

    def test_unreadable_or_unknown_activity_is_not_assumed_idle(self):
        for field, value in [('initial_activity', None), ('initial_activity', {'kind': 'unknown'}),
                             ('initial_activity', {'kind': 'snap'}), ('producer_exit', 1),
                             ('binding', {'run': 'other', 'volume': 'v'})]:
            with self.subTest(field=field, value=value):
                scope, observation = removal_trace()
                observation[field] = value
                self.assertEqual(snapshot_removals_safe(scope, observation)['verdict'], 'UNKNOWN')

    def test_recorded_idle_before_removal_closes_the_snapshot(self):
        scope, observation = removal_trace()
        observation['events'].insert(0, {'ordinal': 0, 'kind': 'head_activity',
                                         'activity': {'kind': 'idle'}})
        observation['events'][1]['ordinal'] = 1
        observation['coverage'] = {'first': 0, 'last': 1, 'count': 2}
        self.assertEqual(snapshot_removals_safe(scope, observation)['verdict'], 'PASS')
        observation['events'].reverse()
        for ordinal, event in enumerate(observation['events']):
            event['ordinal'] = ordinal
        self.assertEqual(snapshot_removals_safe(scope, observation)['verdict'], 'FAIL')

    def test_process_memory_cannot_override_head_activity(self):
        scope, observation = removal_trace()
        observation['process_activity'] = {'kind': 'idle'}
        self.assertEqual(snapshot_removals_safe(scope, observation)['verdict'], 'FAIL')

    def test_idle_and_clean_satisfy_this_fact_only(self):
        for activity in [{'kind': 'idle'}, {'kind': 'clean', 'session': 'c1'}]:
            with self.subTest(activity=activity):
                scope, observation = removal_trace()
                observation['initial_activity'] = activity
                self.assertEqual(snapshot_removals_safe(scope, observation)['verdict'], 'PASS')

    def test_missing_removal_or_incoherent_capture_is_unknown(self):
        scope, observation = removal_trace()
        observation['initial_activity'] = {'kind': 'idle'}
        scope['names'].append('v/root/other')
        self.assertEqual(snapshot_removals_safe(scope, observation)['verdict'], 'UNKNOWN')
        for event in [{'ordinal': 1, 'kind': 'remove_name', 'name': 'v/pool/h'},
                      {'ordinal': 0, 'kind': 'process_exit'},
                      {'ordinal': 0, 'kind': 'head_activity', 'activity': {'kind': 'unknown'}}]:
            with self.subTest(event=event):
                scope, observation = removal_trace()
                observation['events'] = [event]
                self.assertEqual(snapshot_removals_safe(scope, observation)['verdict'], 'UNKNOWN')

    def test_snapshot_and_capture_failure_mutants(self):
        source = (Path(__file__).parent / 'oss_kernel.py').read_text()
        for anchor, replacement, producer_failed in [
                ("if activity['kind'] == 'snap':", 'if False:', False),
                ('and type(status) is int and status == 0', 'and True', True)]:
            with self.subTest(anchor=anchor):
                self.assertEqual(source.count(anchor), 1)
                module = types.ModuleType('removal_order_mutant')
                exec(compile(source.replace(anchor, replacement), '<mutant>', 'exec'), module.__dict__)
                scope, observation = removal_trace()
                if producer_failed:
                    observation.update(producer_exit=1, initial_activity={'kind': 'idle'})
                expected = 'UNKNOWN' if producer_failed else 'FAIL'
                self.assertEqual(snapshot_removals_safe(scope, observation)['verdict'], expected)
                self.assertEqual(module.snapshot_removals_safe(scope, observation)['verdict'], 'PASS')


if __name__ == '__main__':
    unittest.main()
