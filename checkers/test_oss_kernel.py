import unittest
import ast
from pathlib import Path
import types
from oss_kernel import root_names_exist


class RootNames(unittest.TestCase):
    def test_deleting_head_is_outside_the_reference_invariant(self):
        heads = {'v': {'producer_exit': 0, 'exists': True, 'state': 'deleting',
                       'roots': ['missing']}}
        objects = {'missing': {'producer_exit': 0, 'exists': False, 'references': []}}
        self.assertEqual(root_names_exist(['v'], heads, objects)['verdict'], 'PASS')
        heads['v']['state'] = 'active'
        self.assertEqual(root_names_exist(['v'], heads, objects)['verdict'], 'FAIL')

    def test_unknown_head_state_cannot_be_assumed_eligible(self):
        for state in (None, 'unknown', False):
            with self.subTest(state=state):
                heads = {'v': {'producer_exit': 0, 'exists': True, 'state': state, 'roots': []}}
                self.assertEqual(root_names_exist(['v'], heads, {})['verdict'], 'UNKNOWN')

    def test_deleting_exclusion_mutant(self):
        source = (Path(__file__).parent / 'oss_kernel.py').read_text()
        anchor = "if head['exists'] and head['state'] == 'deleting':"
        self.assertEqual(source.count(anchor), 1)
        module = types.ModuleType('kernel_state_mutant')
        exec(compile(source.replace(anchor, 'if False:'), '<mutant>', 'exec'), module.__dict__)
        heads = {'v': {'producer_exit': 0, 'exists': True, 'state': 'deleting', 'roots': ['r']}}
        objects = {'r': {'producer_exit': 0, 'exists': False, 'references': []}}
        self.assertEqual(root_names_exist(['v'], heads, objects)['verdict'], 'PASS')
        self.assertEqual(module.root_names_exist(['v'], heads, objects)['verdict'], 'FAIL')

    def test_complete_foreign_owner_graph(self):
        heads={'target': {'producer_exit':0, 'exists':True, 'state':'active', 'roots':['target/root/h']}}
        objects={
            'target/root/h': {'producer_exit':0, 'exists':True, 'references':['target/seg/h']},
            'target/seg/h': {'producer_exit':0, 'exists':True, 'references':['source/pool/0-h']},
            'source/pool/0-h': {'producer_exit':0, 'exists':True, 'references':[]}}
        result=root_names_exist(['target'],heads,objects)
        self.assertEqual(result['verdict'],'PASS')
        self.assertEqual(set(result['checked']),set(objects))

    def test_observed_missing_name_fails(self):
        heads={'v': {'producer_exit':0, 'exists':True, 'state':'active', 'roots':['v/root/h']}}
        objects={'v/root/h': {'producer_exit':0, 'exists':True, 'references':['owner/pool/h']},
                 'owner/pool/h': {'producer_exit':0, 'exists':False, 'references':[]}}
        result=root_names_exist(['v'],heads,objects)
        self.assertEqual(result['verdict'],'FAIL')
        self.assertEqual(result['missing'],['owner/pool/h'])

    def test_incomplete_or_failed_observation_is_unknown(self):
        cases=[
            (['v'],{},{}),
            (['v'],{'v':{'producer_exit':1,'exists':False,'roots':[]}},{}),
            (['v'],{'v':{'producer_exit':0,'exists':True,'state':'active','roots':['r']}},{}),
            (['v'],{'v':{'producer_exit':0,'exists':True,'state':'active','roots':['r']}},
             {'r':{'producer_exit':1,'exists':False,'references':[]}}),
            (['v'],{'v':{'producer_exit':0,'exists':False,'roots':['r']}},
             {'r':{'producer_exit':0,'exists':True,'references':[]}}),
            ([],{},{}),
            (['v','v'],{'v':{'producer_exit':0,'exists':False,'roots':[]}},{}),
            (['v'],{'v':{'producer_exit':False,'exists':False,'roots':[]}},{}),
        ]
        for inputs in cases:
            with self.subTest(inputs=inputs):
                self.assertEqual(root_names_exist(*inputs)['verdict'],'UNKNOWN')

    def test_known_violation_survives_another_failed_read(self):
        heads={'v':{'producer_exit':0,'exists':True,'state':'active','roots':['missing','unreadable']}}
        objects={'missing':{'producer_exit':0,'exists':False,'references':[]},
                 'unreadable':{'producer_exit':1,'exists':False,'references':[]}}
        result=root_names_exist(['v'],heads,objects)
        self.assertEqual(result['verdict'],'FAIL')
        self.assertEqual(result['missing'],['missing'])
        self.assertIn('unreadable',result['unknown'])

    def test_other_owner_cannot_satisfy_a_reference(self):
        heads = {'v': {'producer_exit': 0, 'exists': True, 'state': 'active', 'roots': ['owner/pool/h']}}
        objects = {'other/pool/h': {'producer_exit': 0, 'exists': True, 'references': []}}
        result = root_names_exist(['v'], heads, objects)
        self.assertEqual(result['verdict'], 'UNKNOWN')
        self.assertEqual(result['unknown'], ['owner/pool/h'])

    def test_cycle_terminates_without_claiming_flatness(self):
        heads = {'v': {'producer_exit': 0, 'exists': True, 'state': 'active', 'roots': ['r']}}
        objects = {'r': {'producer_exit': 0, 'exists': True, 'references': ['s']},
                   's': {'producer_exit': 0, 'exists': True, 'references': ['r']}}
        self.assertEqual(root_names_exist(['v'], heads, objects), {
            'verdict': 'PASS', 'checked': ['r', 's'], 'missing': [], 'unknown': []})

    def test_absent_head_does_not_create_a_missing_reference(self):
        heads = {'v': {'producer_exit': 0, 'exists': False, 'roots': []}}
        self.assertEqual(root_names_exist(['v'], heads, {})['verdict'], 'PASS')

    def test_definite_absence_survives_duplicate_scope(self):
        heads = {'v': {'producer_exit': 0, 'exists': True, 'state': 'active', 'roots': ['r']}}
        objects = {'r': {'producer_exit': 0, 'exists': False, 'references': []}}
        result = root_names_exist(['v', 'v'], heads, objects)
        self.assertEqual(result['verdict'], 'FAIL')
        self.assertTrue(result['unknown'])

    def test_invalid_object_shape_is_unknown(self):
        heads = {'v': {'producer_exit': 0, 'exists': True, 'state': 'active', 'roots': ['r']}}
        for observation in [None, {}, {'producer_exit': 0, 'exists': True},
                            {'producer_exit': 0, 'exists': True, 'references': ['']},
                            {'producer_exit': 0, 'exists': False, 'references': ['s']}]:
            with self.subTest(observation=observation):
                self.assertEqual(root_names_exist(['v'], heads, {'r': observation})['verdict'],
                                 'UNKNOWN')

    def test_missing_and_failed_read_mutants(self):
        source = (Path(__file__).parent / 'oss_kernel.py').read_text()
        heads = {'v': {'producer_exit': 0, 'exists': True, 'state': 'active', 'roots': ['r']}}
        for anchor, replacement, objects, expected in [
                ("missing.add(name)", 'pass',
                 {'r': {'producer_exit': 0, 'exists': False, 'references': []}}, 'FAIL'),
                ("unknown.add(name)", 'pass', {}, 'UNKNOWN')]:
            with self.subTest(anchor=anchor):
                self.assertEqual(source.count(anchor), 1)
                module = types.ModuleType('kernel_mutant')
                exec(compile(source.replace(anchor, replacement), '<mutant>', 'exec'),
                     module.__dict__)
                self.assertEqual(root_names_exist(['v'], heads, objects)['verdict'], expected)
                self.assertEqual(module.root_names_exist(['v'], heads, objects)['verdict'], 'PASS')

    def test_deciding_function_and_runtime_export_inventory(self):
        sites = []
        entries = {'root_names_exist', 'entries_data_acknowledged'}
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
        self.assertCountEqual(sites, [(file, kind, entry) for entry in entries
                                    for file, kind in [('oss_kernel.py', 'definition'),
                                                       ('loader.py', 'export')]])


if __name__=='__main__': unittest.main()
