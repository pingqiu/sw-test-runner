"""Per-worker native stats for the KV soak (step 3). Vendored from the r9
harness (soak_series.native_stats, host_ops.identity) without its soak-go
handshake: the recorder starts from the load-start witness the soak load
writes before its first operation, and ticks on CLOCK_MONOTONIC."""
import json
import math
import os
import time
from pathlib import Path

WITNESS = 'soak-load-start.json'
TICK_S = 30


def identity(pid):
    """(pid, starttime, state, pgid) from /proc, or None once the process is gone."""
    try:
        raw = Path('/proc/%d/stat' % pid).read_text().rsplit(')', 1)[1].split()
    except FileNotFoundError:
        return None
    return dict(pid=pid, starttime=int(raw[19]), state=raw[0], pgid=int(raw[2]))


def boot_id():
    return Path('/proc/sys/kernel/random/boot_id').read_text().strip()


def read_witness(root):
    """The load-start witness, or None while it is not written yet. A witness
    from another boot, or a non-finite clock, is refused."""
    path = Path(root) / WITNESS
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except FileNotFoundError:
        return None
    with os.fdopen(fd) as stream:
        row = json.load(stream)
    mono = row.get('mono')
    if row.get('boot_id') != boot_id() or not isinstance(mono, (int, float)) or isinstance(mono, bool) \
            or not math.isfinite(mono) or mono <= 0:
        raise ValueError('foreign or malformed load-start witness')
    return row


def native_stats(conn, root, stop, horizon_s, wait_s=60):
    """Write one stats row per TICK_S from the load start until horizon_s
    (duration + quiescence). Returns early when stop is set. Never unbounded:
    no witness within wait_s ends the recorder with a NOT_STARTED row."""
    root = Path(root)
    me = identity(os.getpid())
    path = root / ('native-stats-%d-%d.jsonl' % (os.getpid(), me['starttime']))
    with path.open('x', buffering=1) as output:
        deadline = time.monotonic() + wait_s
        witness = read_witness(root)
        while witness is None:
            if stop.wait(0.2):
                return
            if time.monotonic() > deadline:
                output.write(json.dumps(dict(status='NOT_STARTED', error='no load-start witness')) + '\n')
                return
            witness = read_witness(root)
        start = witness['mono']
        tick = max(0, math.ceil((time.monotonic() - start) / TICK_S))
        while tick * TICK_S <= horizon_s and not stop.is_set():
            if stop.wait(max(0, start + tick * TICK_S - time.monotonic())):
                return
            row = dict(mono=time.monotonic(), body_elapsed=tick * TICK_S, identity=identity(os.getpid()))
            try:
                row['stats'] = json.loads(conn.stats_json())
                row['status'] = 'OBSERVED'
            except Exception as error:  # the row records it; the assessor decides
                row.update(status='NOT_OBSERVED', error=repr(error))
            output.write(json.dumps(row) + '\n')
            tick += 1
