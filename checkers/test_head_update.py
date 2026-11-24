import unittest
from pathlib import Path
import types

from oss_kernel import head_updates_conditional


def head_trace():
    binding = {'run': 'synthetic', 'volume': 'v'}
    scope = {'binding': binding, 'head': 'v/HEAD', 'updates': ['u1']}
    event = {
        'ordinal': 0, 'kind': 'head_update', 'update_id': 'u1',
        'authority': {'epoch': 7, 'holder': 'h1'},
        'writer': {'epoch': 7, 'holder': 'h1'},
        'before': {'generation': 'g1', 'epoch': 7, 'holder': 'h1'},
        'after': {'generation': 'g2', 'epoch': 7, 'holder': 'h1'},
        'writes': [{'entry': 'v/HEAD', 'operation': 'conditional_update',
                    'expected_generation': 'g1', 'expected_epoch': 7}],
    }
    observation = {'binding': dict(binding), 'producer_exit': 0,
                   'coverage': {'first': 0, 'last': 0, 'count': 1}, 'events': [event]}
    return scope, observation


class HeadUpdate(unittest.TestCase):
    def test_current_holder_conditional_update_passes(self):
        result = head_updates_conditional(*head_trace())
        self.assertEqual(result['verdict'], 'PASS')
        self.assertEqual(result['checked'], ['u1'])

    def test_missing_or_failed_transition_evidence_is_unknown(self):
        for field, value in [('producer_exit', 1), ('binding', {'run': 'other'}),
                             ('coverage', {'first': 0, 'last': 1, 'count': 2})]:
            with self.subTest(field=field):
                scope, observation = head_trace()
                observation[field] = value
                self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'UNKNOWN')
        for field, value in [('writes', []), ('before', {}), ('authority', None)]:
            with self.subTest(field=field):
                scope, observation = head_trace()
                observation['events'][0][field] = value
                self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'UNKNOWN')

    def test_claim_advances_from_the_observed_old_epoch(self):
        scope, observation = head_trace()
        event = observation['events'][0]
        event['before'].update(epoch=6, holder='old')
        event['writes'][0]['expected_epoch'] = 6
        self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'PASS')
        event['writes'][0]['expected_epoch'] = 7
        self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'FAIL')

    def test_current_authority_and_same_epoch_holder_are_required(self):
        for field, value in [('authority', {'epoch': 8, 'holder': 'h2'}),
                             ('before', {'epoch': 7, 'holder': 'other', 'generation': 'g1'}),
                             ('after', {'epoch': 6, 'holder': 'h1', 'generation': 'g2'})]:
            with self.subTest(field=field):
                scope, observation = head_trace()
                observation['events'][0][field] = value
                self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'FAIL')

    def test_generation_and_epoch_conditions_cannot_be_missing_or_stale(self):
        for field, value in [('expected_generation', 'older'), ('expected_generation', None),
                             ('expected_epoch', 6), ('expected_epoch', None)]:
            with self.subTest(field=field, value=value):
                scope, observation = head_trace()
                observation['events'][0]['writes'][0][field] = value
                self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'FAIL')
        scope, observation = head_trace()
        observation['events'][0]['after']['generation'] = 'g1'
        self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'FAIL')

    def test_exactly_one_conditional_head_mutation_is_required(self):
        for field, value in [('entry', 'v/VOLUME'), ('operation', 'unconditional_update')]:
            with self.subTest(field=field):
                scope, observation = head_trace()
                observation['events'][0]['writes'][0][field] = value
                self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'FAIL')
        scope, observation = head_trace()
        writes = observation['events'][0]['writes']
        writes.append(dict(writes[0]))
        self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'FAIL')

    def test_missing_expected_transition_is_unknown(self):
        scope, observation = head_trace()
        scope['updates'].append('u2')
        self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'UNKNOWN')
        observation['events'][0]['writes'][0]['expected_epoch'] = 6
        self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'FAIL')

    def test_unknown_wire_condition_is_not_inferred(self):
        scope, observation = head_trace()
        del observation['events'][0]['writes'][0]['expected_epoch']
        self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'UNKNOWN')

    def test_condition_authority_and_single_write_mutants(self):
        source = (Path(__file__).parent / 'oss_kernel.py').read_text()
        for anchor, mutation in [
                ("if write['expected_generation'] != before['generation']:", 'generation'),
                ("if write['expected_epoch'] != before['epoch']:", 'epoch'),
                ('if len(writes) != 1:', 'multiplicity'),
                ("if (writer['epoch'], writer['holder']) != (authority['epoch'], authority['holder']):", 'authority')]:
            with self.subTest(mutation=mutation):
                self.assertEqual(source.count(anchor), 1)
                module = types.ModuleType('head_update_mutant')
                exec(compile(source.replace(anchor, 'if False:'), '<mutant>', 'exec'), module.__dict__)
                scope, observation = head_trace()
                event = observation['events'][0]
                if mutation == 'generation':
                    event['writes'][0]['expected_generation'] = 'older'
                elif mutation == 'epoch':
                    event['writes'][0]['expected_epoch'] = 6
                elif mutation == 'multiplicity':
                    event['writes'].append(dict(event['writes'][0]))
                else:
                    event['authority'] = {'epoch': 8, 'holder': 'h2'}
                self.assertEqual(head_updates_conditional(scope, observation)['verdict'], 'FAIL')
                self.assertEqual(module.head_updates_conditional(scope, observation)['verdict'], 'PASS')


if __name__ == '__main__':
    unittest.main()
