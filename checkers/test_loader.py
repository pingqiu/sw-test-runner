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
            for name in ['library.json', 'facts.py', 'loader.py']:
                shutil.copy2(source / name, root / name)
            pin = (source / 'library.sha256').read_text().strip()
            self.assertEqual(load_library(root, pin).library_sha256, pin)
            for bad in ['', '0' * 64]:
                with self.subTest(pin=bad), self.assertRaises(ValueError):
                    load_library(root, bad)
            (root / 'facts.py').write_bytes(b'raise RuntimeError("must never execute")')
            with self.assertRaisesRegex(ValueError, 'code mismatch'):
                load_library(root, pin)
            (root / 'facts.py').unlink()
            with self.assertRaises(FileNotFoundError):
                load_library(root, pin)
            (root / 'library.json').write_bytes(b'{}')
            with self.assertRaisesRegex(ValueError, 'manifest mismatch'):
                load_library(root, pin)

    def test_published_pin_refuses_a_changed_manifest(self):
        with tempfile.TemporaryDirectory() as t:
            root = Path(t)
            source = Path(__file__).parent
            for name in ['library.json', 'library.sha256', 'facts.py', 'loader.py']:
                shutil.copy2(source / name, root / name)
            published_pin = (root / 'library.sha256').read_text().strip()
            changed = (root / 'library.json').read_bytes() + b'\n'
            (root / 'library.json').write_bytes(changed)
            with self.assertRaisesRegex(ValueError, 'manifest mismatch'):
                load_library(root, published_pin)
            # The closure is still valid; only the unchanged published pin is stale.
            fresh_pin = hashlib.sha256(changed).hexdigest()
            self.assertEqual(load_library(root, fresh_pin).library_sha256, fresh_pin)

    def test_unknown_closure_refused_even_with_its_pin(self):
        with tempfile.TemporaryDirectory() as t:
            root = Path(t)
            raw = json.dumps({'schema': 'checker-library/v1', 'files': {'other.py': '0' * 64}}).encode()
            (root / 'library.json').write_bytes(raw)
            with self.assertRaisesRegex(ValueError, 'unsupported'):
                load_library(root, hashlib.sha256(raw).hexdigest())
