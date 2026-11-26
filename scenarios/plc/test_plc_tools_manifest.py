"""The executed tool set is one set in four places: plc-tools.sha256 (with each tool's current hash), plc_seal.py's
REQUIRED_TOOLS, plc_seal_check.sh's REQUIRED_TOOLS, and the tools the scenario YAML actually calls. The sealed
packet pins the manifest's sha256, so a tool edited without regenerating it fails here, in CI, rather than at
the next seal check; a tool the YAML starts calling without being pinned fails here too."""
import hashlib
import re
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
YAML = HERE.parent/'plc-powerloss-counted.yaml'
sys.dont_write_bytecode = True


class ToolsManifest(unittest.TestCase):
    def manifest(self):
        rows = {}
        for line in (HERE/'plc-tools.sha256').read_bytes().decode('ascii').splitlines():
            digest, name = line.split('  ', 1)
            rows[name] = digest
        return rows

    def test_manifest_hashes_are_current(self):
        for name, digest in self.manifest().items():
            self.assertEqual(hashlib.sha256((HERE/name).read_bytes()).hexdigest(), digest, '%s changed: regenerate plc-tools.sha256' % name)

    def test_one_executed_set_everywhere(self):
        manifest = set(self.manifest())
        seal_py = re.search(r'REQUIRED_TOOLS = \{([^}]*)\}', (HERE/'plc_seal.py').read_text(encoding='utf-8')).group(1)
        seal_py = set(re.findall(r"'([^']+)'", seal_py))
        check_sh = set(re.search(r"^REQUIRED_TOOLS='([^']*)'", (HERE/'plc_seal_check.sh').read_text(encoding='utf-8'), re.M).group(1).split())
        called = set(re.findall(r'\$PLC_TOOLS/([A-Za-z0-9_.]+)', YAML.read_text(encoding='utf-8')))
        self.assertEqual(seal_py, manifest)
        self.assertEqual(check_sh, manifest)
        self.assertEqual(called, manifest)


if __name__ == '__main__':
    unittest.main()
