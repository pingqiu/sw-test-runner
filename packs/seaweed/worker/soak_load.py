"""M01 KV soak load (step 3a): the r9 workload loop without remote shells, fixed hosts
or a control handshake. Four native workers (3 DC + 1 HTTP), the oracle,
read-after-ACK, the client kill, a read-only topology poller (decision 3),
bounded evidence files and one bounded summary row on stdout."""
import argparse, bisect, hashlib, json, math, multiprocessing as mp, os, random, re, stat, sys, threading, time, types, urllib.request
from pathlib import Path


def pinned(name):
    """The exact protected sibling, as lab_profile loads vf_preflight: -I drops the script directory from
    sys.path and nothing is searched for. Registered under its name, so `from <name> import` resolves to it."""
    source = Path(__file__).resolve().with_name(name + '.py')
    module = sys.modules.get(name)
    if module is not None and getattr(module, '__file__', None) and Path(module.__file__).resolve() == source:
        return module
    module = types.ModuleType(name)
    module.__file__ = str(source)
    sys.modules[name] = module
    exec(compile(source.read_bytes(), str(source), 'exec'), module.__dict__)
    return module


for _name in ('soak_native_worker', 'soak_oracle', 'soak_stats'):
    pinned(_name)
from soak_native_worker import child, payload
from soak_oracle import LinkFlapWindow, Oracle, OracleError, PressureWindow
from soak_stats import WITNESS, boot_id, identity

# NOT_FOUND (status 1) in exactly the two spellings a soak read can carry: the DC batch push read, and the RC
# push-read slot fetch (#999, sw-rdma push_read.rs), whose full message is required so no other status or push form
# (status 10, bykey, direct, kv slab, the unbatched DC) can pass as a miss.
MISS = re.compile(r'DC batch push-read response status 1(?:\D|$)'
                  r'|(?<!DC )(?<!batch )push-read response status 1 pipe=\d+ request_id=\d+ fid=\S+ offset=\d+ length=\d+ slot=\d+')
TRANSIENT = re.compile(r'timeout|timed out|reset|unavailable|refused|closed|broken pipe', re.I)  # r9's fault kinds
EVIDENCE = ('ack-journal.jsonl', 'op-events.jsonl', 'topology-polls.jsonl', 'completions.jsonl')
ROW_EVIDENCE = ('write-ledger.jsonl', 'audit.jsonl')  # stage 3 fault rows only
FLAP_EVIDENCE = ('late-completions.jsonl',)  # F3 and F5: completions of futures abandoned at their deadline
OWN_EVIDENCE = ('ownership.jsonl',)  # F5 only (r26): unblock/orphan events, every child's final row and observed exit
AUDIT_OP_S = 35  # one audit GET: the 30 s op limit plus margin; past it the audit stops incomplete


REAP_GAPS_S = (20, 3, 2, 2, 3)  # T0->K1, K1->G, A->K2, K2->D, D->E (tests may shrink them; production is these)
TEARDOWN_EXIT = 78  # r26 F5: the load's exit status when its teardown is unproven (or proven only after the gate)


class TeardownUnproven(RuntimeError):
    """r26 F5: a child not observed gone by D (or the audit gate missed): no fresh audit verdict, no terminal row, and
    the process must leave without multiprocessing's exit-time join of its still-live children (main())."""


def reap_schedule(t0, audit_s):
    """r26 F5 (plan v6): ONE absolute reap schedule from T0 (the end of the quiescence drain). K1: kill each live load
    worker; G: the audit gate (every load worker's exit observed); A = G + audit_s: the audit deadline (its ready wait
    and every GET wait are clamped to it); K2 = A + 2: kill the audit worker; D = K2 + 2: THE reap deadline for every
    child; E = D + 3: the terminal row and the poller join. E - T0 = audit_s + 30 = the controller's allowance after
    quiescence (audit_s + LoadExitGrace 30); F5's audit_s 20 gives 20/23/43/45/47/50."""
    k1, g, k2, d, e = REAP_GAPS_S
    k = dict(K1=t0 + k1)
    k['G'] = k['K1'] + g
    k['A'] = k['G'] + audit_s
    k['K2'] = k['A'] + k2
    k['D'] = k['K2'] + d
    k['E'] = k['D'] + e
    return k


def reap(children, ids, kill_at, until, exits, kills_sent, stopped, drain, clock=time.monotonic, sleep=time.sleep):
    """r26 F5 (plan v6): stop each child once, then observe exits against absolute instants only. At kill_at each
    still-live child gets kill() on its own Process object (never a pattern). An exit counts only when OBSERVED
    (exitcode is not None) by `until`, stamped with the clock read at that observation (r26 correction: never a time
    taken before a drain); an exit first seen after `until` is no proof. A kill sent, a timed join that returned or a
    final row is no exit proof. children: id -> dict(process=, pipe=); exits/kills_sent/stopped are the caller's
    records, updated in place; drain(id) reads that child's pipe (only before `until`). Returns the children still
    live at `until`."""
    for k in ids:
        if k not in stopped and k not in exits:
            stopped.add(k)
            try:
                children[k]['pipe'].send(dict(action='stop'))
            except (OSError, ValueError):
                pass  # already gone: its exit is observed below
    while True:
        for k in ids:
            if clock() < until:  # pipe work only inside the schedule; a drain that runs past it proves nothing
                try:
                    drain(k)
                except (EOFError, OSError):
                    pass  # its pipe closed: the child is exiting or gone
            code = children[k]['process'].exitcode
            seen = clock()  # the clock AT the observation, after it: never a stale loop time
            if k not in exits and code is not None and seen <= until:
                exits[k] = dict(exitcode=code, mono=seen)
        now = clock()
        live = [k for k in ids if k not in exits]
        if not live or now >= until:
            return live
        if now >= kill_at:
            for k in live:
                if k not in kills_sent:
                    children[k]['process'].kill()
                    kills_sent[k] = now
        sleep(.01)


def evidence_names(row):
    """The load's evidence files for a fault row config (or None): the summary lists exactly these."""
    kind = (row or {}).get('kind')
    return (EVIDENCE + (ROW_EVIDENCE if row else ()) + (FLAP_EVIDENCE if kind in ('link_flap', 'mr_pressure') else ())
            + (OWN_EVIDENCE if kind == 'mr_pressure' else ()))


def volume_ids(topology):
    """Every VID present anywhere in /dir/status (r9 topology_ids rules)."""
    out = set()
    def visit(v):
        if isinstance(v, dict):
            for k, item in v.items():
                if k == 'VolumeIds':
                    if not isinstance(item, str) or re.search(r'[^0-9,\s-]', item):
                        raise ValueError('VolumeIds shape')
                    for token in item.replace(',', ' ').split():
                        lo, hi = (int(x) for x in (token.split('-') * 2)[:2]) if '-' in token else (int(token),) * 2
                        if lo <= 0 or hi < lo or hi - lo > 10000:
                            raise ValueError('VolumeIds bounds')
                        out.update(range(lo, hi + 1))
                visit(item)
        elif isinstance(v, list):
            for item in v:
                visit(item)
    visit(topology)
    return sorted(out)


def poll_topology(config, stop, out):
    """Every 2 s; a poll counts only if it is complete and strictly parsed."""
    lay = config['layout']
    while not stop.is_set():
        row = dict(req=time.monotonic(), ok=False)
        try:
            with urllib.request.urlopen(config['master_http'] + '/dir/status', timeout=1.5) as r:
                topo = json.loads(r.read())
            layouts = topo['Topology']['Layouts']
            row['writables'] = sorted({int(v) for l in layouts if (l['collection'], l['replication'], l.get('ttl') or '')
                                       == (lay['collection'], lay['replication'], lay['ttl']) for v in l['writables'] or []})
            row['present'], row['ok'] = volume_ids(topo['Topology']), True
        except Exception as error:  # a failed poll is a gap, never absence
            row['error'] = repr(error)[:200]
        row['done'] = time.monotonic()
        out.write(json.dumps(row) + '\n')
        stop.wait(max(0, row['req'] + 2 - time.monotonic()))


def classify_get(row, acked_pending):
    """r9's GET rule, ported exactly (archived soak_workload.py sha 6a4bbb6b, lines 54-64 and 74-99): a hit is ok with
    results None or [True]; ok=True with any other results is a MISS CANDIDATE (r9: the not-hit branch, judged by
    retirement proof; here by the decision-3 join in 3b); ok=False is a miss only in the push-read status-1 shape
    (results [False], untouched, r9's DC error text or the RC fetch's), else a credited fault-window error, else fatal."""
    if row['ok'] and row['results'] in (None, [True]):
        return 'hit'
    if row['ok'] is True:
        return 'miss'
    if row['results'] == [False] and row.get('untouched') is True and MISS.search(row.get('error') or ''):
        return 'miss'
    if acked_pending and TRANSIENT.search(row.get('error') or ''):
        return 'fault'
    return 'unexpected'


def publish(path, row):
    """Exclusive and atomic: a reader sees the whole file or none (link fails if it exists)."""
    tmp = path.with_name('.%s.%d.tmp' % (path.name, os.getpid()))
    with tmp.open('x') as f:
        json.dump(row, f)
    try:
        os.link(tmp, path)
    finally:
        tmp.unlink()


def _unique(pairs):
    row = {}
    for k, v in pairs:
        if k in row:
            raise ValueError('duplicate JSON key ' + repr(k)[:40])
        row[k] = v
    return row


def _finite(name):
    raise ValueError('non-finite JSON number ' + name)


def strict_json(raw):
    """One JSON value: duplicate keys and NaN/Infinity refused."""
    return json.loads(raw, object_pairs_hook=_unique, parse_constant=_finite)


def read_evidence(path, limit, line_limit=None, max_rows=None):
    """The one strict evidence reader: no-follow, a regular file, at most `limit` bytes, strict JSON; with
    line_limit, JSON lines (each bounded, a torn last line refused, at most max_rows). None if absent."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return None
    except OSError as error:  # ELOOP: a symlink is never followed
        raise ValueError('evidence not openable without following: ' + path.name) from error
    with os.fdopen(fd, 'rb') as f:
        if not stat.S_ISREG(os.fstat(f.fileno()).st_mode):
            raise ValueError('evidence is not a regular file: ' + path.name)
        raw = f.read(limit + 1)
    if len(raw) > limit:
        raise ValueError('evidence over %d bytes: %s' % (limit, path.name))
    if line_limit is None:
        return strict_json(raw)
    lines = raw.split(bytes([10]))  # newline-terminated rows
    if lines.pop() != b'' or len(lines) > max_rows or any(len(line) > line_limit for line in lines):
        raise ValueError('evidence lines torn or over bound: ' + path.name)
    return [strict_json(line) for line in lines]


def read_bound(path, binding):
    """A controller fault-window file: nofollow, bounded, exactly bound, or None if absent."""
    row = read_evidence(path, 4096)
    if row is None:
        return None
    if not isinstance(row, dict) or any(row.get(k) != v for k, v in binding.items()):
        raise ValueError('fault-window file binding differs: ' + path.name)
    return row


WRITE_ORDER_SEED = 20261104


class Pattern:
    """kv_load's client pattern (LoadPattern in soak_plan.go): which operation a
    free client issues next. write_once writes every key exactly once; read and
    mixed then run for their durations, keys drawn uniformly or by zipf(s)."""

    def __init__(self, p, rng, prefix):
        self.p, self.rng, self.prefix, self.cursor, self.phase, self.started, self.versions = p, rng, prefix, 0, 0, None, {}
        # write_once in a seeded permutation, from its own generator so the
        # read draws are unchanged: a zipf-hot key is not also the oldest one.
        self.order = list(range(p['keys']))
        random.Random(WRITE_ORDER_SEED).shuffle(self.order)
        w = [1.0 if p['distribution'] == 'uniform' else (k + 1) ** -p['zipf_s'] for k in range(p['keys'])]
        self.cum = [sum(w[:1])]
        for x in w[1:]:
            self.cum.append(self.cum[-1] + x)

    def key(self, index):
        return self.prefix + hashlib.sha256(('kv-load-key-%d' % index).encode()).hexdigest()

    def draw(self):
        return min(bisect.bisect(self.cum, self.rng.random() * self.cum[-1]), self.p['keys'] - 1)

    def finished(self, now):
        """True once every declared phase ran to its end."""
        phases = self.p['phases']
        while 0 < self.phase < len(phases) and now >= self.started + phases[self.phase]['duration_s']:
            self.phase, self.started = self.phase + 1, self.started + phases[self.phase]['duration_s']
        return self.phase >= len(phases)

    def next(self, now, writing_allowed, pending_sets, latest, busy):
        """-> ('set', key, seed) | ('get', key) | None for this free client."""
        phases = self.p['phases']
        if self.phase == 0:
            if self.cursor < self.p['keys'] and not writing_allowed:
                return None  # past the write stop, write_once can't finish: finished() then fails the load
            if self.cursor < self.p['keys']:
                index = self.order[self.cursor]
                self.cursor += 1
                return 'set', self.key(index), 'kv-load-%d-0' % index
            if pending_sets:
                return None  # every key is written once and acknowledged before the reads
            self.phase, self.started = 1, now
        if self.finished(now):
            return None
        index = self.draw()
        key = self.key(index)
        ph = phases[self.phase]
        if ph['kind'] == 'mixed' and writing_allowed and self.rng.random() >= ph['read_ratio']:
            if key in busy:
                return None  # a write needs its key to itself (the oracle)
            self.versions[index] = self.versions.get(index, 0) + 1
            return 'set', key, 'kv-load-%d-%d' % (index, self.versions[index])
        if key in pending_sets:
            return None  # never read beside a write; reads beside reads are a hot key's load
        return ('get', key) if key in latest else None


def push_pipes(pat):
    """kv_load's push_pipes knob, over configure()'s value; absent keeps it."""
    pipes = (pat or {}).get('push_pipes')
    if pipes is None:
        return
    if isinstance(pipes, bool) or not isinstance(pipes, int) or not 1 <= pipes <= 8:
        raise ValueError('kv_load push_pipes must be 1..8')
    os.environ['SW_RDMA_KVCACHE_PUSH_PIPES'] = str(pipes)


def read_route(config):
    """The soak's RDMA read route (#999): absent is 'dc'. 'rc' turns DC push off for every worker (they
    inherit this environment) and drops the DC selectors, so no worker can reach DC by accident."""
    route = config.get('read_transport', 'dc')
    if route not in ('dc', 'rc') or (route == 'rc' and config.get('pattern')):
        raise ValueError('soak read_transport must be dc or rc, and rc is the soak only')
    if route == 'rc':
        for key in [k for k in os.environ if k.startswith('SW_RDMA_KVCACHE_PUSH_DC_')]:
            del os.environ[key]
        os.environ['SW_RDMA_KVCACHE_PUSH_DC'] = '0'
    return route


def run(config):
    import sys
    sys.path[:0] = [config['smoke']['helper_dir'], config['smoke']['native_dir']]
    from kv_smoke import configure
    configure(config['smoke'])  # the smoke task's exact RDMA environment, inherited by the workers
    push_pipes(config.get('pattern'))
    route = read_route(config)
    root = Path(config['root'])
    row_cfg = config.get('fault_row')  # stage 3: absent keeps the soak and kv_load byte for byte
    if row_cfg and row_cfg['kind'] == 'link_flap' and not (
            isinstance(row_cfg.get('recovery_s'), int) and not isinstance(row_cfg['recovery_s'], bool) and row_cfg['recovery_s'] > 0
            and all(isinstance(row_cfg.get(k), str) and row_cfg[k] for k in ('netdev', 'rdma_device'))):
        raise ValueError('link_flap row needs recovery_s, netdev and rdma_device')  # F3's window bound and receipt
    if row_cfg and row_cfg['kind'] == 'mr_pressure' and not (
            isinstance(row_cfg.get('recovery_s'), int) and not isinstance(row_cfg['recovery_s'], bool) and row_cfg['recovery_s'] > 0):
        raise ValueError('mr_pressure row needs recovery_s')  # F5's window bound (r26)
    flap_row = bool(row_cfg) and row_cfg['kind'] == 'link_flap'  # F3: late completions of abandoned futures
    # r26 F5 (plan v6): the ownership mode: phase-aware errors, the reuse barrier, poison, the late completions of
    # abandoned futures, and the one-deadline reap BEFORE the audit and the terminal claim
    pressure_row = bool(row_cfg) and row_cfg['kind'] == 'mr_pressure'
    late_row = flap_row or pressure_row
    files ={n: (root / n).open('x', buffering=1) for n in evidence_names(row_cfg)}
    ctx, workers, busy, commands, seeds, verify, available = mp.get_context('spawn'), {}, {}, {}, {}, [], {}
    oracle, rng, seq = Oracle(), random.Random(20261027), [0]
    counts = dict(acks=0, hits=0, misses=0, wrong_bytes=0, worker_errors=0)
    if row_cfg:
        counts['unknown_attributed'] = 0  # always present in a row's summary, zero included
    pat = config.get('pattern')  # kv_load's client pattern; absent, the soak's own mix
    clients, gap = (pat['clients'], 1 / pat['rate_per_client']) if pat else (4, .125)
    transport = (lambda i: pat['transport']) if pat else (lambda i: 'http' if i == 3 else route)
    pattern = Pattern(pat, rng, config['key_prefix']) if pat else None
    writers, key_writers, submissions, kills = {i: 0 for i in range(clients)}, {}, {}, []
    stop = threading.Event()
    window = dict(episode=None, acked=False, done=set(), allowed={})  # addendum r4
    bind = dict(run_id=config['smoke']['run_id'], boot_id=boot_id())
    # Stage 3: an armed window keeps the load running through the fault; every op dispatched or pending while
    # it is armed may end in a credited error, and in F1 an UNKNOWN PUT among them quarantines its key.
    armed_ops, ledger = set(), {}  # ledger: key -> survivor (highest ACK), UNKNOWN generations, quarantined
    flap = {}  # F3/F5: 'window' -> LinkFlapWindow/PressureWindow on M01 marks only, from the armed ack on, kept after the close
    timed_out = {}  # F3/F5: (worker, op) -> (action, key) for ops that hit the native deadline; consumed by one late row
    blocked, poisoned = set(), set()  # r26 F5: workers that take no op (an abandoned future outstanding; a raised submit)
    finals, exits, kills_sent, sched = {}, {}, {}, {}  # r26 F5: each child's final row, observed exit and kill; schedule

    def fault_window():
        """Pause: stop all dispatch, drain SETs (a GET error now is fatal: no
        ack, so no Kill), ack the pending GETs; resume only on the exact close."""
        if window['episode'] is None:
            for path in sorted(root.glob('fault-pause-*.json')):
                ep = path.name[len('fault-pause-'):-len('.json')]
                if ep in window['done'] or not re.fullmatch(r'[A-Za-z0-9_.-]{1,64}', ep):
                    continue
                row = read_bound(path, dict(bind, episode=ep))
                pm = row.get('pause_mono')
                if row.get('role') not in ('volume', 'master') or isinstance(pm, bool) or not isinstance(pm, (int, float)) \
                        or not math.isfinite(pm) or pm <= 0:
                    raise ValueError('fault pause malformed')
                observed = time.monotonic()  # captured once; a pause from the future is refused
                if pm > observed:
                    raise ValueError('fault pause dated after its observation')
                window.update(episode=ep, acked=False, pause=row, observed=observed, armed=bool(row_cfg), inflight_sent=False)
                window['done'].add(ep)
                if not row_cfg:
                    return
                break
        if window['episode'] is not None and window.get('armed') and not window['acked']:
            # A row window acknowledges at once, with its pending PUTs and GETs, and never drains or pauses.
            sets = [dict(op=c['operation_id'], submitted=c['dispatched']) for c in commands.values() if c['action'] == 'set']
            gets = [dict(op=c['operation_id'], submitted=c['dispatched']) for c in commands.values() if c['action'] == 'get']
            armed_ops.update(c['operation_id'] for c in commands.values())
            p = window['pause']
            window['ack_mono'] = time.monotonic()
            publish(root / ('fault-ack-%s.json' % window['episode']),
                    dict(bind, role=p['role'], episode=window['episode'], pause_mono=p['pause_mono'],
                         observed_mono=window['observed'], ack_mono=window['ack_mono'], inflight_sets=len(sets),
                         pending_gets=gets, pending_sets=sets, armed=True))
            window['acked'] = True
            log('op-events.jsonl', dict(event='fault_window', phase='armed', episode=window['episode'], mono=window['ack_mono']))
            if row_cfg['kind'] == 'link_flap' and window['episode'] == row_cfg['episode']:
                # start: the controller's pause mark, published before it issues the apply (same M01 clock)
                flap['window'] = LinkFlapWindow(p['pause_mono'], math.inf, (
                    ('run_id', bind['run_id']), ('boot_id', bind['boot_id']), ('episode', row_cfg['episode']),
                    ('kind', 'link_flap'), ('netdev', row_cfg['netdev']), ('rdma_device', row_cfg['rdma_device'])))
            if pressure_row and window['episode'] == row_cfg['episode']:
                # r26 F5: the same M01 marks bound the memlock-pressure window
                flap['window'] = PressureWindow(p['pause_mono'], math.inf, (
                    ('run_id', bind['run_id']), ('boot_id', bind['boot_id']), ('episode', row_cfg['episode']),
                    ('kind', 'mr_pressure')))
        elif window['episode'] is None:
            return
        elif not window['acked']:
            if any(c['action'] == 'set' for c in commands.values()):
                return  # still draining SETs
            pending = [dict(op=c['operation_id'], submitted=c['dispatched']) for c in commands.values()]
            window['allowed'].update((g['op'], window['episode']) for g in pending)
            p = window['pause']
            window['ack_mono'] = time.monotonic()
            publish(root / ('fault-ack-%s.json' % window['episode']),
                    dict(bind, role=p['role'], episode=window['episode'], pause_mono=p['pause_mono'],
                         observed_mono=window['observed'], ack_mono=window['ack_mono'], inflight_sets=0,
                         pending_gets=pending))
            window['acked'] = True
        else:
            ep, role = window['episode'], window['pause']['role']
            if window.get('armed') and not window.get('inflight_sent'):
                # F1 freeze-then-kill: the controller has SIGSTOPped the volume and published its controller clock
                # (this host's monotonic clock). Ops dispatched after it cannot have been served, so they are pending
                # by construction; answer once with them, as soon as there is at least one PUT and one GET.
                freeze = read_bound(root / ('fault-freeze-%s.json' % ep), dict(bind, episode=ep, role=role))
                if freeze is not None:
                    fm = freeze.get('freeze_mono')
                    sp, ss = freeze.get('stopped_pid'), freeze.get('stopped_starttime')
                    if isinstance(fm, bool) or not isinstance(fm, (int, float)) or not math.isfinite(fm) \
                            or fm < window['ack_mono'] or fm > time.monotonic() \
                            or any(isinstance(v, bool) or not isinstance(v, int) or v <= 0 for v in (sp, ss)):
                        raise ValueError('fault freeze malformed, before the ack or from the future')
                    after = lambda a: [dict(op=c['operation_id'], submitted=c['dispatched']) for c in commands.values()
                                       if c['action'] == a and c['dispatched'] > fm]
                    sets, gets = after('set'), after('get')
                    if sets and gets:
                        publish(root / ('fault-inflight-%s.json' % ep),
                                dict(bind, role=role, episode=ep, freeze_mono=fm, ack_mono=time.monotonic(),
                                     stopped_pid=sp, stopped_starttime=ss, pending_sets=sets, pending_gets=gets))
                        window['inflight_sent'] = True
            close = read_bound(root / ('fault-close-%s.json' % ep), dict(bind, episode=ep, role=role))
            if close is not None:
                end = close.get('end_mono')
                if isinstance(end, bool) or not isinstance(end, (int, float)) or not math.isfinite(end) or end < window['ack_mono']:
                    raise ValueError('fault close malformed or before the ack')
                if 'window' in flap and ep == row_cfg['episode']:
                    # end: the controller's close, published only after the restore proved restored=true, plus the
                    # row's recovery bound; a close from the future is refused like a pause from the future
                    if end > time.monotonic():
                        raise ValueError('fault close dated after its observation')
                    w = flap['window']
                    flap['window'] = type(w)(w.start, end + row_cfg['recovery_s'], w.binding)
                if window.get('armed'):
                    log('op-events.jsonl', dict(event='fault_window', phase='closed', episode=ep, mono=time.monotonic()))
                window.update(episode=None, armed=False)
    log = lambda name, row: files[name].write(json.dumps(row) + '\n')

    def start_worker(i, ready_by=None):
        """ready_by (r26 F5 audit worker): an absolute instant the ready wait is clamped to, instead of 45 s."""
        parent, remote = ctx.Pipe()
        cfg = dict(config['native'], root=str(root), stats_horizon_s=config['duration_s'] + config['quiescence_s'],
                   transport=transport(i))
        if late_row:  # F3 and F5 only: every other row's worker config is unchanged
            cfg['late_completions'] = True
        if pressure_row:  # r26 F5: phase-aware errors, the reuse barrier, poison, the final row at stop
            cfg['ownership'] = True
        p = ctx.Process(target=child, args=(remote, cfg))
        p.start()
        remote.close()
        workers[i] = dict(process=p, pipe=parent, identity=identity(p.pid))
        wait = 45 if ready_by is None else max(0, min(45, ready_by - time.monotonic()))
        if not parent.poll(wait) or parent.recv().get('event') != 'ready':
            raise RuntimeError('worker %s not ready' % i)

    def send(i, action, key, length, seed=None, verification=False):
        seq[0] += 1
        cmd = dict(action=action, key=key, seed=seed or seeds.get(key), length=length, operation_id=str(seq[0]),
                   dispatched=time.monotonic())
        if action == 'set':
            cmd['sequence'] = oracle.begin_write(key, payload(cmd['seed'], length)).sequence
        else:
            oracle.begin_read(key)
        busy[key] = busy.get(key, 0) + 1  # in-flight ops per key: kv_load reads of one key may overlap
        commands[i], available[i] = cmd, time.monotonic() + gap
        if window.get('armed'):
            armed_ops.add(cmd['operation_id'])
        workers[i]['pipe'].send(cmd)

    def ledger_row(i, cmd, state, digest, submitted, completed, error=None, attributed=None, ident=None, reason=None):
        """Stage 3 write contract: one terminal row per PUT (ACKED, REFUSED or UNKNOWN), frozen as it happens."""
        cand = dict(op=cmd['operation_id'], seed=cmd['seed'], length=cmd['length'], digest=digest, sequence=cmd['sequence'])
        ident = ident or workers[i]['identity']
        log('write-ledger.jsonl', dict(cand, key=cmd['key'], state=state, worker=i, dispatched=cmd['dispatched'],
                                       submitted=submitted, completed=completed, error=error, attributed=attributed,
                                       worker_identity=dict(pid=ident['pid'], starttime=ident['starttime']),
                                       **({} if reason is None else dict(reason=reason))))
        entry = ledger.setdefault(cmd['key'], dict(survivor=None, unknown=[], quarantined=False)) if state != 'REFUSED' else None
        if state == 'ACKED':
            entry['survivor'] = cand
        elif state == 'UNKNOWN':
            entry['unknown'].append(cand)

    def quarantine(i, cmd, submitted, completed, error, ident=None, reason=None):
        """An attributed UNKNOWN (F1, F2, F3): the key leaves the load for good, pinned busy so nothing selects it."""
        key = cmd['key']
        digest = oracle.pending[key].digest
        oracle.quarantine_write(key)
        busy[key] = busy.get(key, 0) + 1
        ledger_row(i, cmd, 'UNKNOWN', digest, submitted, completed, error, row_cfg['episode'], ident, reason)
        ledger[key]['quarantined'] = True
        counts['unknown_attributed'] += 1

    def put_failed(i, cmd, row, event):
        """REFUSED for a named definite error; in F1 an UNKNOWN among the armed ops quarantines; in F3 an UNKNOWN in
        the link flap's window (LinkFlapWindow, M01 marks) quarantines with its receipt; any other UNKNOWN stops the
        run as today."""
        error, key = str(row.get('error')), cmd['key']
        # r26 F5: generic text never certifies nothing-written (#1219): the row's textual REFUSED classes are not
        # consulted under memlock pressure; a matching PUT stays UNKNOWN and reaches the audit
        if not pressure_row and any(s in error for s in row_cfg['refused']):
            digest = oracle.pending[key].digest
            oracle.refuse_write(key)
            ledger_row(i, cmd, 'REFUSED', digest, row['submitted'], row['completed'], error)
            event['state'] = 'REFUSED'
        elif row_cfg['kind'] == 'volume_kill_io' and cmd['operation_id'] in armed_ops:
            quarantine(i, cmd, row['submitted'], row['completed'], error)
            event['state'] = 'UNKNOWN'
        elif row_cfg['kind'] == 'link_flap' and 'window' in flap and flap['window'].contains(row['submitted'], row['completed']):
            quarantine(i, cmd, row['submitted'], row['completed'], error,
                       reason=flap['window'].reason(row['submitted'], row['completed']))
            event['state'] = 'UNKNOWN'
        elif pressure_row and 'window' in flap and flap['window'].contains(row['submitted'], row['completed']):
            # r26 F5: a SETTLED failed PUT in the pressure window. Its error is generic text (rc -1 at the C ABI): nothing
            # certifies nothing-written (#1219), so it is an attributed UNKNOWN, quarantined and audited old-or-new;
            # never REFUSED, never retried. Its own per-op connector_failed_ops.set delta rides in the receipt.
            quarantine(i, cmd, row['submitted'], row['completed'], error, reason=dict(
                flap['window'].reason(row['submitted'], row['completed']), phase='complete', native_ok=bool(row['ok']),
                failed_delta=row.get('failed_delta')))
            event['state'] = 'UNKNOWN'
        else:
            ledger_row(i, cmd, 'UNKNOWN', oracle.pending[key].digest, row['submitted'], row['completed'], error)
            log('op-events.jsonl', dict(event, state='UNKNOWN'))
            oracle.abandon_write(key)  # raises UNKNOWN_WRITE: the run stops
        log('op-events.jsonl', event)

    def release(key):
        busy[key] -= 1
        if not busy[key]:
            del busy[key]

    def complete(i, row):
        cmd = commands.pop(i)
        key = cmd['key']
        log('completions.jsonl', dict(worker=i, op=cmd['operation_id'], action=cmd['action'], row=row))  # raw, BEFORE judging (r9 line 53)
        release(key)
        if row['operation_id'] != cmd['operation_id']:
            raise OracleError('completion identity')
        event = dict(worker=i, op=cmd['operation_id'], action=cmd['action'], key=key, dispatched=cmd['dispatched'], submitted=row['submitted'],
                     completed=row['completed'], ok=row['ok'], error=row.get('error'))
        if pressure_row:
            # r26 F5: the parent's receipt stamp (a SET's complete row is sent only after its ACK metadata check, so this
            # is the ACK instant recovery judges), and the worker's strict per-op counter delta and pid when present
            event['received'] = time.monotonic()
            event.update({k: row[k] for k in ('failed_delta', 'pid') if k in row})
        if cmd['action'] == 'set':
            if not row['ok'] or row['results'] not in (None, [True]):
                if row_cfg:
                    return put_failed(i, cmd, row, event)
                raise OracleError('failed SET ' + str(row.get('error')))  # any failed SET is fatal (r9)
            v = oracle.acknowledge(key, cmd['sequence'], cmd['length'], row['fids'])
            seeds[key] = cmd['seed']
            log('ack-journal.jsonl', dict(key=key, writer=i, seed=cmd['seed'], submitted=row['submitted'], completed=row['completed'], **vars(v)))
            if row_cfg:
                ledger_row(i, cmd, 'ACKED', v.digest, row['submitted'], row['completed'])
                event['state'] = 'ACKED'
            writers[i] += 1
            key_writers.setdefault(key, set()).add(i)
            counts['acks'] += 1
            verify.append(key)
        else:
            v = oracle.end_read(key)
            event.update(sequence=v.sequence, length=row.get('length'), sha256=row.get('sha256'))
            credited = cmd['operation_id'] in window['allowed'] or cmd['operation_id'] in armed_ops
            case = classify_get(row, credited and not pressure_row)  # r26 F5: no credit by error text
            if (case == 'unexpected' and row_cfg and credited and not pressure_row
                    and any(s in str(row.get('error')) for s in row_cfg['refused'])):
                case = 'fault'  # a named definite refusal inside the armed window
            if case == 'unexpected' and pressure_row and not row['ok'] and 'window' in flap \
                    and flap['window'].contains(row['submitted'], row['completed']):
                case = 'failed'  # r26 F5: a settled failed GET in the pressure window, judged by its counter, not text
            if case == 'hit':
                if not v.matches(row['length'], row['sha256']):
                    counts['wrong_bytes'] += 1
                    raise OracleError('WRONG_BYTES latest ACK')
                counts['hits'] += 1
            elif case == 'miss':
                event.update(miss=True, fids=list(v.fids), results=row['results'])  # judged in 3b by the decision-3 join
                if row.get('cause') == 'budget_refusal':  # the worker's counter delta: a K8 refusal miss
                    event['cause'] = 'budget_refusal'
                if row.get('unavailable') is True:  # #1187: the connector counted it volume-unavailable (a named miss)
                    event['unavailable'] = True
                counts['misses'] += 1
            elif case == 'fault':  # acked + r9 kind
                event.update(fault_error=True, episode=window['allowed'].get(cmd['operation_id']) or row_cfg['episode'])  # credited only by 3b, within [P, P+60]
            elif case == 'failed':
                # r26 F5: the assessor requires this op's failed_delta == 1 (or #1187's unavailable) and its buffer
                # untouched; the error text is kept raw and never classifies it
                event.update(state='failed', results=row['results'], untouched=row.get('untouched'),
                             **({'unavailable': True} if row.get('unavailable') is True else {}))
            else:
                raise OracleError('unexpected GET error ' + str(row.get('error')))
        log('op-events.jsonl', event)

    def unresolved(i, row):
        """r26 F5 (plan v4 C, v5 C1): an op that raised in its worker, judged by the phase it raised in and NEVER as a
        settled failure. submit: no fid, enqueue cannot be excluded: the worker is POISONED (no further op, no counter
        attribution). wait: the future missed its deadline: the worker is BLOCKED until it drains (its late completion
        is evidence only). ack: a settled ok=true SET whose ACK metadata was not verified. Inside the pressure window a
        PUT is an attributed UNKNOWN (quarantined, audited, never causal) and a GET is listed unresolved; outside it,
        either stops the run as today."""
        phase, cmd = row.get('phase'), commands[i]
        settled = row.get('row') if phase == 'ack' else None
        if phase not in ('submit', 'wait', 'ack') or (phase == 'ack') != (cmd['action'] == 'set' and isinstance(settled, dict)):
            counts['worker_errors'] += 1
            raise RuntimeError('worker error ' + repr(row)[:300])  # protocol (a duplicate or unowned completion): fatal
        received = time.monotonic()
        sub = submissions.pop(i, None)
        submitted = settled['submitted'] if settled else sub['monotonic'] if sub else cmd['dispatched']
        completed = settled['completed'] if settled else received
        commands.pop(i)
        log('completions.jsonl', dict(worker=i, op=cmd['operation_id'], action=cmd['action'], row=row))  # raw, BEFORE judging
        release(cmd['key'])
        if phase == 'submit':
            poisoned.add(i)
        elif phase == 'wait':
            blocked.add(i)
            timed_out[(i, cmd['operation_id'])] = (cmd['action'], cmd['key'])
        error = str(row.get('error'))
        event = dict(worker=i, op=cmd['operation_id'], action=cmd['action'], key=cmd['key'], dispatched=cmd['dispatched'],
                     submitted=submitted, completed=completed, ok=False, error=error, state='unresolved', phase=phase,
                     received=received)
        inside = 'window' in flap and flap['window'].contains(submitted, completed)
        if cmd['action'] == 'set':
            if not inside:
                ledger_row(i, cmd, 'UNKNOWN', oracle.pending[cmd['key']].digest, submitted, completed, error)
                log('op-events.jsonl', dict(event, state='UNKNOWN'))
                oracle.abandon_write(cmd['key'])  # raises UNKNOWN_WRITE: the run stops
            quarantine(i, cmd, submitted, completed, error, reason=dict(
                flap['window'].reason(submitted, completed), phase=phase, native_ok=True if phase == 'ack' else None,
                failed_delta=None))
            event['state'] = 'UNKNOWN'
        else:
            oracle.end_read(cmd['key'])
            if not inside:
                raise OracleError('unresolved GET outside the pressure window: ' + error[:200])
        log('op-events.jsonl', event)

    def pump(i):
        while workers[i]['pipe'].poll():
            row = workers[i]['pipe'].recv()
            if row['event'] == 'submitted':
                submissions[i] = row
            elif row['event'] == 'complete':
                complete(i, row['row'])
                submissions.pop(i, None)
            elif row['event'] == 'error' and pressure_row and i in commands and row.get('operation_id') == commands[i]['operation_id']:
                unresolved(i, row)
            elif row['event'] == 'unblocked' and pressure_row and i in blocked:
                blocked.discard(i)  # its abandoned futures drained: the next op's counter delta is its own again
                log('ownership.jsonl', dict(event='unblocked', child=str(i), pid=row.get('pid'), mono=row.get('mono'),
                                            received=time.monotonic()))
            elif row['event'] == 'orphan' and pressure_row and i in poisoned:
                log('ownership.jsonl', dict(event='orphan', child=str(i), fid=row.get('fid'), ok=row.get('ok'),
                                            error=row.get('error'), mono=row.get('mono')))  # evidence only, never attributed
            elif row['event'] == 'final' and pressure_row and i not in finals:
                finals[i] = dict(row, received=time.monotonic())
            elif row['event'] == 'late' and late_row and (i, row.get('operation_id')) in timed_out:
                # F3: the native completion of an op this worker abandoned at its deadline. Evidence only: the op
                # keeps its terminal state (a PUT stays UNKNOWN and quarantined; the audit decides old or new).
                action, key = timed_out.pop((i, row['operation_id']))
                entry = ledger.get(key) or {}
                log('late-completions.jsonl', dict(
                    run_id=bind['run_id'], episode=row_cfg['episode'], worker=i, worker_identity=workers[i]['identity'],
                    op=row['operation_id'], action=action, key=key, fid=row.get('fid'), ok=row.get('ok'),
                    error=row.get('error'), results=row.get('results'), worker_mono=row.get('mono'),
                    received_mono=time.monotonic(), ledger_state='UNKNOWN' if any(c['op'] == row['operation_id'] for c in entry.get('unknown', ())) else None,
                    quarantined=bool(entry.get('quarantined')) if action == 'set' else None))
            elif row['event'] == 'error' and row_cfg and i in commands and row.get('operation_id') == commands[i]['operation_id']:
                # Stage 3: an op that raised in its worker (a completion deadline, a lost connection) still ends in
                # exactly one terminal state, judged like a failed completion; outside a row it stays fatal.
                if flap_row and 'native completion deadline' in str(row.get('error')):
                    timed_out[(i, row['operation_id'])] = (commands[i]['action'], commands[i]['key'])
                sub = submissions.pop(i, None)
                complete(i, dict(operation_id=row['operation_id'], action=row.get('action'), ok=False, results=None,
                                 error=str(row.get('error')), completed=time.monotonic(),
                                 submitted=sub['monotonic'] if sub else commands[i]['dispatched']))
            elif row['event'] != 'stats':
                counts['worker_errors'] += 1
                raise RuntimeError('worker error ' + repr(row)[:300])

    def client_kill(started):
        """r9's rule: kill an RDMA (DC or RC) worker only while its GET is submitted and pending."""
        while time.monotonic() - started < 60:
            for i in (i for i in range(clients) if transport(i) in ('dc', 'rc')):
                pump(i)
                if i in commands and commands[i]['action'] == 'get' and i in submissions:
                    witness, cmd = submissions.pop(i), commands.pop(i)
                    oracle.end_read(cmd['key'])
                    release(cmd['key'])
                    killed = time.monotonic()
                    workers[i]['process'].kill()
                    workers[i]['process'].join(5)
                    if workers[i]['process'].is_alive():
                        raise RuntimeError('owned client survived kill')
                    start_worker(i)
                    return dict(kind='client_kill', worker=i, started=started, killed=killed, finished=time.monotonic(),
                                witness=witness, replacement=workers[i]['identity'])
            for i in range(clients):
                if i not in commands and oracle.latest:
                    key = next((k for k in oracle.latest if k not in busy), None)
                    if key:
                        send(i, 'get', key, oracle.latest[key].length)
            time.sleep(.001)
        raise TimeoutError('no submitted GET to kill within 60 s')

    def client_kill_put(started):
        """F2 (extends client_kill): kill a worker only while its PUT is submitted and pending: an RDMA worker when
        the load has one, else one of the load's own (an HTTP-only load). That PUT is UNKNOWN, attributed to the
        killed worker's pid/starttime, and its key is quarantined."""
        n, pool = 0, [i for i in range(clients) if transport(i) in ('dc', 'rc')] or list(range(clients))
        while time.monotonic() - started < 60:
            for i in pool:
                pump(i)
                if i in commands and commands[i]['action'] == 'set' and i in submissions:
                    witness, cmd = submissions.pop(i), commands.pop(i)
                    release(cmd['key'])
                    victim = dict(workers[i]['identity'])
                    killed = time.monotonic()
                    workers[i]['process'].kill()
                    workers[i]['process'].join(5)
                    if workers[i]['process'].is_alive():
                        raise RuntimeError('owned client survived kill')
                    finished = time.monotonic()
                    quarantine(i, cmd, witness['monotonic'], finished, 'client killed with the PUT pending', victim)
                    start_worker(i)
                    return dict(kind='client_kill_put', worker=i, started=started, killed=killed, finished=finished,
                                witness=witness, victim=dict(pid=victim['pid'], starttime=victim['starttime']),
                                replacement=workers[i]['identity'])
                if i not in commands:
                    n += 1
                    key = config['key_prefix'] + hashlib.sha256(('f2-key-%d' % n).encode()).hexdigest()
                    send(i, 'set', key, pat['object_bytes'] if pat else 1 << 20, seed='f2-generation-%d' % n)
            time.sleep(.001)
        raise TimeoutError('no submitted PUT to kill within 60 s')

    def audit_get(w, op, key, cand, until=None):
        """One GET of `cand` (its seed and length) by the fresh audit client; the assessor judges the bytes. until (r26
        F5): the audit deadline A the wait is clamped to."""
        cmd = dict(action='get', key=key, seed=cand['seed'], length=cand['length'], operation_id=op, dispatched=time.monotonic())
        w['pipe'].send(cmd)
        end = time.monotonic() + AUDIT_OP_S if until is None else min(time.monotonic() + AUDIT_OP_S, until)
        while time.monotonic() < end:
            if not w['pipe'].poll(max(0, end - time.monotonic())):
                break
            msg = w['pipe'].recv()
            if msg['event'] == 'complete':
                r = msg['row']
                return dict(op=op, seed=cand['seed'], length=r.get('length'), ok=r['ok'], results=r.get('results'),
                            sha256=r.get('sha256'), error=r.get('error'), untouched=r.get('untouched'),
                            case=classify_get(r, False), submitted=r['submitted'], completed=r['completed'])
            if msg['event'] == 'error':
                return dict(op=op, seed=cand['seed'], ok=False, error=str(msg.get('error')), case='unexpected')
        return None  # no answer within the op limit: the audit stops here, incomplete

    def audit(until=None):
        """Mandatory fresh audit (stage 3): after the load drained, a NEW client process reads every ledger key:
        its survivor, then, for a quarantined key, each UNKNOWN generation, stopping at the first exact match or
        miss. It records; the assessor decides. Past audit_s it stops and the row is NOT_COVERED. until (r26 F5): the
        absolute audit deadline A; it runs only after every load worker's exit was observed, and its ready wait and
        each GET wait are clamped to A."""
        if pressure_row and (until is None or any(i not in exits for i in range(clients))):
            raise RuntimeError('r26 F5: the fresh audit before every load worker exit was observed')
        deadline, n, complete_ = until if until is not None else time.monotonic() + row_cfg['audit_s'], 0, True
        try:
            start_worker('audit', ready_by=until)
        except RuntimeError:
            if until is None:
                raise
            w = workers['audit']  # never ready by A: no read, the audit is incomplete; the reap still owns the child
            log('op-events.jsonl', dict(event='audit_done', mono=time.monotonic(), keys=len(ledger), complete=False,
                                        client=dict(pid=w['identity']['pid'], starttime=w['identity']['starttime'])))
            return
        w = workers['audit']
        for key in sorted(ledger):
            entry, reads = ledger[key], []
            for cand in ([entry['survivor']] if entry['survivor'] else []) + (entry['unknown'] if entry['quarantined'] else []):
                n += 1
                r = audit_get(w, 'audit-%d' % n, key, cand, until) if time.monotonic() < deadline else None
                if r is None:
                    complete_ = False
                    break
                reads.append(r)
                if r['case'] == 'miss' or (r['case'] == 'hit' and r['sha256'] == cand['digest'] and r['length'] == cand['length']):
                    break
            if not complete_:
                break
            log('audit.jsonl', dict(key=key, reads=reads))
        log('op-events.jsonl', dict(event='audit_done', mono=time.monotonic(), keys=len(ledger), complete=complete_,
                                    client=dict(pid=w['identity']['pid'], starttime=w['identity']['starttime'])))

    stopped = set()

    def drain_child(k):
        """The reap's reader of one child: a load worker through pump (finals, late completions, unblocks, orphans);
        the audit child for its final row only (a GET answer after a clamped wait is discarded)."""
        if k != 'audit':
            return pump(k)
        while workers[k]['pipe'].poll():
            msg = workers[k]['pipe'].recv()
            if msg.get('event') == 'final' and k not in finals:
                finals[k] = dict(msg, received=time.monotonic())

    def reap_children(ids, kill_at, until):
        return reap(workers, ids, kill_at, until, exits, kills_sent, stopped, drain_child)

    def teardown():
        """r26 F5 (plan v5 C2, v6): settlement, final rows, joins and the OBSERVED exit of every child (the 4 load
        workers, then the audit worker) on ONE absolute schedule, BEFORE the fresh audit and any terminal claim. A child
        not observed gone by D is UNPROVEN: the row fails, and no terminal row or LOAD_DONE is written."""
        sched.update(reap_schedule(time.monotonic(), row_cfg['audit_s']))
        load = list(range(clients))
        reap_children(load, sched['K1'], sched['G'])
        gated = all(i in exits for i in load)
        if gated:
            audit(until=sched['A'])
        if 'audit' in workers:
            reap_children(['audit'], sched['K2'], sched['D'])
        reap_children(load, sched['K1'], sched['D'])  # load workers not seen gone by G: still owned until D
        unproven = sorted(str(k) for k in workers if k not in exits)
        for k, w in workers.items():
            f, x = finals.get(k) or {}, exits.get(k) or {}
            log('ownership.jsonl', dict(event='exit', child=str(k), pid=w['identity']['pid'], starttime=w['identity']['starttime'],
                                        final=k in finals, poisoned=f.get('poisoned'), abandoned_outstanding=f.get('abandoned_outstanding'),
                                        unsettled_buffers=f.get('unsettled_buffers'), final_mono=f.get('received'),
                                        exitcode=x.get('exitcode'), observed=x.get('mono'), killed=k in kills_sent,
                                        kill_mono=kills_sent.get(k)))
        log('ownership.jsonl', dict(event='teardown', schedule=dict(sched), gated=gated, audit='run' if gated else 'skipped',
                                    unproven=unproven, mono=time.monotonic()))
        if unproven or not gated:
            raise TeardownUnproven('r26 F5 teardown %s: no fresh audit verdict and no terminal row' % (
                'UNPROVEN, children still live at D: %s' % unproven if unproven else 'proven only after the audit gate G'))

    poller = threading.Thread(target=poll_topology, args=(config, stop, files['topology-polls.jsonl']), daemon=True)
    try:
        for i in range(clients):
            start_worker(i)
        poller.start()
        publish(root / 'workers.json', {str(i): dict(w['identity'], transport=transport(i)) for i, w in workers.items()})
        begin = time.monotonic()  # the witness is written before the first operation
        publish(root / WITNESS, dict(boot_id=boot_id(), mono=begin))
        end, pace, next_write, owner, cursor = begin + config['duration_s'], config['pacing_s'], begin, 0, 0
        while time.monotonic() < end:
            now = time.monotonic()
            for i in range(clients):
                if not workers[i]['process'].is_alive():
                    raise RuntimeError('worker exited unexpectedly')
                pump(i)
            fault_window()
            if window['episode'] is not None and not window.get('armed'):
                time.sleep(.002)
                continue  # paused: no new dispatch of any kind until the exact close
            if row_cfg and row_cfg['kind'] == 'client_kill_put' and not kills and now - begin >= row_cfg['at_s']:
                if pattern and pattern.phase == 0:
                    raise OracleError('kv_load client kill due before write_once ended')
                kills.append(client_kill_put(now))
                log('op-events.jsonl', dict(event='client_kill', **kills[-1]))
            if not row_cfg and not kills and now - begin >= config['client_kill_at_s'] and not any(c['action'] == 'set' for c in commands.values()):
                if pattern and pattern.phase == 0:  # the kill's own GET would precede the write barrier
                    raise OracleError('kv_load client kill due before write_once ended')
                kills.append(client_kill(now))
                log('op-events.jsonl', dict(event='client_kill', **kills[-1]))
            for i in range(clients):
                if i in commands or now < available.get(i, 0) or i in blocked or i in poisoned:
                    continue  # r26 F5: a blocked or poisoned worker takes no op (the reuse barrier)
                # kv_load's write_once phase reads nothing, not even verification, until every write is ACKed
                key = None if pattern and pattern.phase == 0 else next((k for k in verify if k not in busy), None)
                if key is not None:
                    verify.remove(key)
                    send(i, 'get', key, oracle.latest[key].length, verification=True)
                elif pattern:
                    op = pattern.next(now, now < end - config['write_stop_s'], oracle.pending, oracle.latest, busy)
                    if op and op[0] == 'set':
                        send(i, 'set', op[1], pat['object_bytes'], seed=op[2])
                    elif op:
                        send(i, 'get', op[1], oracle.latest[op[1]].length)
                elif i == owner and now >= next_write and now < end - config['write_stop_s']:
                    cursor += 1
                    slot = rng.randrange(32) if cursor % 7 else 32 + (cursor // 7) % 480
                    key = config['key_prefix'] + hashlib.sha256(('soak-key-%d' % slot).encode()).hexdigest()
                    if key not in busy:
                        length = (4 << 20 if cursor % 64 == 0 else 1 << 20) + (17 if cursor % 31 == 0 else 0)
                        send(i, 'set', key, length, seed='soak-generation-%d' % cursor)
                        owner, next_write = (owner + 1) % 4, max(next_write + pace, now)
                elif oracle.latest:
                    key = next((k for k in rng.sample(list(oracle.latest), len(oracle.latest)) if k not in busy), None)
                    if key:
                        send(i, 'get', key, oracle.latest[key].length)
            time.sleep(.002)
        body_end = time.monotonic()
        if pattern and not pattern.finished(body_end):
            raise OracleError('kv_load phases did not complete within the load')
        log('op-events.jsonl', dict(event='body_end', mono=body_end))
        while time.monotonic() < end + config['quiescence_s']:  # drain, admit nothing
            for i in range(clients):
                pump(i)
            time.sleep(.02)
        if commands or oracle.pending or oracle.reading:
            raise OracleError('unfinished operations after quiescence')
        if pressure_row:
            teardown()  # r26 F5: every child's exit observed, then the audit inside it, before the terminal row
        elif row_cfg:
            audit()
        log('op-events.jsonl', dict(event='quiescent', mono=time.monotonic(), dispatched=seq[0]))  # the terminal row
        return dict(status='LOAD_DONE', counts=counts, writers=writers, kills=kills, body_start=begin, body_end=body_end,
                    shared_keys_all_four=sum(len(w) == 4 for w in key_writers.values()),
                    witness=dict(boot_id=boot_id(), mono=begin))
    finally:
        stop.set()
        for w in workers.values():
            if w['process'].is_alive():
                if sched:  # r26 F5: past the schedule nothing waits beyond E (an unproven child stays unproven)
                    w['process'].kill()
                    w['process'].join(max(0, sched['E'] - time.monotonic()))
                    continue
                w['pipe'].send(dict(action='stop'))
                w['process'].join(5)
                if w['process'].is_alive():
                    w['process'].kill()
                    w['process'].join(3)
        poller.join(max(0, sched['E'] - time.monotonic()) if sched else 5)
        for f in files.values():
            f.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', required=True)
    config = json.loads(parser.parse_args().config)
    try:
        summary = run(config)
    except TeardownUnproven:
        # r26 correction: run() has already closed its evidence (the teardown record, unproven children included, is in
        # ownership.jsonl) and bounded every join by E. A normal interpreter exit would now join each still-live
        # multiprocessing child without any bound, and any stdout/stderr write or flush can block on a full pipe: leave
        # the process at once, with no I/O at all on this path.
        os._exit(TEARDOWN_EXIT)
    root = Path(config['root'])
    summary['evidence'] = {n: dict(sha256=hashlib.sha256((root / n).read_bytes()).hexdigest(), bytes=(root / n).stat().st_size)
                           for n in evidence_names(config.get('fault_row'))
                           + tuple(sorted(p.name for p in root.glob('native-stats-*.jsonl')))}
    print(json.dumps(summary, sort_keys=True), flush=True)


if __name__ == '__main__':
    main()
