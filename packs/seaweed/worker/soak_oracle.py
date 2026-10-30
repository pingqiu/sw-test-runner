"""Pure correctness oracle for the reviewed-before-run rotation soak. No I/O."""
from dataclasses import dataclass
from hashlib import sha256
import math

class OracleError(AssertionError):
    pass

@dataclass(frozen=True)
class Version:
    sequence: int
    length: int
    digest: str
    fids: tuple[str, ...]

class Oracle:
    def __init__(self):
        self.latest = {}
        self.pending = {}
        self.reading = {}
        self.retired = set()
        self.sequence = 0
        self.acks = []

    def begin_write(self, key, payload):
        if key in self.pending or key in self.reading:
            raise OracleError("overlapping same-key operation")
        self.sequence += 1
        intent = Version(self.sequence, len(payload), sha256(payload).hexdigest(), ())
        self.pending[key] = intent
        return intent

    def acknowledge(self, key, sequence, length, fids):
        intent = self.pending.get(key)
        if intent is None or sequence != intent.sequence or length != intent.length:
            raise OracleError("ACK does not match planned write")
        fids = tuple(fids)
        if not fids or any(not f or ',' not in f for f in fids):
            raise OracleError("ACK lacks independently resolved FIDs")
        version = Version(sequence, length, intent.digest, fids)
        self.latest[key] = version
        del self.pending[key]
        self.acks.append((key, version))
        return version

    def begin_read(self, key):
        if key in self.pending or key in self.reading:
            raise OracleError("ambiguous read during write or existing read")
        if key not in self.latest:
            raise OracleError("unacknowledged key")
        self.reading[key] = self.latest[key]
        return self.reading[key]

    def finish_read(self, key, status, payload):
        version = self.reading.pop(key, None)
        if version is None:
            raise OracleError("read lacks its per-key guard")
        if status == 200:
            if len(payload) != version.length or sha256(payload).hexdigest() != version.digest:
                raise OracleError("WRONG_BYTES: latest ACK hash/length differs")
            return "hit"
        if status == 404:
            vids = {fid.split(',', 1)[0] for fid in version.fids}
            if not vids or not vids.issubset(self.retired):
                raise OracleError("unexpected miss for latest ACK version")
            return "retired_miss"
        raise OracleError("non-success needs explicit fault attribution")

    def retirement(self, vid, master_success, topology_absent, dat_absent):
        if not vid or not (master_success and topology_absent and dat_absent):
            raise OracleError("retirement is not independently confirmed")
        self.retired.add(str(vid))

    def abandon_write(self, key):
        if key not in self.pending:
            raise OracleError("no submitted mutation")
        # Do not remove pending: another operation could mistake an unknown
        # write outcome for the old acknowledged version. The run must stop.
        raise OracleError("UNKNOWN_WRITE: stop; no automatic retry or invented ACK")

    def record(self):
        return {"acknowledged": len(self.acks), "retired_volumes": len(self.retired),
                "pending_mutations": len(self.pending), "active_reads": len(self.reading)}

@dataclass(frozen=True)
class FaultWindow:
    start: float
    end: float
    operation_ids: frozenset[str]

    def accepts(self, operation_id, kind, operation, submitted, completed):
        return (operation == "read" and kind in {"timeout", "connection_reset", "unavailable"}
                and operation_id in self.operation_ids and submitted <= self.start
                and self.start <= completed <= self.end and self.end-self.start <= 60)

REQUIRED = {"rss_bytes", "fds", "VmPin_bytes", "VmLck_bytes", "disk_bytes", "volume_count",
            "SeaweedFS_rdma_range_pinned_bytes", "SeaweedFS_rdma_range_retained_bytes",
            "SeaweedFS_rdma_range_retained_sources"}

def require_sample(sample):
    missing = REQUIRED - sample.keys()
    if missing or any(sample[k] is None for k in REQUIRED):
        raise OracleError("NOT_OBSERVED resource series: " + ','.join(sorted(missing)))
    if any(type(sample[k]) not in (int, float) or not math.isfinite(sample[k]) or sample[k] < 0 for k in REQUIRED):
        raise OracleError("invalid resource observation")
    return sample

def rotation_math(rate_mib_s=4, seconds=1800, slots=8, volume_mib=32):
    if min(rate_mib_s, seconds, slots, volume_mib) <= 0:
        raise ValueError("positive workload/capacity required")
    written = rate_mib_s * seconds
    return {"written_mib": written, "capacity_mib": slots*volume_mib,
            "fleet_turns": written/(slots*volume_mib),
            "full_volume_equivalents": written/volume_mib}
