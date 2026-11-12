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

    def matches(self, length, digest):
        """The one GET hash/length check (live worker, assessor, finish_read)."""
        return length == self.length and digest == self.digest

class Oracle:
    def __init__(self):
        self.latest = {}
        self.pending = {}
        self.reading = {}
        self.readers = {}  # concurrent GETs per key in `reading`: a hot key is read by many clients at once
        self.retired = set()
        self.sequence = 0
        self.acks = []
        self.quarantined = {}  # stage 3 F1/F2: key -> (survivor or None, the attributed UNKNOWN intent)

    def begin_write(self, key, payload):
        if key in self.quarantined:
            raise OracleError("UNKNOWN_WRITE retry on a quarantined key")
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
        """Reads of one key may overlap: none can begin during a write, and begin_write refuses while
        any is in flight, so every concurrent reader expects the same latest version."""
        if key in self.pending:
            raise OracleError("ambiguous read during write")
        if key in self.quarantined:
            raise OracleError("read of a quarantined key outside the audit")
        if key not in self.latest:
            raise OracleError("unacknowledged key")
        if key in self.reading:
            self.readers[key] += 1
        else:
            self.reading[key], self.readers[key] = self.latest[key], 1
        return self.reading[key]

    def end_read(self, key):
        """Release one in-flight read of key: the version it expects."""
        version = self.reading.get(key)
        if version is None:
            raise OracleError("read lacks its per-key guard")
        self.readers[key] -= 1
        if not self.readers[key]:
            del self.reading[key], self.readers[key]
        return version

    def finish_read(self, key, status, payload):
        version = self.end_read(key)
        if status == 200:
            if not version.matches(len(payload), sha256(payload).hexdigest()):
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

    def refuse_write(self, key):
        """REFUSED: a named definite error before any mutation. The key's survivor stands unchanged."""
        if key not in self.pending:
            raise OracleError("no submitted mutation")
        del self.pending[key]

    def quarantine_write(self, key):
        """The only exception to abandon_write (stage 3, F1/F2): an UNKNOWN attributed to the row's killed
        identity. The key leaves the load for good (no read, no retry) and is never ACKed; its survivor and the
        UNKNOWN intent are kept for the mandatory audit."""
        intent = self.pending.pop(key, None)
        if intent is None:
            raise OracleError("no submitted mutation")
        self.quarantined[key] = (self.latest.pop(key, None), intent)
        return self.quarantined[key]

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

OP_LIMIT_S = 30  # the existing per-operation limit (soak_native_worker.finish)

@dataclass(frozen=True)
class PutFaultWindow:
    """The named PUT extension of FaultWindow (stage 3, F1 and F2 only). An UNKNOWN PUT is attributable only
    when the row's killed identity (pid, starttime) is its worker (F2) or its target (F1), it was submitted no
    later than the window's end, and it ended inside [start, end + OP_LIMIT_S]."""
    start: float
    end: float
    killed: tuple

    def attributes(self, identity, submitted, completed):
        return (tuple(identity) == tuple(self.killed) and submitted <= self.end
                and self.start <= completed <= self.end + OP_LIMIT_S)


LINK_FLAP_BINDING = ('run_id', 'boot_id', 'episode', 'kind', 'netdev', 'rdma_device')


@dataclass(frozen=True)
class LinkFlapWindow:
    """F3 (ruling 2026-11-11 01:54Z): an UNKNOWN PUT is the link flap's when it was in flight at link-down (submitted
    before start, ended at or after it) or was submitted in [start, end]. One clock only, M01 CLOCK_MONOTONIC, where
    the load and its ledger live: start is the controller's fault-pause pause_mono (published before the apply is
    issued), end is its fault-close end_mono (published only after the restore returned restored=true) plus the row's
    recovery bound. M02 clocks never enter. The load decides with end = inf while the window is still open (a
    submission before an unpublished close is necessarily before it); the assessor re-derives both bounds."""
    start: float
    end: float
    binding: tuple  # (field, value) pairs from LINK_FLAP_BINDING: run, M01 boot, episode, kind, target VF

    def contains(self, submitted, completed):
        return completed >= self.start and submitted <= self.end

    def reason(self, submitted, completed):
        """The ledger receipt of one attributed UNKNOWN."""
        return dict(dict(self.binding), start=self.start, end=None if self.end == math.inf else self.end,
                    start_mark='fault-pause pause_mono', end_mark='fault-close end_mono + recovery_s',
                    case='in flight at link-down' if submitted < self.start else 'submitted in the down window')

    def attributes(self, reason, submitted, completed):
        """The assessor's check of a receipt: bound to this row, recorded against these exact marks, and inside."""
        return (isinstance(reason, dict) and all(reason.get(k) == v for k, v in self.binding)
                and reason.get('start') == self.start and reason.get('end') in (None, self.end)
                and self.contains(submitted, completed))

PRESSURE_BINDING = ('run_id', 'boot_id', 'episode', 'kind')


@dataclass(frozen=True)
class PressureWindow(LinkFlapWindow):
    """F5 (r26, plan v6): the memlock-pressure window, on the same M01 marks as LinkFlapWindow (fault-pause pause_mono to
    fault-close end_mono + the row's recovery bound) and the same membership rule; only the receipt's case differs."""

    def reason(self, submitted, completed):
        return dict(super().reason(submitted, completed),
                    case='in flight at the pause' if submitted < self.start else 'submitted in the pressure window')


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
