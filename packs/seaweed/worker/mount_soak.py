#!/usr/bin/python3
"""Mount soak load (Stage 1 plan r4): real `weed mount` processes, the workloads, the partition and its witnesses.

Writes the evidence mount_assess.py judges into the run root. Phases never overlap, because the partition stalls every
mount on the host. Config JSON keys: root, weed, filer (host:port), filer_path, volume_host, volume_port, phases_s (A, B,
C), episodes_s, iptables (argv prefix), mounts (int), dual_append_s, write_budget_bytes (W1's total; the lab cluster is
small and volumes only reclaim space on vacuum, so an unbounded W1 ends in ENOSPC and proves nothing).
"""
import argparse, concurrent.futures, errno as E, hashlib, json, os, random, signal, subprocess, sys, threading, time, urllib.error, urllib.request
from pathlib import Path

CHUNK, RECORD = 1 << 18, 256
# Every fsync'd append and attribute change logs the file's whole entry, chunk list included, so an unrotated append
# file grows the filer's metadata log quadratically and fills a small cluster. Rotate and pace to keep W2 honest.
W2_RECORDS_PER_FILE, W2_APPEND_PAUSE_S, W2_ATTR_PAUSE_S = 64, 0.2, 0.1
W3_FILES, W3_SYNC_FILES, W3_SIZE = 24, 4, 1 << 20
W1_BURST_BYTES = 1 << 25  # the largest write has to fit in the bucket
SIZES, SIZE_WEIGHTS = (4096, 1 << 16, 1 << 20, 1 << 24), (50, 30, 19, 1)


def payload(seed, n):  # as mount_assess.payload
    out, i = bytearray(), 0
    while len(out) < n:
        out += hashlib.sha256(('%s/%d' % (seed, i)).encode()).digest()
        i += 1
    return bytes(out[:n])


# Runs fault commands from a process started before any FUSE file is open. A child forked later inherits those
# descriptors, and closing a descriptor with a stalled flush blocks the child until the write gives up, which would
# hold the heal back for minutes.
FAULT_HELPER = '''
import json, subprocess, sys
for line in sys.stdin:
    try:
        r = subprocess.run(json.loads(line), capture_output=True, text=True, timeout=30)
        print(json.dumps([r.returncode, r.stdout]), flush=True)
    except Exception as e:
        print(json.dumps([-1, str(e)]), flush=True)
'''


IPTABLES_NO_SUCH_RULE = 1  # other codes are errors, which prove nothing about the rule


def join_all(threads, seconds):
    """One shared deadline: stuck writers must not cost seconds each. A thread still stuck leaves its op unsettled,
    which the assessor reports as INVALID."""
    deadline = time.monotonic() + seconds
    for t in threads:
        t.join(max(0, deadline - time.monotonic()))


def errname(e):
    return E.errorcode.get(e.errno, str(e.errno)) if isinstance(e, OSError) and e.errno else type(e).__name__


class Run:
    def __init__(self, c):
        self.c, self.root, self.lock, self.op = c, Path(c['root']), threading.Lock(), 0
        self.journal = open(self.root / 'mount-journal.jsonl', 'a', buffering=1)
        self.mounts, self.procs = {}, []
        self.cohorts, self.faults, self.w1_state, self.w2_current = {}, {}, {}, {}
        self.w1_left = c['write_budget_bytes']
        self.w1_rate = c['write_budget_bytes'] / max(1, sum(c['phases_s']))  # bytes per second over all load phases
        self.w1_tokens, self.w1_at = 0.0, time.monotonic()
        self.helper = subprocess.Popen([sys.executable, '-I', '-c', FAULT_HELPER], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)

    def begin(self):
        with self.lock:
            self.op += 1
            return str(self.op), time.monotonic()

    def log(self, **row):
        line = json.dumps(row, sort_keys=True)
        with self.lock:
            self.journal.write(line + '\n')

    def done(self, op, start, **row):
        self.log(op=op, start=start, end=time.monotonic(), **row)

    # --- Mounts
    def mount(self, name, writeback=False):
        d = self.root / 'mnt' / name
        d.mkdir(parents=True, exist_ok=True)
        argv = [self.c['weed'], 'mount', '-filer=' + self.c['filer'], '-filer.path=' + self.c['filer_path'], '-dir=' + str(d),
                '-volumeServerAccess=direct', '-dirAutoCreate']
        if writeback:
            argv.append('-writebackCache')
        p = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=open(self.root / ('mount-%s.log' % name), 'ab'),
                             stderr=subprocess.STDOUT, start_new_session=True)
        for _ in range(300):
            if os.path.ismount(d):
                self.mounts[name] = dict(dir=d, proc=p, argv=argv)
                self.procs.append((name, p))
                return d
            if p.poll() is not None:
                break
            time.sleep(0.1)
        raise RuntimeError('mount %s did not start' % name)

    def unmount(self, name):
        m = self.mounts.pop(name)
        self.procs = [(n, p) for n, p in self.procs if n != name]
        subprocess.run(['fusermount', '-u', str(m['dir'])], check=False, timeout=120)
        try:
            m['proc'].wait(30)
        except subprocess.TimeoutExpired:
            m['proc'].kill()

    def path(self, mount, rel):
        return self.mounts[mount]['dir'] / rel

    def check_mounts(self):
        for name, p in self.procs:
            if p.poll() is not None:
                self.log(t='mount_exit', mount=name, after_start=True, code=p.returncode)
                self.procs = [(n, q) for n, q in self.procs if n != name]

    # --- Files
    def write_file(self, path, seed, size, do_fsync=True):
        """-> (full, fsync rc or None, close rc or None, errno). Never raises."""
        data, full, fs, cl, err = payload(seed, size), True, None, None, None
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
        except OSError as e:
            return False, None, None, errname(e)
        try:
            for i in range(0, size, CHUNK):
                part = data[i:i + CHUNK]
                if os.write(fd, part) != len(part):
                    full = False
                    break
            if do_fsync:
                os.fsync(fd)
                fs = 0
        except OSError as e:
            full, err = False if fs is None and not do_fsync else full, errname(e)
            if do_fsync and fs is None:
                fs = e.errno or 5
            else:
                full = False
        try:
            os.close(fd)
            cl = 0
        except OSError as e:
            cl, err = e.errno or 5, err or errname(e)
        return full, fs, cl, err

    def read_file(self, path):
        try:
            data = path.read_bytes()
            return 0, hashlib.sha256(data).hexdigest(), len(data), None
        except OSError as e:
            return -1, '', 0, errname(e)

    # --- W1: one owner per generation; other mounts read closed files
    def w1(self, m, rng, stop, shared):
        state = self.w1_state.setdefault(m, dict(n=0, live=[]))
        live = state['live']  # names and generations stay unique across phases
        while not stop.is_set():
            state['n'] += 1
            n = state['n']
            action, rel = rng.random(), None
            size = rng.choices(SIZES, SIZE_WEIGHTS)[0]
            if (action < 0.5 or not live) and self.spend(size, stop):
                rel = '%s/f%d' % (m, n)
                if self.w1_write(m, rel, 'create', '%s-%d' % (m, n), size):
                    live.append(rel)  # only acknowledged files may be renamed, replaced or deleted later
                else:
                    rel = None
            elif action < 0.7 and live and self.spend(size, stop):
                rel = rng.choice(live)
                self.w1_replace(m, rel, '%s-%d' % (m, n), size)
            elif not live or action < 0.7:
                time.sleep(0.05)  # the budget is spent: W1 goes on with renames, deletes and reads
            elif action < 0.8:
                rel = live.pop(rng.randrange(len(live)))
                to = '%s/r%d' % (m, n)
                op, s = self.begin()
                try:
                    os.rename(self.path(m, rel), self.path(m, to))
                    rc, err = 0, None
                    live.append(to)
                except OSError as e:
                    rc, err = -1, errname(e)
                self.done(op, s, t='mut', kind='rename', path=rel, to=to, gen='', seed='', size=0, sha='', full=True, fsync=0,
                          close=0, rc=rc, errno=err, mount=m)
            elif action < 0.9:
                rel = live.pop(rng.randrange(len(live)))
                op, s = self.begin()
                try:
                    os.unlink(self.path(m, rel))
                    rc, err = 0, None
                except OSError as e:
                    rc, err = -1, errname(e)
                self.done(op, s, t='mut', kind='delete', path=rel, gen='', seed='', size=0, sha='', full=True, fsync=0, close=0,
                          rc=rc, errno=err, mount=m)
            else:
                others = [x for x in shared.get('w1', ()) if not x.startswith(m + '/')]
                if others:
                    rel = rng.choice(others)
                    op, s = self.begin()
                    rc, sha, size, err = self.read_file(self.path(m, rel))
                    self.done(op, s, t='read', path=rel, rc=rc, sha=sha, size=size, errno=err, mount=m)
            shared['w1'] = shared.get('w1', [])[-200:] + ([rel] if rel and rel in live else [])

    def start_phase_budget(self, secs):
        """Each load phase gets its own share, so no phase can spend another's, and nothing accrues across a partition."""
        with self.lock:
            self.w1_left = int(self.c['write_budget_bytes'] * secs / max(1, sum(self.c['phases_s'])))
            self.w1_tokens, self.w1_at = 0.0, time.monotonic()

    def spend(self, size, stop):
        """Wait for this write's share of the paced budget; False once the budget is gone or the phase ends."""
        while not stop.is_set():
            with self.lock:
                now = time.monotonic()
                self.w1_tokens = min(self.w1_tokens + (now - self.w1_at) * self.w1_rate, W1_BURST_BYTES)
                self.w1_at = now
                if self.w1_left < size:
                    return False
                if self.w1_tokens >= size:
                    self.w1_tokens -= size
                    self.w1_left -= size
                    return True
            time.sleep(0.05)
        return False

    def w1_write(self, m, rel, kind, seed, size):
        op, s = self.begin()
        full, fs, cl, err = self.write_file(self.path(m, rel), seed, size)
        self.done(op, s, t='mut', kind=kind, path=rel, gen=seed, seed=seed, size=size, sha=hashlib.sha256(payload(seed, size)).hexdigest(),
                  full=full, fsync=fs, close=cl, rc=0, errno=err, mount=m)
        return full and fs == 0

    def w1_replace(self, m, rel, seed, size):
        tmp = self.path(m, rel + '.tmp')
        op, s = self.begin()
        full, fs, cl, err = self.write_file(tmp, seed, size)
        rename = -1
        if full and fs == 0:
            try:
                os.rename(tmp, self.path(m, rel))
                rename = 0
            except OSError as e:
                err = errname(e)
        self.done(op, s, t='mut', kind='replace', path=rel, gen=seed, seed=seed, size=size, sha=hashlib.sha256(payload(seed, size)).hexdigest(),
                  full=full, fsync=fs, close=cl, rename=rename, rc=0, errno=err, mount=m)

    # --- W2: one appender, one attribute mount per file, self-identifying records
    @staticmethod
    def record(file, seq):
        head = ('%s %08d ' % (file, seq)).encode()
        body = hashlib.sha256(head).hexdigest().encode()
        return (head + body + b' ' * RECORD)[:RECORD - 1] + b'\n'

    def parse(self, file, data):
        seqs, valid = [], True
        for i in range(0, len(data) - len(data) % RECORD, RECORD):
            rec = data[i:i + RECORD]
            parts = rec.split(b' ', 2)
            try:
                seq = int(parts[1])
            except (IndexError, ValueError):
                valid = False
                break
            if rec != self.record(file, seq):
                valid = False
                break
            seqs.append(seq)
        return seqs, valid

    def w2_append(self, m, base, seqs, stop):
        """One appender per file, rotating so no file's chunk list grows without bound."""
        gen = self.w2_current.get(base, (0,))[0]
        while not stop.is_set():
            file, seq = '%s-g%d' % (base, gen), None
            seq = seqs.get(file, 0)
            self.w2_current[base] = (gen, file)
            if seq >= W2_RECORDS_PER_FILE:
                gen += 1
                continue
            op, s = self.begin()
            full, fs, cl, err = True, None, None, None
            try:
                fd = os.open(self.path(m, 'w2/' + file), os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
            except OSError as e:
                self.done(op, s, t='app', file=file, seq=seq, full=False, fsync=None, close=None, errno=errname(e), mount=m)
                time.sleep(0.2)
                continue
            try:
                if os.write(fd, self.record(file, seq)) != RECORD:
                    full = False
                os.fsync(fd)
                fs = 0
            except OSError as e:
                err, fs = errname(e), e.errno or 5
            try:
                os.close(fd)
                cl = 0
            except OSError as e:
                cl, err = e.errno or 5, err or errname(e)
            self.done(op, s, t='app', file=file, seq=seq, full=full, fsync=fs, close=cl, errno=err, mount=m)
            if full and fs == 0:
                seqs[file] = seq + 1
            time.sleep(W2_APPEND_PAUSE_S)

    def w2_attr(self, m, base, stop):
        n = 0
        while not stop.is_set():
            n += 1
            file = self.w2_current.get(base, (0, None))[1]
            if file is None:
                time.sleep(0.05)
                continue
            mode, val = 0o600 + (n % 64), 'v%d' % n
            op, s = self.begin()
            try:
                p = self.path(m, 'w2/' + file)
                os.chmod(p, mode)
                os.setxattr(p, 'user.soak', val.encode())
                os.utime(p)
                rc, err = 0, None
            except OSError as e:
                rc, err = -1, errname(e)
            self.done(op, s, t='attr', file=file, mode=mode, xattr=val, rc=rc, errno=err, mount=m)
            time.sleep(W2_ATTR_PAUSE_S)

    def w2_read(self, m, base, stop):
        while not stop.is_set():
            file = self.w2_current.get(base, (0, None))[1]
            if file is None:
                time.sleep(0.05)
                continue
            op, s = self.begin()
            try:
                p = self.path(m, 'w2/' + file)
                data = p.read_bytes()
                mode = os.stat(p).st_mode & 0o777
                seqs, valid = self.parse(file, data)
                self.done(op, s, t='wread', file=file, seqs=seqs, valid=valid, mode=mode, initial_mode=0o644, mount=m)
            except OSError as e:
                self.done(op, s, t='wread', file=file, seqs=[], valid=True, mode=None, errno=errname(e), mount=m)
            time.sleep(0.5)

    def load_phase(self, name, secs, names, seqs):
        """Run W1 and W2 for secs; returns when every thread has exited."""
        self.start_phase_budget(secs)
        stop, shared, ts = threading.Event(), {}, []
        for i, m in enumerate(names):
            ts.append(threading.Thread(target=self.w1, args=(m, random.Random('%s-%s' % (m, name)), stop, shared), daemon=True))
        for i in range(0, len(names) - 1, 2):
            a, b, f = names[i], names[i + 1], 'file%d' % (i // 2)
            ts.append(threading.Thread(target=self.w2_append, args=(a, f, seqs, stop), daemon=True))
            ts.append(threading.Thread(target=self.w2_attr, args=(b, f, stop), daemon=True))
            ts.append(threading.Thread(target=self.w2_read, args=(b, f, stop), daemon=True))
        for t in ts:
            t.start()
        began, end = time.monotonic(), time.monotonic() + secs
        while time.monotonic() < end:
            self.check_mounts()
            time.sleep(1)
        stop.set()
        join_all(ts, 300)
        self.log(t='phase', name=name, start=began, end=time.monotonic())

    # --- W3 and its witnesses
    def filer_meta(self, path):
        url = 'http://%s%s/%s?metadata=true' % (self.c['filer'], self.c['filer_path'], path)
        try:
            with urllib.request.urlopen(url, timeout=10) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            return None if e.code == 404 else {'unknown': e.code}
        except (OSError, ValueError):
            return {'unknown': 'unreachable'}

    def fault_cmd(self, argv):
        """-> [rc, stdout]. Never raises: a broken channel is rc -1, which proves nothing."""
        try:
            self.helper.stdin.write(json.dumps(self.c['iptables'] + argv) + '\n')
            self.helper.stdin.flush()
            return json.loads(self.helper.stdout.readline())
        except (OSError, ValueError) as e:
            return [-1, str(e)]

    def rule(self, action):
        c = self.c
        return self.fault_cmd(['iptables', action, 'OUTPUT', '-p', 'tcp', '-d', c['volume_host'], '--dport', str(c['volume_port']),
                               '-m', 'comment', '--comment', 'swmountsoak', '-j', 'DROP'])[0]

    def counter(self):
        lines = [ln for ln in self.fault_cmd(['iptables', '-L', 'OUTPUT', '-v', '-x', '-n'])[1].splitlines() if 'swmountsoak' in ln]
        return int(lines[0].split()[0]) if lines else None

    def episode(self, ep, secs):
        """The partition is owned here: every way out, including an exception, removes the rule."""
        try:
            self.run_episode(ep, secs)
        finally:
            self.heal_rule()

    def heal_rule(self):
        """Remove every copy of the rule; stops at the first answer other than success."""
        for _ in range(8):
            if self.rule('-D') != 0:
                break

    def run_episode(self, ep, secs):
        c, wb, sync = self.c, 'wb', 'sync'
        rec = dict(argv_direct=all('-volumeServerAccess=direct' in self.mounts[m]['argv'] for m in (wb, sync)))
        if self.rule('-I') != 0:
            raise RuntimeError('fault rule not applied')
        apply_t, start_count = time.monotonic(), self.counter()
        trace = lambda what: self.log(t='trace', ep=ep, what=what, after_apply_s=round(time.monotonic() - apply_t, 2))
        rec.update(apply=apply_t, rule_confirmed=self.rule('-C') == 0 and start_count is not None)
        files, sync_ts, cohort, flock = [], [], [], threading.Lock()

        def wb_write(i):
            rel, seed = 'w3-%s/f%d' % (ep, i), '%s-%d' % (ep, i)
            op, s = self.begin()
            full, fs, cl, err = self.write_file(self.path(wb, rel), seed, W3_SIZE, do_fsync=False)
            sha = hashlib.sha256(payload(seed, W3_SIZE)).hexdigest()
            self.done(op, s, t='w3', mount='wb', ep=ep, path=rel, full=full, fsync=fs, close=cl, sha=sha, size=W3_SIZE, errno=err)
            with flock:
                files.append(dict(path=rel, sha=sha, size=W3_SIZE, ack=full and cl == 0))
        wb_ts = [threading.Thread(target=wb_write, args=(i,), daemon=True) for i in range(W3_FILES)]
        for t in wb_ts:
            t.start()
        trace('writers started')
        admit_until = apply_t + secs * 0.5  # never let a stalled write hold the partition open
        for t in wb_ts:
            t.join(max(0, admit_until - time.monotonic()))
        with flock:
            files = list(files)  # the cohort can only come from writes acked by now

        def sync_write(i):
            rel, seed = 'sync-%s/f%d' % (ep, i), 'sync-%s-%d' % (ep, i)
            op, s = self.begin()
            full, fs, cl, err = self.write_file(self.path(sync, rel), seed, W3_SIZE)
            self.done(op, s, t='w3', mount='sync', ep=ep, path=rel, full=full, fsync=fs, close=cl,
                      sha=hashlib.sha256(payload(seed, W3_SIZE)).hexdigest(), size=W3_SIZE, errno=err)
        for i in range(W3_SYNC_FILES):
            sync_ts.append(threading.Thread(target=sync_write, args=(i,), daemon=True))
            sync_ts[-1].start()
        time.sleep(min(5, secs / 4))
        trace('admitted')
        # Concurrent, so these lookups cannot push the heal past its schedule.
        with concurrent.futures.ThreadPoolExecutor(16) as pool:
            pending = list(pool.map(self.filer_meta, [f['path'] for f in files]))
            time.sleep(min(5, secs / 4))
            again = list(pool.map(self.filer_meta, [f['path'] for f in files]))
        trace('lookups done')
        chunkless = lambda m: m is None or ('unknown' not in m and not m.get('chunks'))
        cohort = [dict(path=f['path'], sha=f['sha'], size=f['size']) for f, a, b in zip(files, pending, again) if f['ack'] and chunkless(a) and chunkless(b)]
        rec['filer_ok'] = all(not (isinstance(m, dict) and 'unknown' in m) for m in pending + again)
        doc = dict(files=cohort, frozen_before_heal=True)
        raw = json.dumps(doc, sort_keys=True)
        self.log(t='cohort', ep=ep, sha=hashlib.sha256(raw.encode()).hexdigest(), files=len(cohort))
        self.cohorts[ep] = doc
        (self.root / 'mount-cohorts.json').write_text(json.dumps(self.cohorts, sort_keys=True))
        trace('cohort frozen')
        rest = apply_t + secs - time.monotonic()
        if rest > 0:
            time.sleep(rest)
        end_count = self.counter()
        rec['counter_rose'] = end_count is not None and start_count is not None and end_count > start_count
        self.rule('-D')
        rec['heal'] = heal_t = time.monotonic()
        trace('healed')
        rec['rule_gone'] = self.rule('-C') == IPTABLES_NO_SUCH_RULE
        join_all(sync_ts + wb_ts, 180)
        self.faults[ep] = rec
        (self.root / 'mount-faults.json').write_text(json.dumps(self.faults, sort_keys=True))
        self.w3_outcome(ep, cohort, heal_t)

    def w3_outcome(self, ep, cohort, heal_t):
        deadline, left = heal_t + 120, {f['path']: f for f in cohort}
        while left and time.monotonic() < deadline:
            for rel in list(left):
                m = self.filer_meta(rel)
                if m and 'unknown' not in m and m.get('chunks'):
                    del left[rel]
            time.sleep(1)
        self.mount('fresh-' + ep)
        for f in cohort:
            op, s = self.begin()
            m = self.filer_meta(f['path'])
            rc, sha, size, err = self.read_file(self.path('fresh-' + ep, f['path']))
            self.done(op, s, t='w3final', ep=ep, path=f['path'], chunks=bool(m and m.get('chunks')), rc=rc, sha=sha, size=size, errno=err,
                      after_heal_s=time.monotonic() - heal_t)
        for i in range(W3_SYNC_FILES):
            rel = 'sync-%s/f%d' % (ep, i)
            op, s = self.begin()
            rc, sha, size, err = self.read_file(self.path('fresh-' + ep, rel))
            self.done(op, s, t='sync_final', ep=ep, path=rel, rc=rc, sha=sha, size=size, errno=err)
        self.unmount('fresh-' + ep)

    # --- Final barrier and verification
    def dual_append(self, a, b, secs):
        """Two mounts appending one file: report only (no POSIX cross-client guarantee)."""
        stop, file = threading.Event(), 'dual'

        def run(m):
            n = 0
            while not stop.is_set():
                op, s = self.begin()
                try:
                    with open(self.path(m, 'w2/' + file), 'ab') as f:
                        f.write(self.record(file, n))
                    self.done(op, s, t='dual', mount=m, n=n)
                except OSError as e:
                    self.done(op, s, t='dual', mount=m, n=n, errno=errname(e))
                n += 1
                time.sleep(1)  # one shared file: unpaced, its chunk list fills the metadata log
        ts = [threading.Thread(target=run, args=(m,), daemon=True) for m in (a, b)]
        for t in ts:
            t.start()
        time.sleep(secs)
        stop.set()
        join_all(ts, 120)

    def finish(self, names, seqs):
        self.mount('barrier')
        deadline = time.monotonic() + 120  # one shared wait: a file short of its acked size is what the final check judges
        for f, n in seqs.items():
            want = n * RECORD
            while time.monotonic() < deadline:
                try:
                    if os.stat(self.path('barrier', 'w2/' + f)).st_size >= want:
                        break
                except OSError:
                    pass
                time.sleep(0.5)
        sentinels = {}
        for f in sorted(seqs):
            attr_mount = names[int(f.split('-')[0][4:]) * 2 + 1]  # the attribute mount of that file's pair
            mode, val = 0o640, 'sentinel-' + f
            ok = True
            try:
                p = self.path(attr_mount, 'w2/' + f)
                os.chmod(p, mode)
                os.setxattr(p, 'user.soak', val.encode())
            except OSError:
                ok = False
            sentinels[f] = (ok, mode, val)
        self.unmount('barrier')
        self.mount('final')
        for f, (ok, mode, val) in sentinels.items():
            op, s = self.begin()
            try:
                p = self.path('final', 'w2/' + f)
                data = p.read_bytes()
                seqs_seen, valid = self.parse(f, data)
                seen = os.stat(p).st_mode & 0o777
                xv = os.getxattr(p, 'user.soak').decode()
            except OSError as e:
                seqs_seen, valid, seen, xv = [], False, None, None
                self.log(t='barrier_error', file=f, errno=errname(e))
            self.done(op, s, t='barrier', file=f, seqs=seqs_seen, valid=valid, sentinel_acked=ok, sentinel_mode=mode, sentinel_xattr=val,
                      seen_mode=seen, seen_xattr=xv)
        base = self.path('final', '')
        for m in names:
            d = base / m
            for dirpath, _, fnames in os.walk(d):
                for fn in fnames:
                    if fn.endswith('.tmp'):
                        continue
                    full = Path(dirpath) / fn
                    rel = str(full.relative_to(base))
                    op, s = self.begin()
                    rc, sha, size, err = self.read_file(full)
                    self.done(op, s, t='final', path=rel, rc=rc, sha=sha, size=size, errno=err)
        self.unmount('final')


def fuse_ok():
    return os.path.exists('/dev/fuse') and 'user_allow_other' in Path('/etc/fuse.conf').read_text()


def drive(run, c, root):
    names = ['m%d' % i for i in range(c['mounts'])]
    env = dict(fuse_ok=fuse_ok(), mounts=names, mounts_started=False)
    try:
        if not env['fuse_ok']:
            raise RuntimeError('FUSE unavailable')
        body = b'--swms\r\nContent-Disposition: form-data; name="file"; filename=".keep"\r\n\r\nkeep\r\n--swms--\r\n'  # creates filer_path
        urllib.request.urlopen(urllib.request.Request('http://%s%s/.keep' % (c['filer'], c['filer_path']), data=body, method='POST',
                                                      headers={'Content-Type': 'multipart/form-data; boundary=swms'}), timeout=30).read()
        for m in names:
            run.mount(m)
        run.mount('wb', writeback=True)
        run.mount('sync')
        (run.path(names[0], 'w2')).mkdir(exist_ok=True)
        for m in names:
            for _ in range(600):
                if run.path(m, 'w2').is_dir():
                    break
                time.sleep(0.1)
            else:
                raise RuntimeError('w2 not visible on ' + m)
        env['mounts_started'] = True
    finally:
        (root / 'mount-env.json').write_text(json.dumps(env, sort_keys=True))
    seqs = {}
    a, b, last = c['phases_s']
    e1, e2 = c['episodes_s']
    run.load_phase('A', a, names, seqs)
    run.episode('E1-%ds' % e1, e1)
    run.load_phase('B', b, names, seqs)
    run.episode('E2-%ds' % e2, e2)
    run.load_phase('C', last, names, seqs)
    run.dual_append(names[0], names[1], c['dual_append_s'])
    run.finish(names, seqs)
    run.log(t='end', dispatched=run.op)
    print(json.dumps(dict(status='LOAD_DONE', ops=run.op)))


def teardown(run, root):
    """On every path out: remove the rule, unmount everything, then prove absence. Anything unchecked stays UNKNOWN."""
    run.heal_rule()
    for m in list(run.mounts):
        try:
            run.unmount(m)
        except Exception:  # keep going: the check below decides
            pass
    rc, listing = run.fault_cmd(['iptables', '-S', 'OUTPUT'])
    try:
        mounts = [ln for ln in Path('/proc/mounts').read_text().splitlines() if str(root) in ln]
    except OSError:
        mounts = None
    proven = rc == 0 and 'swmountsoak' not in listing and mounts == []
    (root / 'mount-cleanup.json').write_text(json.dumps('PROVEN' if proven else 'UNKNOWN'))


def stop_on_term(signum, frame):
    raise SystemExit(143)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', required=True)
    c = json.loads(parser.parse_args().config)
    root = Path(c['root'])
    run = Run(c)
    signal.signal(signal.SIGTERM, stop_on_term)
    code = 1
    try:
        drive(run, c, root)
        code = 0
    finally:
        teardown(run, root)
        run.journal.flush()
    os._exit(code)


if __name__ == '__main__':
    signal.signal(signal.SIGPIPE, signal.SIG_DFL)
    main()
