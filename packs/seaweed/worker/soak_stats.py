"""Per-worker native stats for the KV soak (step 3). Vendored from the r9
harness (soak_series.native_stats, host_ops.identity) without its soak-go
handshake: the recorder starts from the load-start witness the soak load
writes before its first operation, and ticks on CLOCK_MONOTONIC."""
import json
import math
import os
import re
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


MEMORY_FIELDS = (('VmPin:', 'vm_pin_kB'), ('VmLck:', 'vm_lck_kB'), ('VmRSS:', 'vm_rss_kB'))


def proc_memory(status='/proc/self/status'):
    """This process's pinned (VmPin: RDMA-registered pages), locked and resident
    memory in kB, as /proc reports them; a field the kernel doesn't report is
    absent, never 0. None when the file can't be read."""
    try:
        lines = Path(status).read_text().splitlines()
    except OSError:
        return None
    out = {}
    for line in lines:
        for field, key in MEMORY_FIELDS:
            parts = line.split()
            if line.startswith(field) and len(parts) >= 2 and parts[1].isdigit():
                out[key] = int(parts[1])
    return out


# Stage 3 host-side views of one process (the volume), each None when it cannot be read. The sampler in
# soak_fault.py joins them on pid/starttime; the assessor classifies them. No view is ever guessed as 0.

VOLUME_FILE = re.compile(r'\.(dat|idx|vif|ecx|ecj|ec[0-9]{2}|cpd|cpx|note)$')  # a volume's own data and index files


def fd_view(pid):
    """(open fd count, sizes of the regular *.dat files it holds open, how many fds are volume data/index files),
    or None. The third lets a reader tell fds held by volumes the load created from any other fd."""
    base = Path('/proc/%d/fd' % pid)
    try:
        names = os.listdir(base)
    except OSError:
        return None
    dats, data = [], 0
    for name in names:
        try:
            target = os.readlink(base / name)
            data += bool(VOLUME_FILE.search(target))
            if target.endswith('.dat'):
                dats.append(os.stat(base / name).st_size)
        except OSError:
            continue  # a descriptor closed between listing and reading
    return len(names), sorted(dats), data


def limits(pid):
    """{'memlock': [soft, hard], 'nofile': [soft, hard]} from /proc/pid/limits; unlimited is -1. None if unread."""
    want = {'Max locked memory': 'memlock', 'Max open files': 'nofile'}
    try:
        lines = Path('/proc/%d/limits' % pid).read_text().splitlines()
    except OSError:
        return None
    out = {}
    for line in lines:
        for label, key in want.items():
            if line.startswith(label):
                soft, hard = line[len(label):].split()[:2]
                out[key] = [-1 if v == 'unlimited' else int(v) for v in (soft, hard)]
    return out if len(out) == 2 else None


def rdma_rows(kind, run):
    """`rdma -j resource show <qp|mr>` as a list of row dicts, or None when the tool fails or the shape differs."""
    try:
        done = run(['rdma', '-j', 'resource', 'show', kind], capture_output=True, text=True, timeout=10)
        rows = json.loads(done.stdout) if done.returncode == 0 else None
    except Exception:  # missing tool, timeout, bad JSON: the view is unread
        return None
    if isinstance(rows, dict):  # some iproute2 versions wrap the table
        rows = rows.get(kind)
    if not isinstance(rows, list) or not all(isinstance(r, dict) for r in rows):
        return None
    return rows


def rdma_view(pid, run=None):
    """QPs and MRs owned by pid: {'qp': n, 'mr': n, 'mr_lens': [...]}, or None. A table with any row that carries
    no integer pid cannot be PID-joined (that row could be this process's), so the whole view is unread, never a
    count that silently leaves rows out."""
    import subprocess
    run = run or subprocess.run
    qp, mr = rdma_rows('qp', run), rdma_rows('mr', run)
    if qp is None or mr is None:
        return None
    if any(isinstance(r.get('pid'), bool) or not isinstance(r.get('pid'), int) for r in qp + mr):
        return None
    mine = lambda rows: [r for r in rows if r.get('pid') == pid]
    lens = [r.get('mrlen') for r in mine(mr)]
    if any(isinstance(v, bool) or not isinstance(v, int) for v in lens):
        return None
    return dict(qp=len(mine(qp)), mr=len(lens), mr_lens=sorted(lens))


def scrape(url, series, timeout=3):
    """The configured Prometheus series: {key: summed value}. A series is a bare metric name (every label set
    summed) or one exact `name{labels}` token. A key whose series is absent is absent, never 0. None if unread."""
    import urllib.request
    try:
        with urllib.request.urlopen(url, timeout=timeout) as response:
            text = response.read(4 << 20).decode()
    except Exception:
        return None
    out = {}
    for line in text.splitlines():
        if not line or line.startswith('#'):
            continue
        token, _, rest = line.partition(' ')
        name = token.split('{', 1)[0]
        try:
            value = float(rest.split()[0])
        except (IndexError, ValueError):
            continue
        for key, want in series.items():
            if token == want or ('{' not in want and name == want):
                out[key] = out.get(key, 0.0) + value
    return out


def port_view(sys_root, device, port):
    """(state text, link_downed counter) of one RDMA port, or None."""
    base = Path(sys_root) / 'class' / 'infiniband' / device / 'ports' / str(port)
    try:
        return (base / 'state').read_text().strip(), int((base / 'counters' / 'link_downed').read_text())
    except (OSError, ValueError):
        return None


def statfs_free(mount):
    try:
        s = os.statvfs(mount)
    except OSError:
        return None
    return s.f_bavail * s.f_frsize


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
            row = dict(mono=time.monotonic(), body_elapsed=tick * TICK_S, identity=identity(os.getpid()),
                       memory=proc_memory())  # #997: pinned bytes per client process
            try:
                row['stats'] = json.loads(conn.stats_json())
                row['status'] = 'OBSERVED'
            except Exception as error:  # the row records it; the assessor decides
                row.update(status='NOT_OBSERVED', error=repr(error))
            output.write(json.dumps(row) + '\n')
            tick += 1
