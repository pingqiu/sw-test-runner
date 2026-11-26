import contextlib
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import types
import unittest
from unittest.mock import patch
import host

class HostTest(unittest.TestCase):
    def setUp(self):
        p=patch.object(host.os,"getuid",return_value=1000,create=True);p.start();self.addCleanup(p.stop)
    def fixture(self, base):
        root=Path(base)/"swb-kz-20261126t050000z"; root.mkdir()
        c=dict(root=str(root), run="kz-20261126t050000z", role="source", self="192.0.2.1", peer="192.0.2.2",
               rc="a"*40, archive_sha="b"*64, ci_unit="actions.runner.example.service", min_free_bytes="1", unit="swb-kz-20261126t050000z-source")
        return root,c
    def test_prepare_interrupt_at_every_command_then_cleanup(self):
        # Interrupt after a command's effects (including CI stop), then rerun cleanup.
        for phase,cut in [(p,c) for p,n in (("prepare",13),("cleanup",9)) for c in range(1,n)]:
            with self.subTest(phase=phase,cut=cut), tempfile.TemporaryDirectory() as base:
                root,c=self.fixture(base); ci=[True]; calls=[]; count=[0]; current=["prepare"]
                def call(*args,**kw):
                    calls.append(args); out=""
                    if args[0]=="systemctl" and "LoadState" in args: out="loaded" if c["ci_unit"] in args else "not-found"
                    if args[0]=="systemctl" and "ActiveState" in args: out="inactive"
                    if args[:2]==("systemctl","is-active"): out="active" if ci[0] else "inactive"
                    if args[:4]==("sudo","-n","systemctl","stop") and c["ci_unit"] in args: ci[0]=False
                    if args[:4]==("sudo","-n","systemctl","start") and c["ci_unit"] in args: ci[0]=True
                    if args[:4]==("sudo","-n","nvme","list-subsys"): out='{"Subsystems":[]}'
                    if args[:3]==("sudo","-n","losetup"): out='{"loopdevices":[]}'
                    count[0]+=1
                    if current[0]==phase and count[0]==cut: raise RuntimeError("interrupted after command")
                    return out
                def atomic(path,value): path.write_text(json.dumps(value))
                flock=types.SimpleNamespace(flock=lambda *a:None, LOCK_EX=0)
                with patch.object(host,"call",call), patch.object(host,"unit_field",side_effect=lambda u,f: "loaded" if u==c["ci_unit"] else ("not-found" if f=="LoadState" else "inactive")), patch.object(host,"atomic",atomic), patch.object(host,"fcntl",flock), patch.object(host,"nvme_root",return_value="/dev/nvme0n1p1"), patch.object(host,"unpack",return_value={}), patch.object(host.shutil,"which",return_value="/bin/tool"), contextlib.redirect_stdout(io.StringIO()):
                    try: host.prepare(c)
                    except RuntimeError: pass
                    current[0]="cleanup";count[0]=0 if phase=="cleanup" else 100
                    try: host.cleanup(c)
                    except RuntimeError: pass
                    count[0]=100  # retry after the interruption, then repeat for idempotence
                    host.cleanup(c); host.cleanup(c)
                    self.assertTrue(ci[0], calls)
                    if (root/"state.json").exists():
                        self.assertTrue(json.loads((root/"out/cleanup.json").read_text())["clean"])
                        self.assertFalse((root/"tmp").exists())

    def test_cleanup_refuses_a_foreign_unit_before_stopping_anything(self):
        with tempfile.TemporaryDirectory() as base:
            root,c=self.fixture(base);(root/"out").mkdir()
            (root/"state.json").write_text(json.dumps(dict(config=c,ci_was_active=True)))
            flock=types.SimpleNamespace(flock=lambda *a:None, LOCK_EX=0)
            with patch.object(host,"fcntl",flock),patch.object(host,"unit_field",side_effect=lambda u,f:"loaded" if f=="LoadState" else "another root"),patch.object(host,"call") as call:
                with self.assertRaisesRegex(ValueError,"foreign run unit"):host.cleanup(c)
                call.assert_not_called()

    def test_archive_rejects_path_escape_and_symlink(self):
        for name,kind in (("../escape",tarfile.REGTYPE),("blockvolume",tarfile.SYMTYPE)):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as base:
                root=Path(base); archive=root/"bins.tar"
                with tarfile.open(archive,"w") as t:
                    m=tarfile.TarInfo(name);m.type=kind;m.linkname="/etc/passwd" if kind==tarfile.SYMTYPE else ""
                    t.addfile(m,io.BytesIO())
                pin=hashlib.sha256(archive.read_bytes()).hexdigest()
                with self.assertRaises(ValueError):host.unpack(root,pin,"a"*40)
                self.assertFalse((root/"bins").exists())
    def test_running_is_not_a_handoff(self):
        with tempfile.TemporaryDirectory() as base:
            root,c=self.fixture(base);(root/"state.json").write_text(json.dumps(dict(config=c)))
            with patch.dict(host.os.environ,{"KZ_HANDOFF_B64":"RUNNING"}):
                with self.assertRaisesRegex(ValueError,"handoff"):host.accept(c)
    def test_launch_retry_never_starts_a_second_unit(self):
        with tempfile.TemporaryDirectory() as base:
            root,c=self.fixture(base);(root/"state.json").write_text(json.dumps(dict(config=c)))
            with patch.object(host,"call",side_effect=RuntimeError("reply lost")) as call:
                with self.assertRaises(RuntimeError):host.start(c)
                with self.assertRaisesRegex(ValueError,"already launched"):host.start(c)
                self.assertEqual(call.call_count,1)

if __name__=="__main__":unittest.main()
