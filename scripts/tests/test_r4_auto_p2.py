import copy
import importlib.util
from pathlib import Path
import unittest
import contextlib
import io
import json
import subprocess
import tempfile
from unittest import mock

spec = importlib.util.spec_from_file_location(
    "auto_p2", Path(__file__).resolve().parents[1] / "r4" / "auto-p2.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class AutoP2(unittest.TestCase):
    def setUp(self):
        self.line = {"body": {"VolumeID": "v1", "ReplicaID": "r2", "Epoch": 2,
                     "DataAddr": "10.84.1.11:19601",
                     "Primary": {"Kind": 2, "FencedEpoch": 2, "Basis": 1,
                                 "DurableLSN": 70}}}
        self.status = {"VolumeID": "v1", "ReplicaID": "r2", "Epoch": 2,
                       "AuthorityRole": "primary", "Healthy": True,
                       "FrontendPrimaryReady": True}
        self.kw = dict(volume="v1", old_holder="r1", old_epoch=1,
                       minimum_lsn=70, operator_calls=0)

    def test_fenced_successor(self):
        self.assertEqual(module.check(self.line, self.status, **self.kw)["replica"], "r2")

    def test_each_missing_proof_refuses(self):
        for field, value in [("Kind", 1), ("FencedEpoch", 1), ("Basis", 2),
                             ("DurableLSN", 69)]:
            with self.subTest(field=field):
                bad = copy.deepcopy(self.line)
                bad["body"]["Primary"][field] = value
                with self.assertRaises(ValueError):
                    module.check(bad, self.status, **self.kw)

    def test_status_identity_and_readiness_refuse(self):
        for field, value in [("VolumeID", "other"), ("ReplicaID", "r3"),
                             ("Epoch", 1), ("AuthorityRole", "replica"),
                             ("Healthy", False), ("FrontendPrimaryReady", False)]:
            with self.subTest(field=field):
                bad = dict(self.status, **{field: value})
                with self.assertRaises(ValueError):
                    module.check(self.line, bad, **self.kw)

    def test_operator_publication_is_not_automatic(self):
        with self.assertRaises(ValueError):
            module.check(self.line, self.status, **dict(self.kw, operator_calls=1))

    def test_old_holder_refuses_even_at_new_epoch(self):
        line = copy.deepcopy(self.line)
        line["body"]["ReplicaID"] = "r1"
        with self.assertRaises(ValueError):
            module.check(line, dict(self.status, ReplicaID="r1"), **self.kw)

    def invoke(self, failure=None):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            authority, status = root / "authority.json", root / "status.json"
            authority.write_text(json.dumps(self.line))
            status.write_text(json.dumps(self.status))
            argv = ["auto-p2.py", "check", "--authority", str(authority), "--status", str(status),
                    "--volume", "v1", "--old-holder", "r1", "--old-epoch", "1",
                    "--minimum-lsn", "70", "--operator-calls", "0", "--helper",
                    "r4.sh", "--device", "owned-device", "--ledger", "ledger"]
            output = io.StringIO()
            with mock.patch("sys.argv", argv), contextlib.redirect_stdout(output):
                with mock.patch.object(module.subprocess, "run", side_effect=failure) as run:
                    module.main()
                    run.assert_called_once_with(
                        ["bash", "r4.sh", "verify", "owned-device", "ledger",
                         "before", "during-loss"], check=True, timeout=240)
            return output.getvalue()

    def test_ack_readback_is_required_before_pass(self):
        self.assertEqual(json.loads(self.invoke())["P2"], "PASS")

    def test_lost_ack_fails_instead_of_metadata_only_pass(self):
        with self.assertRaises(subprocess.CalledProcessError):
            self.invoke(subprocess.CalledProcessError(1, "verify"))

    def test_observer_follows_either_survivor(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / "authority.json"
            for replica, address in [("r2", "10.84.1.11:19601"), ("r3", "10.84.1.13:19601")]:
                line = copy.deepcopy(self.line)
                line["body"].update(ReplicaID=replica, DataAddr=address)
                path.write_text(json.dumps(line))
                value = module.wait_line(path, 1, "automatic", 0, {replica: address})
                self.assertEqual(module.decode(module.token(value))["authority"]["body"]["ReplicaID"], replica)

    def test_no_line_never_invokes_operator(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / "authority.json"
            line = copy.deepcopy(self.line)
            line["body"].update(Epoch=1, ReplicaID="r1")
            path.write_text(json.dumps(line))
            with mock.patch.object(module.subprocess, "run") as run:
                with self.assertRaisesRegex(ValueError, "no automatic operator fallback"):
                    module.wait_line(path, 0, "automatic", 0, {"r2": "10.84.1.11:19601"})
                run.assert_not_called()

    def test_foreign_endpoint_and_unfenced_line_refuse(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / "authority.json"
            path.write_text(json.dumps(self.line))
            with self.assertRaisesRegex(ValueError, "endpoint"):
                module.wait_line(path, 1, "automatic", 0, {"r2": "10.84.1.99:19601"})
            bad = copy.deepcopy(self.line)
            bad["body"]["Primary"]["FencedEpoch"] = 1
            path.write_text(json.dumps(bad))
            with self.assertRaisesRegex(ValueError, "fencing"):
                module.wait_line(path, 1, "automatic", 0, {"r2": "10.84.1.11:19601"})

    def test_operator_mode_must_be_explicit_and_counted(self):
        module.check(self.line, self.status, **dict(self.kw, operator_calls=1), mode="operator")
        with self.assertRaises(ValueError):
            module.check(self.line, self.status, **self.kw, mode="operator")

    def test_scenario_packages_checker_and_guards_operator_command(self):
        source = (Path(__file__).resolve().parents[2] / "scenarios/v3block/r4-node-loss.yaml").read_text()
        self.assertIn('failover_mode: "automatic"', source)
        self.assertIn('git -C "$RS" show "$RHEAD:scripts/r4/auto-p2.py"', source)
        self.assertIn("[ '{{ failover_mode }}' = operator ] || exit 0\n", source)
        self.assertIn('auto-p2.py verify "$PROMOTION"', source)
        self.assertIn('auto-p2.py field "$PROMOTION" host', source)


if __name__ == "__main__":
    unittest.main()
