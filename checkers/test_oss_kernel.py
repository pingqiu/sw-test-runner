import unittest
import ast
from pathlib import Path
import types
from oss_kernel import root_names_exist


def snapshot_graph():
    heads = {
        'target': {
            'producer_exit': 0,
            'exists': True,
            'state': 'active',
            'current': 'target/root/live',
            'snapshots': {'saved': {'root': 'target/root/saved'}},
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
    return heads, objects


class RootNames(unittest.TestCase):
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
                'observation = objects.get(name)',
                "observation = objects.get('target/' + name.split('/', 1)[-1], objects.get(name))",
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
                    module.root_names_exist(['target'], heads, objects)['verdict'], 'PASS'
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
            }
        }
        objects = {'r': {'producer_exit': 0, 'exists': False, 'references': []}}
        for state in ('deleting', 'deleted'):
            with self.subTest(state=state):
                heads['v']['state'] = state
                self.assertEqual(root_names_exist(['v'], heads, objects)['verdict'], 'PASS')
                self.assertEqual(module.root_names_exist(['v'], heads, objects)['verdict'], 'FAIL')

    def test_complete_foreign_owner_graph(self):
        heads = {
            'target': {
                'producer_exit': 0,
                'exists': True,
                'state': 'active',
                'current': 'target/root/h',
                'snapshots': {},
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
                {'v': {'producer_exit': 1, 'exists': False, 'current': None, 'snapshots': {}}},
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
                    }
                },
                {'r': {'producer_exit': 1, 'exists': False, 'references': []}},
            ),
            (
                ['v'],
                {'v': {'producer_exit': 0, 'exists': False, 'current': 'r', 'snapshots': {}}},
                {'r': {'producer_exit': 0, 'exists': True, 'references': []}},
            ),
            ([], {}, {}),
            (
                ['v', 'v'],
                {'v': {'producer_exit': 0, 'exists': False, 'current': None, 'snapshots': {}}},
                {},
            ),
            (
                ['v'],
                {'v': {'producer_exit': False, 'exists': False, 'current': None, 'snapshots': {}}},
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
            }
        }
        objects = {
            'r': {'producer_exit': 0, 'exists': True, 'references': ['s']},
            's': {'producer_exit': 0, 'exists': True, 'references': ['r']},
        }
        self.assertEqual(
            root_names_exist(['v'], heads, objects),
            {'verdict': 'PASS', 'checked': ['r', 's'], 'missing': [], 'unknown': []},
        )

    def test_absent_head_does_not_create_a_missing_reference(self):
        heads = {'v': {'producer_exit': 0, 'exists': False, 'current': None, 'snapshots': {}}}
        self.assertEqual(root_names_exist(['v'], heads, {})['verdict'], 'PASS')

    def test_definite_absence_survives_duplicate_scope(self):
        heads = {
            'v': {
                'producer_exit': 0,
                'exists': True,
                'state': 'active',
                'current': 'r',
                'snapshots': {},
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
                self.assertEqual(module.root_names_exist(['v'], heads, objects)['verdict'], 'PASS')

    def test_deciding_function_and_runtime_export_inventory(self):
        sites = []
        entries = {
            'root_names_exist',
            'entries_data_acknowledged',
            'snapshot_removals_safe',
            'acknowledged_writes_synced',
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
        self.assertCountEqual(
            sites,
            [
                (file, kind, entry)
                for entry in entries
                for file, kind in [('oss_kernel.py', 'definition'), ('loader.py', 'export')]
            ],
        )


if __name__ == '__main__':
    unittest.main()
