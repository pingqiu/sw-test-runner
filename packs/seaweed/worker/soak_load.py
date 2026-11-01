"""M01 KV soak load (step 3a): the r9 workload loop without remote shells, fixed hosts
or a control handshake. Four native workers (3 DC + 1 HTTP), the oracle,
read-after-ACK, the client kill, a read-only topology poller (decision 3),
bounded evidence files and one bounded summary row on stdout."""
import argparse, hashlib, json, multiprocessing as mp, os, random, re, threading, time, urllib.request
from pathlib import Path
from soak_native_worker import child, payload
from soak_oracle import Oracle, OracleError
from soak_stats import WITNESS, boot_id, identity, process_sample

MISS = re.compile(r'DC batch push-read response status 1(?:\D|$)')
EVIDENCE = ('ack-journal.jsonl', 'op-events.jsonl', 'topology-polls.jsonl', 'client-series.jsonl')


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


def run(config):
    import sys
    sys.path[:0] = [config['smoke']['helper_dir'], config['smoke']['native_dir']]
    from kv_smoke import configure
    configure(config['smoke'])  # the smoke task's exact RDMA environment, inherited by the workers
    root = Path(config['root'])
    files = {n: (root / n).open('x', buffering=1) for n in EVIDENCE}
    ctx, workers, busy, commands, seeds, verify, available = mp.get_context('spawn'), {}, {}, {}, {}, [], {}
    oracle, rng, seq = Oracle(), random.Random(20261027), [0]
    counts = dict(acks=0, hits=0, misses=0, wrong_bytes=0, worker_errors=0)
    writers, key_writers, submissions, kills = {i: 0 for i in range(4)}, {}, {}, []
    stop = threading.Event()
    log = lambda name, row: files[name].write(json.dumps(row) + '\n')

    def start_worker(i):
        parent, remote = ctx.Pipe()
        cfg = dict(config['native'], root=str(root), stats_horizon_s=config['duration_s'] + config['quiescence_s'],
                   transport='http' if i == 3 else 'dc')
        p = ctx.Process(target=child, args=(remote, cfg))
        p.start()
        remote.close()
        workers[i] = dict(process=p, pipe=parent, identity=identity(p.pid))
        if not parent.poll(45) or parent.recv().get('event') != 'ready':
            raise RuntimeError('worker %d not ready' % i)

    def send(i, action, key, length, seed=None, verification=False):
        seq[0] += 1
        cmd = dict(action=action, key=key, seed=seed or seeds.get(key), length=length, operation_id=str(seq[0]))
        if action == 'set':
            cmd['sequence'] = oracle.begin_write(key, payload(cmd['seed'], length)).sequence
        else:
            oracle.begin_read(key)
        busy[key], commands[i], available[i] = i, cmd, time.monotonic() + .125
        workers[i]['pipe'].send(cmd)

    def complete(i, row):
        cmd = commands.pop(i)
        key = cmd['key']
        busy.pop(key)
        if row['operation_id'] != cmd['operation_id']:
            raise OracleError('completion identity')
        event = dict(worker=i, op=cmd['operation_id'], action=cmd['action'], key=key, submitted=row['submitted'],
                     completed=row['completed'], ok=row['ok'], error=row.get('error'))
        if cmd['action'] == 'set':
            if not row['ok'] or row['results'] not in (None, [True]):
                raise OracleError('failed SET ' + str(row.get('error')))  # any failed SET is fatal (r9)
            v = oracle.acknowledge(key, cmd['sequence'], cmd['length'], row['fids'])
            seeds[key] = cmd['seed']
            log('ack-journal.jsonl', dict(key=key, writer=i, submitted=row['submitted'], completed=row['completed'], **vars(v)))
            writers[i] += 1
            key_writers.setdefault(key, set()).add(i)
            counts['acks'] += 1
            verify.append(key)
        else:
            v = oracle.reading.pop(key)
            event.update(sequence=v.sequence, length=row.get('length'), sha256=row.get('sha256'))
            if row['ok'] and row['results'] in (None, [True]):
                if row['length'] != v.length or row['sha256'] != v.digest:
                    counts['wrong_bytes'] += 1
                    raise OracleError('WRONG_BYTES latest ACK')
                counts['hits'] += 1
            elif row['results'] == [False] and row.get('untouched') is True and MISS.search(row.get('error') or ''):
                event.update(miss=True, fids=list(v.fids))  # judged in 3b by the decision-3 join
                counts['misses'] += 1
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
        """r9's rule: kill a DC worker only while its GET is submitted and pending."""
        while time.monotonic() - started < 60:
            for i in (0, 1, 2):
                pump(i)
                if i in commands and commands[i]['action'] == 'get' and i in submissions:
                    witness, cmd = submissions.pop(i), commands.pop(i)
                    oracle.reading.pop(cmd['key'])
                    busy.pop(cmd['key'])
                    workers[i]['process'].kill()
                    workers[i]['process'].join(5)
                    if workers[i]['process'].is_alive():
                        raise RuntimeError('owned client survived kill')
                    start_worker(i)
                    return dict(kind='client_kill', worker=i, started=started, finished=time.monotonic(),
                                witness=witness, replacement=workers[i]['identity'])
            for i in range(4):
                if i not in commands and oracle.latest:
                    key = next((k for k in oracle.latest if k not in busy), None)
                    if key:
                        send(i, 'get', key, oracle.latest[key].length)
            time.sleep(.001)
        raise TimeoutError('no submitted GET to kill within 60 s')

    poller = threading.Thread(target=poll_topology, args=(config, stop, files['topology-polls.jsonl']), daemon=True)
    try:
        for i in range(4):
            start_worker(i)
        poller.start()
        begin = time.monotonic()  # the witness is written before the first operation
        with (root / WITNESS).open('x') as w:
            json.dump(dict(boot_id=boot_id(), mono=begin), w)
        end, pace, next_write, owner, cursor, next_sample = begin + config['duration_s'], config['pacing_s'], begin, 0, 0, begin
        while time.monotonic() < end:
            now = time.monotonic()
            for i in range(4):
                if not workers[i]['process'].is_alive():
                    raise RuntimeError('worker exited unexpectedly')
                pump(i)
            if not kills and now - begin >= config['client_kill_at_s'] and not any(c['action'] == 'set' for c in commands.values()):
                kills.append(client_kill(now))
            if now >= next_sample:
                log('client-series.jsonl', dict(mono=now, processes=[dict(role='parent', **process_sample(os.getpid(), identity(os.getpid())['starttime']))]
                    + [dict(role='client-%d' % i, **process_sample(w['identity']['pid'], w['identity']['starttime'])) for i, w in workers.items()]))
                next_sample += 30
            for i in range(4):
                if i in commands or now < available.get(i, 0):
                    continue
                key = next((k for k in verify if k not in busy), None)
                if key is not None:
                    verify.remove(key)
                    send(i, 'get', key, oracle.latest[key].length, verification=True)
                elif i == owner and now >= next_write and now < end - 30:
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
        while time.monotonic() < end + config['quiescence_s']:  # drain, admit nothing
            for i in range(4):
                pump(i)
            time.sleep(.02)
        if commands or oracle.pending or oracle.reading:
            raise OracleError('unfinished operations after quiescence')
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
