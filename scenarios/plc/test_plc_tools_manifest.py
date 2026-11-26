"""plc-tools.sha256 names exactly the controller-side tools a power-loss run executes, each with its current hash.
The sealed packet pins this manifest's sha256, so a tool edited without regenerating it fails here, in CI,
rather than at the next seal check."""
import hashlib
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
EXECUTED = {'plc_seal_check.sh', 'plc_judge.py', 'plc_gate.sh', 'plc_settle.sh'}


class ToolsManifest(unittest.TestCase):
    def test_manifest_names_exactly_the_executed_tools_with_their_hashes(self):
        rows = {}
        for line in (HERE/'plc-tools.sha256').read_bytes().decode('ascii').splitlines():
            digest, name = line.split('  ', 1)
            rows[name] = digest
        self.assertEqual(set(rows), EXECUTED)
        for name, digest in rows.items():
            self.assertEqual(hashlib.sha256((HERE/name).read_bytes()).hexdigest(), digest, '%s changed: regenerate plc-tools.sha256' % name)


if __name__ == '__main__':
    unittest.main()
