import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "r4_cloud_pack", Path(__file__).resolve().parents[1] / "r4/cloud-pack.py")
pack = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pack)


class R4CloudPack(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.out = self.root / "out"
        self.out.mkdir()
        self.pins = {"product": "a" * 40, "runner": "b" * 40}
        (self.root / "pins.json").write_text(json.dumps(self.pins))
        (self.root / "swblock").write_bytes(b"test-only binary")
        (self.root / "manifest.json").write_text(json.dumps({
            name: hashlib.sha256((self.root / name).read_bytes()).hexdigest()
            for name in ("pins.json", "swblock")}))
        self.hosts = [dict(id=f"node-{i}", resource_id=f"vm-{i}",
                           management=dict(user="swtest", port=22, address=f"192.0.2.{i}"),
                           data=dict(address=f"10.84.1.{10 + i}")) for i in range(1, 4)]
        self.context = dict(schema="seaweed.cloud-pack-context/v1",
                            inventory=str(self.root / "inventory.json"), output=str(self.out),
                            ssh_key="/test/client-key", host_keys={
                                h["id"]: "ssh-ed25519 TEST-PIN-" + h["id"] for h in self.hosts})
        self.calls = []

    def run_pack(self, first_scenario_exit=0):
        (self.root / "inventory.json").write_text(json.dumps(dict(
            target="azure", hosts=self.hosts,
            managed_resource_ids=[h["resource_id"] for h in self.hosts])))
        (self.root / "context.json").write_text(json.dumps(self.context))

        def run(argv, **kwargs):
            self.calls.append(argv)
            rc = first_scenario_exit if argv[0] == str(self.root / "swblock") else 0
            return subprocess.CompletedProcess(argv, rc, b"", b"")

        def git(argv, **kwargs):
            return self.pins[Path(argv[2]).name] + "\n"

        with patch.dict(os.environ, {"SEAWEED_CLOUD_PACK_CONTEXT": str(self.root / "context.json")}), \
                patch.object(pack.subprocess, "run", side_effect=run), \
                patch.object(pack.subprocess, "check_output", side_effect=git):
            return pack.main(self.root)

    def test_inventory_user_and_per_node_pins_reach_both_rounds(self):
        self.assertEqual(self.run_pack(), 0)
        self.assertEqual(len(self.calls), 5)
        for i, argv in enumerate(self.calls[:3], 1):
            self.assertIn(f"swtest@192.0.2.{i}", argv)
            self.assertIn("StrictHostKeyChecking=yes", argv)
            self.assertIn("UserKnownHostsFile=" + str(self.out / f"known_hosts-{i}"), argv)
            self.assertEqual((self.out / f"known_hosts-{i}").read_text(),
                             f"192.0.2.{i} ssh-ed25519 TEST-PIN-node-{i}\n")
        for rf, argv in zip((3, 2), self.calls[3:]):
            self.assertIn(f"rf={rf}", argv)
            self.assertIn("ssh_user=swtest", argv)
            self.assertIn("ssh_key=/test/client-key", argv)
            for i in range(1, 4):
                self.assertIn(f"n{i}_host_key=ssh-ed25519 TEST-PIN-node-{i}", argv)

    def test_other_valid_inventory_user_is_not_replaced(self):
        for h in self.hosts:
            h["management"]["user"] = "run-worker"
        self.assertEqual(self.run_pack(), 0)
        self.assertIn("run-worker@192.0.2.1", self.calls[0])
        self.assertIn("ssh_user=run-worker", self.calls[3])

    def test_invalid_or_different_users_refuse_before_ssh(self):
        for user in ("", "bad user", "root;command", "other-user"):
            with self.subTest(user=user):
                self.hosts[0]["management"]["user"] = user
                with self.assertRaises(AssertionError):
                    self.run_pack()
                self.assertEqual(self.calls, [])

    def test_missing_node_pin_refuses_before_ssh(self):
        del self.context["host_keys"]["node-2"]
        with self.assertRaises(AssertionError):
            self.run_pack()
        self.assertEqual(self.calls, [])

    def test_uniform_invalid_user_refuses_before_ssh(self):
        for h in self.hosts:
            h["management"]["user"] = "bad user"
        with self.assertRaises(AssertionError):
            self.run_pack()
        self.assertEqual(self.calls, [])

    def test_first_failed_round_does_not_start_second(self):
        self.assertEqual(self.run_pack(first_scenario_exit=7), 7)
        self.assertEqual(len(self.calls), 4)
        self.assertIn("rf=3", self.calls[-1])


if __name__ == "__main__":
    unittest.main()
