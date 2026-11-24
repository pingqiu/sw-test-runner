"""Load the exact sealed checker bytes without ambient import fallback."""
import hashlib
import json
from pathlib import Path
import re
import types


def _verified_code(root, manifest_sha256):
    if not re.fullmatch('[0-9a-f]{64}', manifest_sha256):
        raise ValueError('a lowercase manifest SHA256 pin is required')
    root = Path(root)
    raw = (root / 'library.json').read_bytes()
    if hashlib.sha256(raw).hexdigest() != manifest_sha256:
        raise ValueError('checker manifest mismatch')
    manifest = json.loads(raw)
    if manifest.get('schema') != 'checker-library/v1' or set(manifest.get('files', {})) != {'facts.py', 'loader.py', 'resource_inventory.py', 'ops_rmguard.py', 'unit_properties.py', 'completion.py'}:
        raise ValueError('unsupported checker closure')
    code = {}
    for name, pin in manifest['files'].items():
        code[name] = (root / name).read_bytes()
        if hashlib.sha256(code[name]).hexdigest() != pin:
            raise ValueError('checker code mismatch')
    return root, code


def load_rmguard(root, manifest_sha256):
    root, code = _verified_code(root, manifest_sha256)
    operations = types.ModuleType('sealed_rmguard')
    exec(compile(code['ops_rmguard.py'], str(root / 'ops_rmguard.py'), 'exec'), operations.__dict__)
    return operations.run


def load_library(root, manifest_sha256):
    root, code = _verified_code(root, manifest_sha256)
    module = types.ModuleType('sealed_checker_facts')
    exec(compile(code['facts.py'], str(root / 'facts.py'), 'exec'), module.__dict__)
    inventory = types.ModuleType('sealed_resource_inventory')
    exec(compile(code['resource_inventory.py'], str(root / 'resource_inventory.py'), 'exec'), inventory.__dict__)

    def resources_absent_cli(packet_file, arguments):
        return inventory.main(packet_file, arguments, module.resources_absent)

    properties = types.ModuleType('sealed_unit_properties')
    exec(compile(code['unit_properties.py'], str(root / 'unit_properties.py'), 'exec'), properties.__dict__)
    completion = types.ModuleType('sealed_completion')
    exec(compile(code['completion.py'], str(root / 'completion.py'), 'exec'), completion.__dict__)
    module.rep_complete = completion.rep_complete
    module.show_unit_properties = properties.show_unit_properties
    module.resources_absent_cli = resources_absent_cli
    module.library_sha256 = manifest_sha256
    return module
