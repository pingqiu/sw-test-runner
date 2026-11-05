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
from soak_oracle import Oracle, OracleError
from soak_stats import WITNESS, boot_id, identity

# NOT_FOUND (status 1) in exactly the two spellings a soak read can carry: the DC batch push read, and the RC
# push-read slot fetch (#999, sw-rdma push_read.rs), whose full message is required so no other status or push form
# (status 10, bykey, direct, kv slab, the unbatched DC) can pass as a miss.
MISS = re.compile(r'DC batch push-read response status 1(?:\D|$)'
                  r'|(?<!DC )(?<!batch )push-read response status 1 pipe=\d+ request_id=\d+ fid=\S+ offset=\d+ length=\d+ slot=\d+')
TRANSIENT = re.compile(r'timeout|timed out|reset|unavailable|refused|closed|broken pipe', re.I)  # r9's fault kinds
EVIDENCE = ('ack-journal.jsonl', 'op-events.jsonl', 'topology-polls.jsonl', 'completions.jsonl')


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
    files = {n: (root / n).open('x', buffering=1) for n in EVIDENCE}
    ctx, workers, busy, commands, seeds, verify, available = mp.get_context('spawn'), {}, {}, {}, {}, [], {}
    oracle, rng, seq = Oracle(), random.Random(20261027), [0]
    counts = dict(acks=0, hits=0, misses=0, wrong_bytes=0, worker_errors=0)
    pat = config.get('pattern')  # kv_load's client pattern; absent, the soak's own mix
    clients, gap = (pat['clients'], 1 / pat['rate_per_client']) if pat else (4, .125)
    transport = (lambda i: pat['transport']) if pat else (lambda i: 'http' if i == 3 else route)
    pattern = Pattern(pat, rng, config['key_prefix']) if pat else None
    writers, key_writers, submissions, kills = {i: 0 for i in range(clients)}, {}, {}, []
    stop = threading.Event()
    window = dict(episode=None, acked=False, done=set(), allowed={})  # addendum r4
    bind = dict(run_id=config['smoke']['run_id'], boot_id=boot_id())

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
                window.update(episode=ep, acked=False, pause=row, observed=observed)
                window['done'].add(ep)
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
            close = read_bound(root / ('fault-close-%s.json' % ep), dict(bind, episode=ep, role=role))
            if close is not None:
                end = close.get('end_mono')
                if isinstance(end, bool) or not isinstance(end, (int, float)) or not math.isfinite(end) or end < window['ack_mono']:
                    raise ValueError('fault close malformed or before the ack')
                window['episode'] = None
    log = lambda name, row: files[name].write(json.dumps(row) + '\n')

    def start_worker(i):
        parent, remote = ctx.Pipe()
        cfg = dict(config['native'], root=str(root), stats_horizon_s=config['duration_s'] + config['quiescence_s'],
                   transport=transport(i))
        p = ctx.Process(target=child, args=(remote, cfg))
        p.start()
        remote.close()
        workers[i] = dict(process=p, pipe=parent, identity=identity(p.pid))
        if not parent.poll(45) or parent.recv().get('event') != 'ready':
            raise RuntimeError('worker %d not ready' % i)

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
        workers[i]['pipe'].send(cmd)

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
        if cmd['action'] == 'set':
            if not row['ok'] or row['results'] not in (None, [True]):
                raise OracleError('failed SET ' + str(row.get('error')))  # any failed SET is fatal (r9)
            v = oracle.acknowledge(key, cmd['sequence'], cmd['length'], row['fids'])
            seeds[key] = cmd['seed']
            log('ack-journal.jsonl', dict(key=key, writer=i, seed=cmd['seed'], submitted=row['submitted'], completed=row['completed'], **vars(v)))
            writers[i] += 1
            key_writers.setdefault(key, set()).add(i)
            counts['acks'] += 1
            verify.append(key)
        else:
            v = oracle.end_read(key)
            event.update(sequence=v.sequence, length=row.get('length'), sha256=row.get('sha256'))
            case = classify_get(row, cmd['operation_id'] in window['allowed'])
            if case == 'hit':
                if not v.matches(row['length'], row['sha256']):
                    counts['wrong_bytes'] += 1
                    raise OracleError('WRONG_BYTES latest ACK')
                counts['hits'] += 1
            elif case == 'miss':
                event.update(miss=True, fids=list(v.fids), results=row['results'])  # judged in 3b by the decision-3 join
                if row.get('cause') == 'budget_refusal':  # the worker's counter delta: a K8 refusal miss
                    event['cause'] = 'budget_refusal'
                counts['misses'] += 1
            elif case == 'fault':  # acked + r9 kind
                event.update(fault_error=True, episode=window['allowed'][cmd['operation_id']])  # credited only by 3b, within [P, P+60]
            else:
                raise OracleError('unexpected GET error ' + str(row.get('error')))
        log('op-events.jsonl', event)

    def pump(i):
        while workers[i]['pipe'].poll():
            row = workers[i]['pipe'].recv()
            if row['event'] == 'submitted':
                submissions[i] = row
            elif row['event'] == 'complete':
                complete(i, row['row'])
                submissions.pop(i, None)
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
            if window['episode'] is not None:
                time.sleep(.002)
                continue  # paused: no new dispatch of any kind until the exact close
            if not kills and now - begin >= config['client_kill_at_s'] and not any(c['action'] == 'set' for c in commands.values()):
                if pattern and pattern.phase == 0:  # the kill's own GET would precede the write barrier
                    raise OracleError('kv_load client kill due before write_once ended')
                kills.append(client_kill(now))
                log('op-events.jsonl', dict(event='client_kill', **kills[-1]))
            for i in range(clients):
                if i in commands or now < available.get(i, 0):
                    continue
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
        log('op-events.jsonl', dict(event='quiescent', mono=time.monotonic(), dispatched=seq[0]))  # the terminal row
        return dict(status='LOAD_DONE', counts=counts, writers=writers, kills=kills, body_start=begin, body_end=body_end,
                    shared_keys_all_four=sum(len(w) == 4 for w in key_writers.values()),
                    witness=dict(boot_id=boot_id(), mono=begin))
    finally:
        stop.set()
        for w in workers.values():
            if w['process'].is_alive():
                w['pipe'].send(dict(action='stop'))
                w['process'].join(5)
                if w['process'].is_alive():
                    w['process'].kill()
                    w['process'].join(3)
        poller.join(5)
        for f in files.values():
            f.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', required=True)
    config = json.loads(parser.parse_args().config)
    summary = run(config)
    root = Path(config['root'])
    summary['evidence'] = {n: dict(sha256=hashlib.sha256((root / n).read_bytes()).hexdigest(), bytes=(root / n).stat().st_size)
                           for n in EVIDENCE + tuple(sorted(p.name for p in root.glob('native-stats-*.jsonl')))}
    print(json.dumps(summary, sort_keys=True), flush=True)


if __name__ == '__main__':
    main()
