"""Load the exact sealed checker bytes without ambient import fallback."""
import hashlib
import json
from pathlib import Path
import re
import types


def load_library(root, manifest_sha256):
    if not re.fullmatch('[0-9a-f]{64}', manifest_sha256):
        raise ValueError('a lowercase manifest SHA256 pin is required')
    root = Path(root)
    raw = (root / 'library.json').read_bytes()
    if hashlib.sha256(raw).hexdigest() != manifest_sha256:
        raise ValueError('checker manifest mismatch')
    manifest = json.loads(raw)
    if manifest.get('schema') != 'checker-library/v1' or set(manifest.get('files', {})) != {'facts.py', 'loader.py'}:
        raise ValueError('unsupported checker closure')
    code = {}
    for name, pin in manifest['files'].items():
        code[name] = (root / name).read_bytes()
        if hashlib.sha256(code[name]).hexdigest() != pin:
            raise ValueError('checker code mismatch')
    module = types.ModuleType('sealed_checker_facts')
    exec(compile(code['facts.py'], str(root / 'facts.py'), 'exec'), module.__dict__)
    module.library_sha256 = manifest_sha256
    return module
