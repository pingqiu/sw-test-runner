#!/usr/bin/env python3
"""R4 fenced promotion receipts, serving identity and existing ACK readback.

Automatic mode must precede every operator call; operator mode is explicit,
never a fallback after an automatic assertion fails. Reads the master's own
persisted authority record; this checker writes no product state.
"""
import argparse
import base64
import json
import subprocess
import time
import urllib.request
from pathlib import Path


def check(authority, status, *, volume, old_holder, old_epoch, minimum_lsn,
          operator_calls, mode="automatic"):
    if mode not in ("automatic", "operator"):
        raise ValueError("unknown failover mode")
    if operator_calls != (0 if mode == "automatic" else 1):
        raise ValueError("automatic promotion must precede every operator call")
    line = authority["body"]
    proof = line["Primary"]
    epoch = old_epoch + 1
    if line["VolumeID"] != volume or status["VolumeID"] != volume:
        raise ValueError("foreign volume")
    if line["ReplicaID"] == old_holder:
        raise ValueError("old primary cannot be the successor")
    if status["ReplicaID"] != line["ReplicaID"]:
        raise ValueError("status does not belong to the published successor")
    if line["Epoch"] != epoch or status["Epoch"] != epoch:
        raise ValueError("publication and status must both be at e+1")
    if (proof["Kind"] != 2 or proof["FencedEpoch"] != epoch
            or proof["Basis"] != 1 or proof["DurableLSN"] < minimum_lsn):
        raise ValueError("missing fencing-round promotion evidence at e+1")
    if (status["AuthorityRole"] != "primary" or status["Healthy"] is not True
            or status["FrontendPrimaryReady"] is not True):
        raise ValueError("published successor is not serving")
    return {"replica": line["ReplicaID"], "epoch": epoch,
            "durable_lsn": proof["DurableLSN"]}


def token(value):
    return base64.b64encode(json.dumps(value, separators=(",", ":")).encode()).decode()


def decode(text):
    value = json.loads(base64.b64decode(text, validate=True))
    # Revalidate every transported receipt, not only the original observation.
    line = value["authority"]["body"]
    status = dict(VolumeID="v1", ReplicaID=line["ReplicaID"], Epoch=2,
                  AuthorityRole="primary", Healthy=True, FrontendPrimaryReady=True)
    check(value["authority"], status, volume="v1", old_holder="r1", old_epoch=1,
          minimum_lsn=1, operator_calls=value["operator_calls"], mode=value["mode"])
    return value


def wait_line(path, seconds, mode, calls, candidates):
    end = time.monotonic() + seconds
    while True:
        authority = json.loads(path.read_text())
        line = authority["body"]
        if line["VolumeID"] != "v1":
            raise ValueError("foreign authority volume")
        if line["Epoch"] == 1:
            if line["ReplicaID"] != "r1":
                raise ValueError("old epoch has an unexpected holder")
        else:
            value = dict(authority=authority, mode=mode, operator_calls=calls)
            decode(token(value))
            if line["ReplicaID"] not in candidates:
                raise ValueError("successor is not a configured survivor")
            if line["DataAddr"] != candidates[line["ReplicaID"]]:
                raise ValueError("successor endpoint differs from pinned topology")
            return value
        if time.monotonic() >= end:
            raise ValueError("no fenced successor by deadline; no automatic operator fallback")
        time.sleep(min(0.5, max(0, end-time.monotonic())))


def peer_healthy(payload, primary, replica, epoch):
    """The primary's view, not supporting admission on the returning replica."""
    return (payload.get("VolumeID") == "v1" and payload.get("ReplicaID") == primary
            and any(p.get("ReplicaID") == replica and p.get("Epoch") == epoch
                    and p.get("State") == "healthy" for p in payload.get("Peers", [])))


def main():
    p = argparse.ArgumentParser(description=__doc__)
    sub = p.add_subparsers(dest="command", required=True)
    wait = sub.add_parser("wait")
    wait.add_argument("--authority", type=Path, required=True)
    wait.add_argument("--seconds", type=float, required=True)
    wait.add_argument("--mode", choices=("automatic", "operator"), required=True)
    wait.add_argument("--calls", type=Path, required=True)
    wait.add_argument("--candidate", action="append", required=True)
    wait.add_argument("--out", type=Path, required=True)
    field = sub.add_parser("field")
    field.add_argument("token")
    field.add_argument("field", choices=("replica", "host", "epoch"))
    ready = sub.add_parser("status")
    ready.add_argument("token")
    ready.add_argument("replica")
    ready.add_argument("url")
    ready.add_argument("out", type=Path)
    peers = sub.add_parser("peers")
    peers.add_argument("token")
    peers.add_argument("replica") # this process's identity; skip if not selected
    peers.add_argument("url")
    peers.add_argument("out", type=Path)
    verify = sub.add_parser("verify")
    verify.add_argument("token")
    verify.add_argument("helper", type=Path)
    verify.add_argument("device")
    verify.add_argument("ledger", type=Path)
    # Preserve the prepared snapshot-check interface for direct controls.
    cp = sub.add_parser("check")
    cp.add_argument("--authority", type=Path, required=True)
    cp.add_argument("--status", type=Path, required=True)
    cp.add_argument("--volume", required=True)
    cp.add_argument("--old-holder", required=True)
    cp.add_argument("--old-epoch", type=int, required=True)
    cp.add_argument("--minimum-lsn", type=int, required=True)
    cp.add_argument("--operator-calls", type=int, required=True)
    cp.add_argument("--helper", type=Path, required=True)
    cp.add_argument("--device", required=True)
    cp.add_argument("--ledger", type=Path, required=True)
    args = p.parse_args()
    if args.command == "wait":
        if not 0 < args.seconds <= 600:
            p.error("wait must be positive and <=600s")
        candidates = dict(c.split("=", 1) for c in args.candidate)
        if not candidates or not set(candidates) <= {"r2", "r3"}:
            p.error("only configured surviving replicas are allowed")
        value = wait_line(args.authority, args.seconds, args.mode,
                          int(args.calls.read_text()), candidates)
        args.out.write_text(json.dumps(value, indent=2)+"\n")
        print(token(value))
        return
    if args.command in ("field", "status", "peers", "verify"):
        value = decode(args.token)
        line = value["authority"]["body"]
        if args.command == "field":
            print(dict(replica=line["ReplicaID"], epoch=line["Epoch"],
                       host=line["DataAddr"].rsplit(":", 1)[0])[args.field])
            return
        if args.command in ("status", "peers"):
            if args.replica != line["ReplicaID"]:
                print("not the selected successor")
                return
            end = time.monotonic()+(240 if args.command == "peers" else 120)
            while True:
                try:
                    with urllib.request.urlopen(args.url, timeout=5) as response:
                        status = json.load(response)
                    if args.command == "peers":
                        if not peer_healthy(status, line["ReplicaID"], "r1", line["Epoch"]):
                            raise ValueError("primary has not made old r1 healthy at e+1: "+json.dumps(status))
                    else:
                        check(value["authority"], status, volume="v1", old_holder="r1",
                              old_epoch=1, minimum_lsn=1,
                              operator_calls=value["operator_calls"], mode=value["mode"])
                    args.out.write_text(json.dumps(status, indent=2)+"\n")
                    print("old r1 healthy in primary view" if args.command == "peers"
                          else "successor serving with fencing proof")
                    return
                except (OSError, ValueError) as error:
                    if time.monotonic() >= end:
                        raise RuntimeError("successor not serving: "+str(error)) from error
                    time.sleep(.5)
        subprocess.run(["bash", str(args.helper), "verify", args.device,
                        str(args.ledger), "before", "during-loss"], check=True, timeout=240)
        print(json.dumps({"P2": "PASS", "mode": value["mode"],
                          "replica": line["ReplicaID"], "epoch": line["Epoch"],
                          "operator_calls": value["operator_calls"]}))
        return
    if args.old_epoch < 1 or args.minimum_lsn < 1:
        p.error("a positive pre-fault epoch and acknowledged barrier are required")
    result = check(json.loads(args.authority.read_text()),
                   json.loads(args.status.read_text()), volume=args.volume,
                   old_holder=args.old_holder, old_epoch=args.old_epoch,
                   minimum_lsn=args.minimum_lsn, operator_calls=args.operator_calls)
    # The existing verifier enforces every ACK and the existing rule for a
    # later unacknowledged overwrite. Metadata evidence alone never passes P2.
    subprocess.run(["bash", str(args.helper), "verify", args.device,
                    str(args.ledger), "before", "during-loss"],
                   check=True, timeout=240)
    print(json.dumps({"P2": "PASS", "mode": "fenced-automatic", **result}))


if __name__ == "__main__":
    main()
