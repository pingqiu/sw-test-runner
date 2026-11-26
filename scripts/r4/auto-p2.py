#!/usr/bin/env python3
"""Unpublished option A: check a fenced automatic promotion and ACK readback.

This is not called by the released scenario. The owner must select option A
before replacing its held assertion. Receipts must be freshly collected after
the fault, before ANY operator failover; this checker does not mint a line.
"""
import argparse
import json
import subprocess
from pathlib import Path


def check(authority, status, *, volume, old_holder, old_epoch, minimum_lsn,
          operator_calls):
    if operator_calls != 0:
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


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--authority", type=Path, required=True)
    p.add_argument("--status", type=Path, required=True)
    p.add_argument("--volume", required=True)
    p.add_argument("--old-holder", required=True)
    p.add_argument("--old-epoch", type=int, required=True)
    p.add_argument("--minimum-lsn", type=int, required=True)
    p.add_argument("--operator-calls", type=int, required=True)
    p.add_argument("--helper", type=Path, required=True)
    p.add_argument("--device", required=True)
    p.add_argument("--ledger", type=Path, required=True)
    args = p.parse_args()
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
