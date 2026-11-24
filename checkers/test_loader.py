import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import unittest
from loader import load_library


class Loader(unittest.TestCase):
    def test_pin_and_missing_or_modified_members(self):
        with tempfile.TemporaryDirectory() as t:
            root = Path(t)
            source = Path(__file__).parent
            for name in [
                'library.json',
                'facts.py',
                'loader.py',
                'resource_inventory.py',
                'ops_rmguard.py',
                'unit_properties.py',
                'completion.py',
                'power_loss.py',
                'oss_kernel.py',
            ]:
                shutil.copy2(source / name, root / name)
            pin = (source / 'library.sha256').read_text().strip()
            library = load_library(root, pin)
            self.assertEqual(library.library_sha256, pin)
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
            self.assertEqual(library.root_names_exist(['v'], heads, objects)['verdict'], 'FAIL')
            from test_oss_kernel import snapshot_graph

            graph_heads, graph_objects = snapshot_graph()
            self.assertEqual(
                library.root_names_exist(['target'], graph_heads, graph_objects)['verdict'], 'PASS'
            )
            graph_objects['source/pool/shared']['exists'] = False
            self.assertEqual(
                library.root_names_exist(['target'], graph_heads, graph_objects)['verdict'], 'FAIL'
            )
            graph_heads['target']['state'] = 'deleted'
            self.assertEqual(
                library.root_names_exist(['target'], graph_heads, graph_objects)['verdict'], 'PASS'
            )
            from test_entry_order import entry_trace

            self.assertEqual(library.entries_data_acknowledged(*entry_trace())['verdict'], 'PASS')
            from test_removal_order import removal_trace

            self.assertEqual(library.snapshot_removals_safe(*removal_trace())['verdict'], 'FAIL')
            from test_log_ack_order import log_trace

            self.assertEqual(library.acknowledged_writes_synced(*log_trace())['verdict'], 'PASS')
            from test_head_update import head_trace

            self.assertEqual(library.head_updates_conditional(*head_trace())['verdict'], 'PASS')
            self.assertTrue(callable(library.show_unit_properties))
            self.assertTrue(callable(library.rep_complete))
            from test_power_loss import fixture

            self.assertEqual(library.power_loss_image_exact(*fixture())['verdict'], 'PASS')
            from test_power_loss_tags import tag_fixture

            self.assertEqual(library.power_loss_image_exact(*tag_fixture())['verdict'], 'PASS')
            for bad in ['', '0' * 64]:
                with self.subTest(pin=bad), self.assertRaises(ValueError):
                    load_library(root, bad)
            for member in [
                'facts.py',
                'loader.py',
                'resource_inventory.py',
                'ops_rmguard.py',
                'unit_properties.py',
                'completion.py',
                'power_loss.py',
                'oss_kernel.py',
            ]:
                with self.subTest(member=member):
                    original = (root / member).read_bytes()
                    (root / member).write_bytes(b'raise RuntimeError("must never execute")')
                    with self.assertRaisesRegex(ValueError, 'code mismatch'):
                        load_library(root, pin)
                    (root / member).unlink()
                    with self.assertRaises(FileNotFoundError):
                        load_library(root, pin)
                    (root / member).write_bytes(original)
            (root / 'library.json').write_bytes(b'{}')
            with self.assertRaisesRegex(ValueError, 'manifest mismatch'):
                load_library(root, pin)

    def test_published_pin_refuses_a_changed_manifest(self):
        with tempfile.TemporaryDirectory() as t:
            root = Path(t)
            source = Path(__file__).parent
            for name in [
                'library.json',
                'library.sha256',
                'facts.py',
                'loader.py',
                'resource_inventory.py',
                'ops_rmguard.py',
                'unit_properties.py',
                'completion.py',
                'power_loss.py',
                'oss_kernel.py',
            ]:
                shutil.copy2(source / name, root / name)
            published_pin = (root / 'library.sha256').read_text().strip()
            changed = (root / 'library.json').read_bytes() + b'\n'
            (root / 'library.json').write_bytes(changed)
            with self.assertRaisesRegex(ValueError, 'manifest mismatch'):
                load_library(root, published_pin)
            fresh_pin = hashlib.sha256(changed).hexdigest()
            self.assertEqual(load_library(root, fresh_pin).library_sha256, fresh_pin)

    def test_unknown_closure_refused_even_with_its_pin(self):
        with tempfile.TemporaryDirectory() as t:
            root = Path(t)
            raw = json.dumps(
                {'schema': 'checker-library/v1', 'files': {'other.py': '0' * 64}}
            ).encode()
            (root / 'library.json').write_bytes(raw)
            with self.assertRaisesRegex(ValueError, 'unsupported'):
                load_library(root, hashlib.sha256(raw).hexdigest())
