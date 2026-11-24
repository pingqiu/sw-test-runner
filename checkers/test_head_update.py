import unittest
import types
from copy import deepcopy
from pathlib import Path
from oss_kernel import head_updates_ordered


def head_trace():
    binding = {'run': 'synthetic', 'volume': 'v'}
    before = {'generation': 4, 'epoch': 7, 'holder': 'h1'}
    event = {
        'ordinal': 0,
        'kind': 'head_update',
        'entry': 'v/HEAD',
        'update_id': 'u1',
        'before': dict(before),
        'after': dict(before, generation=5),
        'authority_revision_before': 12,
    }
    scope = {'binding': binding, 'head': 'v/HEAD', 'updates': ['u1']}
    observation = {
        'binding': dict(binding),
        'producer_exit': 0,
        'source': 'filer_metadata',
        'stream_id': 'filer-stream',
        'initial_head': dict(before),
        'coverage': {'first': 0, 'last': 0, 'count': 1},
        'events': [event],
        'authority_store': {
            'binding': dict(binding),
            'producer_exit': 0,
            'source': 'authority_store',
            'records': [{'revision': 12, 'epoch': 7, 'holder': 'h1'}],
        },
    }
    return scope, observation


class HeadUpdate(unittest.TestCase):
    def test_two_events_cannot_publish_the_same_new_generation(self):
        scope, observation = head_trace()
        scope['updates'].append('u2')
        observation['events'].append(
            dict(deepcopy(observation['events'][0]), ordinal=1, update_id='u2')
        )
        observation['coverage'] = {'first': 0, 'last': 1, 'count': 2}
        self.assertEqual(head_updates_ordered(scope, observation)['verdict'], 'FAIL')

    def test_filer_generation_increments_exactly_once_under_granted_line(self):
        self.assertEqual(head_updates_ordered(*head_trace())['verdict'], 'PASS')

    def test_repeated_or_skipped_generation_is_a_violation(self):
        for generation in (4, 6):
            scope, observation = head_trace()
            observation['events'][0]['after']['generation'] = generation
            self.assertEqual(head_updates_ordered(scope, observation)['verdict'], 'FAIL')

    def test_higher_epoch_claim_matches_a_previously_granted_line(self):
        scope, observation = head_trace()
        observation['authority_store']['records'].append(
            {'revision': 15, 'epoch': 8, 'holder': 'h2'}
        )
        event = observation['events'][0]
        event['after'].update(epoch=8, holder='h2')
        event['authority_revision_before'] = 15
        self.assertEqual(head_updates_ordered(scope, observation)['verdict'], 'PASS')
        event['after']['epoch'] = 9
        self.assertEqual(head_updates_ordered(scope, observation)['verdict'], 'FAIL')

    def test_lower_epoch_or_ungranted_holder_is_a_violation(self):
        for field, value in [('epoch', 6), ('epoch', 8), ('holder', 'other')]:
            scope, observation = head_trace()
            observation['events'][0]['after'][field] = value
            self.assertEqual(head_updates_ordered(scope, observation)['verdict'], 'FAIL')

    def test_existing_volume_head_cannot_be_created_even_after_deletion(self):
        for delete_first in (False, True):
            scope, observation = head_trace()
            observation['events'] = []
            if delete_first:
                observation['events'].append(
                    {'ordinal': 0, 'kind': 'delete_entry', 'entry': 'v/HEAD', 'update_id': 'u1'}
                )
            observation['events'].append(
                {
                    'ordinal': len(observation['events']),
                    'kind': 'create_entry',
                    'entry': 'v/HEAD',
                    'update_id': 'u2',
                }
            )
            scope['updates'] = [event['update_id'] for event in observation['events']]
            n = len(observation['events'])
            observation['coverage'] = {'first': 0, 'last': n - 1, 'count': n}
            self.assertEqual(head_updates_ordered(scope, observation)['verdict'], 'FAIL')

    def test_failed_source_or_missing_boundary_and_authority_join_are_unknown(self):
        for field, value in [
            ('source', 'process_wire_log'),
            ('stream_id', ''),
            ('producer_exit', 1),
            ('initial_head', None),
            ('authority_store', None),
            ('binding', {}),
        ]:
            scope, observation = head_trace()
            observation[field] = value
            self.assertEqual(head_updates_ordered(scope, observation)['verdict'], 'UNKNOWN')
        for revision in (None, True, 99):
            scope, observation = head_trace()
            observation['events'][0]['authority_revision_before'] = revision
            self.assertEqual(head_updates_ordered(scope, observation)['verdict'], 'UNKNOWN')

    def test_master_records_are_ordered_by_revision_not_clock(self):
        scope, observation = head_trace()
        records = observation['authority_store']['records']
        records[0]['clock'] = 99999
        records.append({'revision': 20, 'epoch': 8, 'holder': 'h2', 'clock': 0})
        self.assertEqual(head_updates_ordered(scope, observation)['verdict'], 'PASS')
        records.reverse()
        self.assertEqual(head_updates_ordered(scope, observation)['verdict'], 'UNKNOWN')

    def test_generation_grant_and_recreate_mutants_are_caught(self):
        source = (Path(__file__).parent / 'oss_kernel.py').read_text()
        for anchor, replacement, case in [
            ("if after['generation'] != before['generation'] + 1:", 'if False:', 'generation'),
            (
                "if (after['epoch'], after['holder']) != (grant['epoch'], grant['holder']):",
                'if False:',
                'grant',
            ),
            ("reasons.append('HEAD recreated for an existing volume')", 'pass', 'create'),
        ]:
            self.assertEqual(source.count(anchor), 1)
            mutant = source.replace(anchor, replacement)
            if case == 'generation':
                mutant = mutant.replace(
                    "if current is not None and after['generation'] != current['generation'] + 1:",
                    'if False:',
                )
            module = types.ModuleType('head_metadata_mutant')
            exec(compile(mutant, '<mutant>', 'exec'), module.__dict__)
            scope, observation = head_trace()
            if case == 'generation':
                observation['events'][0]['after']['generation'] = 6
            elif case == 'grant':
                observation['events'][0]['after']['holder'] = 'other'
            else:
                observation['events'][0]['kind'] = 'create_entry'
            self.assertEqual(head_updates_ordered(scope, observation)['verdict'], 'FAIL')
            self.assertEqual(module.head_updates_ordered(scope, observation)['verdict'], 'PASS')


if __name__ == '__main__':
    unittest.main()
