from copy import deepcopy
import unittest
import ast
from pathlib import Path
import types
from oss_kernel import root_names_exist as filer_root_names_exist


def check_graph(judge, expected_heads, heads, objects, **options):
    return judge(
        expected_heads, lambda volume: heads.get(volume), lambda name: objects.get(name), **options
    )


def root_names_exist(expected_heads, heads, objects, **options):
    return check_graph(filer_root_names_exist, expected_heads, heads, objects, **options)


def snapshot_graph():
    heads = {
        'target': {
            'producer_exit': 0,
            'exists': True,
            'state': 'active',
            'current': 'target/root/live',
            'snapshots': {'saved': {'root': 'target/root/saved'}},
            'generation': 'g1',
        }
    }
    objects = {
        'target/root/live': {'producer_exit': 0, 'exists': True, 'references': ['target/seg/live']},
        'target/seg/live': {'producer_exit': 0, 'exists': True, 'references': ['target/pool/live']},
        'target/pool/live': {'producer_exit': 0, 'exists': True, 'references': []},
        'target/root/saved': {
            'producer_exit': 0,
            'exists': True,
            'references': ['target/seg/saved'],
        },
        'target/seg/saved': {
            'producer_exit': 0,
            'exists': True,
            'references': ['source/pool/shared'],
        },
        'source/pool/shared': {'producer_exit': 0, 'exists': True, 'references': []},
        'target/pool/shared': {'producer_exit': 0, 'exists': True, 'references': []},
    }
    return (heads, objects)


class RootNames(unittest.TestCase):
    def test_changed_head_retries_and_discards_old_graph_findings(self):
        heads, objects = snapshot_graph()
        old = heads['target']
        new = deepcopy(old)
        new.update(generation='g2', snapshots={})
        objects['target/seg/saved'].update(exists=False, references=[])
        responses = iter([old, new, new, new])
        result = filer_root_names_exist(
            ['target'], lambda _: next(responses), lambda name: objects[name], max_attempts=2
        )
        self.assertEqual(result['verdict'], 'PASS')
        self.assertEqual(result['missing'], [])
        self.assertEqual(result['generations'], {'target': 'g2'})

    def test_generation_churn_exhausts_a_bounded_retry_as_unknown(self):
        heads, objects = snapshot_graph()
        calls = []

        def read_head(volume):
            calls.append(volume)
            return dict(heads[volume], generation='g' + str(len(calls)))

        result = filer_root_names_exist(
            ['target'], read_head, lambda name: objects[name], max_attempts=2
        )
        self.assertEqual(result['verdict'], 'UNKNOWN')
        self.assertEqual(result['generations'], {})
        self.assertEqual(len(calls), 4)

    def test_failed_head_reread_does_not_certify_a_missing_old_name(self):
        heads, objects = snapshot_graph()
        objects['target/seg/saved'].update(exists=False, references=[])
        calls = []

        def read_head(volume):
            calls.append(volume)
            if len(calls) == 2:
                raise OSError('filer read failed')
            return heads[volume]

        result = filer_root_names_exist(['target'], read_head, lambda name: objects[name])
        self.assertEqual(result['verdict'], 'UNKNOWN')
        self.assertEqual(result['missing'], [])

    def test_omitted_generation_recheck_mutant(self):
        source = (Path(__file__).parent / 'oss_kernel.py').read_text()
        anchor = 'if before != after:'
        self.assertEqual(source.count(anchor), 1)
        module = types.ModuleType('filer_generation_mutant')
        exec(compile(source.replace(anchor, 'if False:'), '<mutant>', 'exec'), module.__dict__)

        def run(judge):
            heads, objects = snapshot_graph()
            responses = iter([heads['target'], dict(heads['target'], generation='g2')])
            return judge(
                ['target'], lambda _: next(responses), lambda name: objects[name], max_attempts=1
            )

        self.assertEqual(run(filer_root_names_exist)['verdict'], 'UNKNOWN')
        self.assertEqual(run(module.root_names_exist)['verdict'], 'PASS')

    def test_direct_filer_head_is_reread_after_the_graph(self):
        heads, objects = snapshot_graph()
        calls = []

        def read_head(volume):
            calls.append(('HEAD', volume))
            return heads[volume]

        def read_object(name):
            calls.append(('object', name))
            return objects[name]

        result = filer_root_names_exist(['target'], read_head, read_object)
        self.assertEqual(result['verdict'], 'PASS')
        self.assertEqual(calls[0], ('HEAD', 'target'))
        self.assertEqual(calls[-1], ('HEAD', 'target'))
        self.assertEqual(sum((kind == 'HEAD' for kind, _ in calls)), 2)

    def test_deleted_segment_fault_is_a_reference_violation(self):
        heads, objects = snapshot_graph()
        self.assertEqual(root_names_exist(['target'], heads, objects)['verdict'], 'PASS')
        objects['target/seg/saved'].update(exists=False, references=[])
        result = root_names_exist(['target'], heads, objects)
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['missing'], ['target/seg/saved'])

    def test_foreign_chunk_removed_from_source_fails_despite_target_copy(self):
        heads, objects = snapshot_graph()
        self.assertEqual(root_names_exist(['target'], heads, objects)['verdict'], 'PASS')
        objects['source/pool/shared']['exists'] = False
        result = root_names_exist(['target'], heads, objects)
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['missing'], ['source/pool/shared'])
        self.assertNotIn('target/pool/shared', result['checked'])

    def test_deleting_and_deleted_heads_do_not_read_dangling_names(self):
        for state in ('deleting', 'deleted'):
            with self.subTest(state=state):
                heads, objects = snapshot_graph()
                heads['target']['state'] = state
                objects['target/seg/saved'].update(exists=False, references=[])
                result = root_names_exist(['target'], heads, objects)
                self.assertEqual(result['verdict'], 'PASS')
                self.assertEqual(result['checked'], [])
                heads['target']['producer_exit'] = 1
                self.assertEqual(root_names_exist(['target'], heads, objects)['verdict'], 'UNKNOWN')

    def test_flattened_roots_cannot_hide_unobserved_snapshot_fields(self):
        heads, objects = snapshot_graph()
        del heads['target']['snapshots']
        heads['target']['roots'] = ['target/root/live']
        self.assertEqual(root_names_exist(['target'], heads, objects)['verdict'], 'UNKNOWN')
        for snapshot in (None, {}, {'root': ''}):
            with self.subTest(snapshot=snapshot):
                heads, objects = snapshot_graph()
                heads['target']['snapshots']['saved'] = snapshot
                self.assertEqual(root_names_exist(['target'], heads, objects)['verdict'], 'UNKNOWN')

    def test_segment_snapshot_and_foreign_owner_source_mutants(self):
        source = (Path(__file__).parent / 'oss_kernel.py').read_text()
        for anchor, replacement, deleted in [
            ('for snapshot in snapshots.values():', 'for snapshot in ():', 'source/pool/shared'),
            (
                'observation = _read_observation(read_object, name)',
                "observation = _read_observation(read_object, 'target/' + name.split('/', 1)[-1])",
                'source/pool/shared',
            ),
            ('missing.add(name)', 'pass', 'target/seg/saved'),
        ]:
            with self.subTest(anchor=anchor):
                self.assertEqual(source.count(anchor), 1)
                module = types.ModuleType('root_resolution_mutant')
                exec(
                    compile(source.replace(anchor, replacement), '<mutant>', 'exec'),
                    module.__dict__,
                )
                heads, objects = snapshot_graph()
                objects[deleted].update(exists=False, references=[])
                self.assertEqual(root_names_exist(['target'], heads, objects)['verdict'], 'FAIL')
                self.assertEqual(
                    check_graph(module.root_names_exist, ['target'], heads, objects)['verdict'],
                    'PASS',
                )

    def test_current_and_every_snapshot_root_are_traversed(self):
        heads = {
            'target': {
                'producer_exit': 0,
                'exists': True,
                'state': 'active',
                'current': 'target/root/live',
                'snapshots': {
                    'first': {'root': 'target/root/first'},
                    'second': {'root': 'target/root/second'},
                },
                'generation': 'g1',
            }
        }
        objects = {
            'target/root/live': {'producer_exit': 0, 'exists': True, 'references': []},
            'target/root/first': {'producer_exit': 0, 'exists': True, 'references': []},
            'target/root/second': {
                'producer_exit': 0,
                'exists': True,
                'references': ['source/pool/h'],
            },
            'source/pool/h': {'producer_exit': 0, 'exists': True, 'references': []},
        }
        self.assertEqual(root_names_exist(['target'], heads, objects)['verdict'], 'PASS')
        objects['source/pool/h']['exists'] = False
        result = root_names_exist(['target'], heads, objects)
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['missing'], ['source/pool/h'])

    def test_deleting_head_is_outside_the_reference_invariant(self):
        heads = {
            'v': {
                'producer_exit': 0,
                'exists': True,
                'state': 'deleting',
                'current': 'missing',
                'snapshots': {},
                'generation': 'g1',
            }
        }
        objects = {'missing': {'producer_exit': 0, 'exists': False, 'references': []}}
        self.assertEqual(root_names_exist(['v'], heads, objects)['verdict'], 'PASS')
        heads['v']['state'] = 'active'
        self.assertEqual(root_names_exist(['v'], heads, objects)['verdict'], 'FAIL')

    def test_deleted_head_is_also_outside_the_reference_invariant(self):
        heads = {
            'v': {
                'producer_exit': 0,
                'exists': True,
                'state': 'deleted',
                'current': 'missing',
                'snapshots': {},
                'generation': 'g1',
            }
        }
        objects = {'missing': {'producer_exit': 0, 'exists': False, 'references': []}}
        self.assertEqual(root_names_exist(['v'], heads, objects)['verdict'], 'PASS')

    def test_unknown_head_state_cannot_be_assumed_eligible(self):
        for state in (None, 'unknown', False):
            with self.subTest(state=state):
                heads = {
                    'v': {
                        'producer_exit': 0,
                        'exists': True,
                        'state': state,
                        'current': None,
                        'snapshots': {},
                        'generation': 'g1',
                    }
                }
                self.assertEqual(root_names_exist(['v'], heads, {})['verdict'], 'UNKNOWN')

    def test_deletion_state_exemption_mutant(self):
        source = (Path(__file__).parent / 'oss_kernel.py').read_text()
        anchor = "if head['state'] in ('deleting', 'deleted'):"
        self.assertEqual(source.count(anchor), 1)
        module = types.ModuleType('kernel_state_mutant')
        exec(compile(source.replace(anchor, 'if False:'), '<mutant>', 'exec'), module.__dict__)
        heads = {
            'v': {
                'producer_exit': 0,
                'exists': True,
                'state': 'deleting',
                'current': 'r',
                'snapshots': {},
                'generation': 'g1',
            }
        }
        objects = {'r': {'producer_exit': 0, 'exists': False, 'references': []}}
        for state in ('deleting', 'deleted'):
            with self.subTest(state=state):
                heads['v']['state'] = state
                self.assertEqual(root_names_exist(['v'], heads, objects)['verdict'], 'PASS')
                self.assertEqual(
                    check_graph(module.root_names_exist, ['v'], heads, objects)['verdict'], 'FAIL'
                )

    def test_complete_foreign_owner_graph(self):
        heads = {
            'target': {
                'producer_exit': 0,
                'exists': True,
                'state': 'active',
                'current': 'target/root/h',
                'snapshots': {},
                'generation': 'g1',
            }
        }
        objects = {
            'target/root/h': {'producer_exit': 0, 'exists': True, 'references': ['target/seg/h']},
            'target/seg/h': {'producer_exit': 0, 'exists': True, 'references': ['source/pool/0-h']},
            'source/pool/0-h': {'producer_exit': 0, 'exists': True, 'references': []},
        }
        result = root_names_exist(['target'], heads, objects)
        self.assertEqual(result['verdict'], 'PASS')
        self.assertEqual(set(result['checked']), set(objects))

    def test_observed_missing_name_fails(self):
        heads = {
            'v': {
                'producer_exit': 0,
                'exists': True,
                'state': 'active',
                'current': 'v/root/h',
                'snapshots': {},
                'generation': 'g1',
            }
        }
        objects = {
            'v/root/h': {'producer_exit': 0, 'exists': True, 'references': ['owner/pool/h']},
            'owner/pool/h': {'producer_exit': 0, 'exists': False, 'references': []},
        }
        result = root_names_exist(['v'], heads, objects)
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['missing'], ['owner/pool/h'])

    def test_incomplete_or_failed_observation_is_unknown(self):
        cases = [
            (['v'], {}, {}),
            (
                ['v'],
                {
                    'v': {
                        'producer_exit': 1,
                        'exists': False,
                        'current': None,
                        'snapshots': {},
                        'generation': None,
                    }
                },
                {},
            ),
            (
                ['v'],
                {
                    'v': {
                        'producer_exit': 0,
                        'exists': True,
                        'state': 'active',
                        'current': 'r',
                        'snapshots': {},
                        'generation': 'g1',
                    }
                },
                {},
            ),
            (
                ['v'],
                {
                    'v': {
                        'producer_exit': 0,
                        'exists': True,
                        'state': 'active',
                        'current': 'r',
                        'snapshots': {},
                        'generation': 'g1',
                    }
                },
                {'r': {'producer_exit': 1, 'exists': False, 'references': []}},
            ),
            (
                ['v'],
                {
                    'v': {
                        'producer_exit': 0,
                        'exists': False,
                        'current': 'r',
                        'snapshots': {},
                        'generation': None,
                    }
                },
                {'r': {'producer_exit': 0, 'exists': True, 'references': []}},
            ),
            ([], {}, {}),
            (
                ['v', 'v'],
                {
                    'v': {
                        'producer_exit': 0,
                        'exists': False,
                        'current': None,
                        'snapshots': {},
                        'generation': None,
                    }
                },
                {},
            ),
            (
                ['v'],
                {
                    'v': {
                        'producer_exit': False,
                        'exists': False,
                        'current': None,
                        'snapshots': {},
                        'generation': None,
                    }
                },
                {},
            ),
        ]
        for inputs in cases:
            with self.subTest(inputs=inputs):
                self.assertEqual(root_names_exist(*inputs)['verdict'], 'UNKNOWN')

    def test_known_violation_survives_another_failed_read(self):
        heads = {
            'v': {
                'producer_exit': 0,
                'exists': True,
                'state': 'active',
                'current': 'missing',
                'snapshots': {'snapshot1': {'root': 'unreadable'}},
                'generation': 'g1',
            }
        }
        objects = {
            'missing': {'producer_exit': 0, 'exists': False, 'references': []},
            'unreadable': {'producer_exit': 1, 'exists': False, 'references': []},
        }
        result = root_names_exist(['v'], heads, objects)
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertEqual(result['missing'], ['missing'])
        self.assertIn('unreadable', result['unknown'])

    def test_other_owner_cannot_satisfy_a_reference(self):
        heads = {
            'v': {
                'producer_exit': 0,
                'exists': True,
                'state': 'active',
                'current': 'owner/pool/h',
                'snapshots': {},
                'generation': 'g1',
            }
        }
        objects = {'other/pool/h': {'producer_exit': 0, 'exists': True, 'references': []}}
        result = root_names_exist(['v'], heads, objects)
        self.assertEqual(result['verdict'], 'UNKNOWN')
        self.assertEqual(result['unknown'], ['owner/pool/h'])

    def test_cycle_terminates_without_claiming_flatness(self):
        heads = {
            'v': {
                'producer_exit': 0,
                'exists': True,
                'state': 'active',
                'current': 'r',
                'snapshots': {},
                'generation': 'g1',
            }
        }
        objects = {
            'r': {'producer_exit': 0, 'exists': True, 'references': ['s']},
            's': {'producer_exit': 0, 'exists': True, 'references': ['r']},
        }
        self.assertEqual(
            root_names_exist(['v'], heads, objects),
            {
                'verdict': 'PASS',
                'checked': ['r', 's'],
                'missing': [],
                'unknown': [],
                'generations': {'v': 'g1'},
            },
        )

    def test_absent_head_does_not_create_a_missing_reference(self):
        heads = {
            'v': {
                'producer_exit': 0,
                'exists': False,
                'current': None,
                'snapshots': {},
                'generation': None,
            }
        }
        self.assertEqual(root_names_exist(['v'], heads, {})['verdict'], 'PASS')

    def test_definite_absence_survives_duplicate_scope(self):
        heads = {
            'v': {
                'producer_exit': 0,
                'exists': True,
                'state': 'active',
                'current': 'r',
                'snapshots': {},
                'generation': 'g1',
            }
        }
        objects = {'r': {'producer_exit': 0, 'exists': False, 'references': []}}
        result = root_names_exist(['v', 'v'], heads, objects)
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertTrue(result['unknown'])

    def test_invalid_object_shape_is_unknown(self):
        heads = {
            'v': {
                'producer_exit': 0,
                'exists': True,
                'state': 'active',
                'current': 'r',
                'snapshots': {},
                'generation': 'g1',
            }
        }
        for observation in [
            None,
            {},
            {'producer_exit': 0, 'exists': True},
            {'producer_exit': 0, 'exists': True, 'references': ['']},
            {'producer_exit': 0, 'exists': False, 'references': ['s']},
        ]:
            with self.subTest(observation=observation):
                self.assertEqual(
                    root_names_exist(['v'], heads, {'r': observation})['verdict'], 'UNKNOWN'
                )

    def test_missing_and_failed_read_mutants(self):
        source = (Path(__file__).parent / 'oss_kernel.py').read_text()
        heads = {
            'v': {
                'producer_exit': 0,
                'exists': True,
                'state': 'active',
                'current': 'r',
                'snapshots': {},
                'generation': 'g1',
            }
        }
        for anchor, replacement, objects, expected in [
            (
                'missing.add(name)',
                'pass',
                {'r': {'producer_exit': 0, 'exists': False, 'references': []}},
                'FAIL',
            ),
            ('unknown.add(name)', 'pass', {}, 'UNKNOWN'),
        ]:
            with self.subTest(anchor=anchor):
                self.assertEqual(source.count(anchor), 1)
                module = types.ModuleType('kernel_mutant')
                exec(
                    compile(source.replace(anchor, replacement), '<mutant>', 'exec'),
                    module.__dict__,
                )
                self.assertEqual(root_names_exist(['v'], heads, objects)['verdict'], expected)
                self.assertEqual(
                    check_graph(module.root_names_exist, ['v'], heads, objects)['verdict'], 'PASS'
                )

    def test_deciding_function_and_runtime_export_inventory(self):
        sites = []
        entries = {
            'root_names_exist',
            'entries_match_names',
            'snapshot_removals_safe',
            'acknowledged_writes_survive',
            'head_updates_conditional',
        }
        for path in Path(__file__).parent.glob('*.py'):
            if path.name.startswith('test_'):
                continue
            for node in ast.walk(ast.parse(path.read_text())):
                if isinstance(node, ast.FunctionDef) and node.name in entries:
                    sites.append((path.name, 'definition', node.name))
                if isinstance(node, ast.Assign):
                    for target in node.targets:
                        if isinstance(target, ast.Attribute) and target.attr in entries:
                            sites.append((path.name, 'export', target.attr))
                if isinstance(node, ast.Call):
                    name = getattr(node.func, 'id', getattr(node.func, 'attr', None))
                    if name in entries:
                        sites.append((path.name, 'consumer', name))
        expected = []
        for entry in entries:
            if entry == 'acknowledged_writes_survive':
                expected.append(('facts.py', 'definition', entry))
            else:
                expected.extend(
                    [('oss_kernel.py', 'definition', entry), ('loader.py', 'export', entry)]
                )
        self.assertCountEqual(sites, expected)
        import json

        manifest = json.loads((Path(__file__).parent / 'library.json').read_text())
        self.assertTrue(entries <= set(manifest['entries']))
        self.assertEqual(len(manifest['entries']), len(set(manifest['entries'])))


if __name__ == '__main__':
    unittest.main()
