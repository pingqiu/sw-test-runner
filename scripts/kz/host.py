#!/usr/bin/env python3
"""One-host verbs for the runner's known-zero scenario; no SSH or peer orchestration."""
import base64
try:
    import fcntl
except ImportError:  # Off-host controls may import pure validation on Windows.
    fcntl = None
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import tarfile

BINS = {"kz.test", "blockmaster", "blockvolume", "blockvolume-kzhold", "sw-block", "weed"}

def call(*args, okay=(0,), timeout=60):
    p = subprocess.run(args, text=True, capture_output=True, timeout=timeout)
    if p.returncode not in okay:
        raise RuntimeError(f"{args[0:3]} rc={p.returncode}: {p.stderr.strip()}")
    return p.stdout.strip()

def config():
    c = {k: os.environ["KZ_" + k.upper()] for k in ("root", "run", "role", "self", "peer", "rc", "archive_sha", "ci_unit", "min_free_bytes")}
    root = Path(c["root"])
    if (not re.fullmatch(r"kz-[0-9]{8}t[0-9]{6}z", c["run"])
            or c["role"] not in ("source", "target")
            or not re.fullmatch(r"/[A-Za-z0-9_./-]+", str(root))
            or not root.is_absolute() or root.name != "swb-" + c["run"]
            or root.resolve() != root or root.is_symlink()
            or root.stat().st_uid != os.getuid()):
        raise ValueError("root/run/role identity refused")
    for field in ("self", "peer"): ipaddress.ip_address(c[field])
    if c["self"] == c["peer"] or int(c["min_free_bytes"]) <= 0: raise ValueError("distinct hosts and positive capacity floor required")
    if not re.fullmatch(r"[0-9a-f]{40}", c["rc"]) or not re.fullmatch(r"[0-9a-f]{64}", c["archive_sha"]):
        raise ValueError("full source/archive pins required")
    if c["ci_unit"] != "none" and not re.fullmatch(r"actions\.runner\.[A-Za-z0-9_.@-]+\.service", c["ci_unit"]):
        raise ValueError("exact GitHub runner unit or explicit none required")
    c["unit"] = "swb-" + c["run"] + "-" + c["role"]
    return c

def atomic(path, value):
    tmp = path.with_suffix(".tmp")
    with tmp.open("w") as f:
        json.dump(value, f, sort_keys=True); f.flush(); os.fsync(f.fileno())
    os.replace(tmp, path)
    fd = os.open(path.parent, os.O_DIRECTORY)
    try: os.fsync(fd)
    finally: os.close(fd)

def state(c):
    s = json.loads((Path(c["root"]) / "state.json").read_text())
    if s["config"] != c: raise ValueError("saved launch identity differs")
    return s

def nvme_root(root):
    source = call("findmnt", "-rn", "-o", "SOURCE", "-T", str(root)).split("[")[0]
    rows = call("lsblk", "-snr", "-o", "TYPE,TRAN", "--", source).splitlines()
    disks = [r.split() for r in rows if r.split()[0] == "disk"]
    if not disks or any(r != ["disk", "nvme"] for r in disks):
        raise ValueError("run root is not proven NVMe-backed")
    return source

def unpack(root, pin, rc):
    archive = root / "bins.tar"
    if hashlib.sha256(archive.read_bytes()).hexdigest() != pin: raise ValueError("archive pin mismatch")
    with tarfile.open(archive) as t:
        members = t.getmembers()
        if {m.name for m in members} != BINS | {"BUILD.json"} or len(members) != 7 or any(not m.isfile() for m in members):
            raise ValueError("archive must contain exactly six regular binaries and BUILD.json")
        dest = root / "bins"; dest.mkdir()
        for m in members:
            with t.extractfile(m) as src, (dest / m.name).open("xb") as out: shutil.copyfileobj(src, out)
    manifest = json.loads((dest / "BUILD.json").read_text())
    if manifest["source_sha"] != rc or set(manifest["files"]) != BINS: raise ValueError("build source/file census differs")
    for name in BINS:
        p = dest / name
        if hashlib.sha256(p.read_bytes()).hexdigest() != manifest["files"][name]: raise ValueError("binary pin mismatch: " + name)
        p.chmod(0o755)
    return manifest

def unit_field(unit, field):
    p = subprocess.run(["systemctl", "show", unit, "-p", field, "--value"], text=True, capture_output=True, timeout=30)
    value = p.stdout.strip()
    if p.returncode and not (field == "LoadState" and value == "not-found"):
        raise RuntimeError("unit state unreadable: " + p.stderr.strip())
    return value

def idle():
    for name in ("Runner.Worker", "go", "compile", "link"):
        if call("pgrep", "-x", name, okay=(0, 1)): raise ValueError("host busy: " + name)

def description(c):
    return "known-zero " + c["root"] + " " + c["rc"]

def prepare(c):
    root = Path(c["root"])
    if (root / "state.json").exists(): raise ValueError("run id already prepared; no reuse")
    backing = nvme_root(root)
    if shutil.disk_usage(root).free < int(c["min_free_bytes"]): raise ValueError("below declared capacity floor")
    for name in ("sudo", "systemd-run", "nvme", "mkfs.ext4", "mkfs.xfs", "filefrag", "findmnt", "losetup"):
        if not shutil.which(name): raise ValueError("missing tool: " + name)
    call("sudo", "-n", "true")
    manifest = unpack(root, c["archive_sha"], c["rc"])
    for name in ("out", "tmp", "x"): (root / name).mkdir()
    idle()
    active = False
    if c["ci_unit"] == "none":
        if call("pgrep", "-x", "Runner.Listener", okay=(0, 1)): raise ValueError("runner listener exists but no unit declared")
    else:
        load = unit_field(c["ci_unit"], "LoadState")
        if load != "loaded": raise ValueError("declared CI unit missing")
        active = call("systemctl", "is-active", c["ci_unit"], okay=(0, 3)) == "active"
    s = dict(config=c, ci_was_active=active, backing=backing, build=manifest)
    atomic(root / "state.json", s)  # durable before stopping CI or starting any unit
    atomic(root / "out/launch.json", s)
    # Host-side fallback uses the same cleanup verb if the controller disappears.
    env = ["KZ_"+k.upper()+"="+v for k,v in c.items() if k != "unit"]
    call("sudo", "-n", "systemd-run", "--unit="+c["unit"]+"-cleanup", "--on-active=275m", "--timer-property=AccuracySec=1s",
         "--timer-property=Description="+description(c)+" cleanup timer",
         "--property=User="+str(os.getuid()), "--property=RuntimeMaxSec=300", "--property=KillMode=control-group", "--collect",
         "/usr/bin/env", *env, sys.executable, str(root / "host.py"), "cleanup")
    if active: call("sudo", "-n", "systemctl", "stop", c["ci_unit"])
    idle()
    print("PREPARED")

def start(c):
    state(c)
    root = Path(c["root"])
    if (root / "started").exists(): raise ValueError("role already launched")
    (root / "started").touch(exist_ok=False)
    env = ["KZ_"+k.upper()+"="+v for k,v in c.items() if k != "unit"]
    limit = "235m" if c["role"] == "source" else "100m"
    call("sudo", "-n", "systemd-run", "--unit="+c["unit"], "--description="+description(c), "--property=User="+str(os.getuid()),
         "--property=RuntimeMaxSec="+limit, "--property=TimeoutStopSec=30", "--property=KillMode=control-group", "--collect",
         "/usr/bin/env", *env, sys.executable, str(root / "host.py"), "run")
    print("STARTED")

def run(c):
    state(c)
    root = Path(c["root"])
    e = os.environ.copy()
    e.update(TMPDIR=str(root / "tmp"), SWBLOCK_TEST_BINS=str(root / "bins"), CHUNKSTORE_WEED=str(root / "bins/weed"),
             KZ_RUN="1", KZ_RUN_ID=c["run"], KZ_SIZE="1T", KZ_DEVICE="1", KZ_FS="ext4,xfs", KZ_ROLE=c["role"],
             KZ_HANDOFF=str(root / "x/handoff.json"), KZ_REPORT=str(root / "out/report.json"),
             SWBLOCK_TEST_LISTEN_HOST=c["self"], KZ_TARGET_HOST=c["peer"], KZ_TARGET_DONE=str(root / "x/target.done"),
             KZ_TARGET_WAIT="105m", KZ_HOLD_BIN=str(root / "bins/blockvolume-kzhold"))
    rc = 125
    try:
        with (root / "out/test.log").open("xb") as log:
            rc = subprocess.run([str(root / "bins/kz.test"), "-test.run=^TestKnownZeroAcceptance$", "-test.v",
                                 "-test.timeout="+("230m" if c["role"] == "source" else "95m")],
                                cwd=root, env=e, stdout=log, stderr=subprocess.STDOUT).returncode
    finally:
        atomic(root / "out/exit.json", dict(rc=rc, run=c["run"], role=c["role"]))
    return rc

def poll(c, handoff=False):
    state(c); root = Path(c["root"])
    if handoff and (root / "x/handoff.json").is_file():
        h = json.loads((root / "x/handoff.json").read_text())
        if h["run"] != c["run"] or h["bytes"] != 1 << 40 or len(h["targets"]) != 4: raise ValueError("foreign/incomplete handoff")
        print("HANDOFF " + base64.b64encode((root / "x/handoff.json").read_bytes()).decode()); return
    if (root / "out/exit.json").exists():
        e = json.loads((root / "out/exit.json").read_text())
        print("DONE rc="+str(e["rc"])); return
    load = unit_field(c["unit"], "LoadState")
    active = "inactive" if load == "not-found" else unit_field(c["unit"], "ActiveState")
    if active in ("inactive", "failed", ""):
        ended = root / "out/ended-without-receipt.json"
        if not ended.exists(): atomic(ended, dict(first_seen=time.time()))
        if time.time() - json.loads(ended.read_text())["first_seen"] >= 30:
            print("DONE rc=125"); return
    print("RUNNING")

def accept(c):
    state(c); root = Path(c["root"])
    text = os.environ["KZ_HANDOFF_B64"]
    if not text.startswith("HANDOFF "): raise ValueError("not a handoff receipt")
    raw = base64.b64decode(text[len("HANDOFF "):], validate=True)
    h = json.loads(raw)
    if h["run"] != c["run"] or h["bytes"] != 1 << 40 or len(h["targets"]) != 4: raise ValueError("handoff mismatch")
    with (root / "x/handoff.json").open("xb") as f: f.write(raw)

def cleanup(c):
    root = Path(c["root"])
    if not (root / "state.json").exists():
        print("UNPREPARED: no service or role was started"); return
    s = state(c)
    with (root / "cleanup.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        # Stopping a nonexistent/collected unit is idempotent, but other failures are not success.
        load = unit_field(c["unit"], "LoadState")
        if load != "not-found":
            if unit_field(c["unit"], "Description") != description(c): raise ValueError("foreign run unit; no stop")
            call("sudo", "-n", "systemctl", "stop", c["unit"])
        mounts = call("findmnt", "-rn", "-o", "TARGET").splitlines()
        for m in sorted((m for m in mounts if m.startswith(str(root)+"/")), key=len, reverse=True): call("sudo", "-n", "umount", "--", m)
        prefix = "nqn.2026-11.io.seaweedfs:kz-"+c["run"]+"-"
        def nqns(value):
            if isinstance(value, dict):
                for k,v in value.items():
                    if k in ("NQN", "SubsystemNQN") and isinstance(v,str) and v.startswith(prefix): yield v
                    else: yield from nqns(v)
            elif isinstance(value,list):
                for v in value: yield from nqns(v)
        for nqn in set(nqns(json.loads(call("sudo", "-n", "nvme", "list-subsys", "-o", "json")))):
            call("sudo", "-n", "nvme", "disconnect", "-n", nqn)
        if any(m.startswith(str(root)+"/") for m in call("findmnt", "-rn", "-o", "TARGET").splitlines()): raise ValueError("mount remains")
        if set(nqns(json.loads(call("sudo", "-n", "nvme", "list-subsys", "-o", "json")))): raise ValueError("NVMe connection remains")
        load = unit_field(c["unit"], "LoadState")
        active = "inactive" if load == "not-found" else unit_field(c["unit"], "ActiveState")
        if active not in ("inactive", "failed", ""): raise ValueError("run unit not stopped")
        loops = json.loads(call("sudo", "-n", "losetup", "--json", "--output", "NAME,BACK-FILE"))
        if any(str(d.get("back-file", "")).startswith(str(root)+"/") for d in loops.get("loopdevices", [])):
            raise ValueError("run-owned loop device remains")
        scratch = root / "tmp"
        if scratch.exists():
            if scratch.resolve() != scratch or scratch.is_symlink(): raise ValueError("scratch symlink refused")
            shutil.rmtree(scratch)  # exact owned root, only after absence proof
        if s["ci_was_active"]: call("sudo", "-n", "systemctl", "start", c["ci_unit"])
        # Keep out/, state and binaries as evidence until controller collection succeeds.
        timer = c["unit"]+"-cleanup.timer"
        if unit_field(timer, "LoadState") != "not-found":
            if unit_field(timer, "Description") != description(c)+" cleanup timer": raise ValueError("foreign cleanup timer; no stop")
            call("sudo", "-n", "systemctl", "stop", timer)
        atomic(root / "out/cleanup.json", dict(run=c["run"], role=c["role"], clean=True, ci_restored=s["ci_was_active"]))
        print("CLEAN")

def main():
    if fcntl is None: raise ValueError("host verbs require Linux")
    if len(sys.argv) == 3:
        saved = json.loads((Path(sys.argv[2]) / "state.json").read_text())["config"]
        if saved["root"] != sys.argv[2]: raise ValueError("saved root differs")
        os.environ.update({"KZ_"+k.upper(): v for k,v in saved.items() if k != "unit"})
    c = config(); verb = sys.argv[1]
    if verb == "prepare": prepare(c)
    elif verb == "start": start(c)
    elif verb == "run": return run(c)
    elif verb == "handoff": poll(c, True)
    elif verb == "poll": poll(c)
    elif verb == "accept": accept(c)
    elif verb == "done": state(c); (Path(c["root"])/"x/target.done").touch(exist_ok=True)
    elif verb == "cleanup": cleanup(c)
    else: raise ValueError("unknown verb")
    return 0

if __name__ == "__main__":
    try: raise SystemExit(main())
    except Exception as e: print(str(e), file=sys.stderr); raise SystemExit(2)
