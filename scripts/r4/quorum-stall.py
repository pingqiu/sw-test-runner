#!/usr/bin/env python3
"""V1.0.1 sync-quorum guest measurements; never changes product authority."""
import argparse,json,math,os,signal,subprocess,sys,time,stat,hashlib,urllib.request,threading
from pathlib import Path

def require(ok,message):
    if not ok:raise ValueError(message)

def append(path,row):
    with path.open('a') as f:
        f.write(json.dumps(row,separators=(',',':'))+'\n');f.flush();os.fsync(f.fileno())

def load(path,live=False):
    raw=path.read_bytes()
    if live and raw and not raw.endswith(b'\n'):raw=raw.rsplit(b'\n',1)[0] if b'\n' in raw else b''
    return [json.loads(line) for line in raw.splitlines()]

def budgets(a):
    require(a.write_budget_ms == 5000 and a.resume_budget_ms == 10000
            and a.budget_ref.strip(), 'approved policy requires 5000 ms ACK gap and 10000 ms RF2 resume')


def peer_state(payload, replica):
    require(payload.get('VolumeID') == 'v1' and payload.get('ReplicaID') == 'r1', 'foreign primary peer view')
    matches = [p for p in payload.get('Peers', []) if p.get('ReplicaID') == replica]
    require(len(matches) == 1 and matches[0].get('Epoch') == 1, 'missing or foreign epoch in peer view')
    state = matches[0].get('State')
    require(state in ('healthy', 'degraded', 'catching_up', 'needs_rebuild', 'unknown'), 'unknown peer view state')
    return state


def peer_window(a, phases, meta=None):
    rows = load(a.work/'peers.jsonl')
    require(len(rows) >= 2, 'missing primary peer observations')
    replica = 'r3' if a.rf == '3' else 'r2'
    down = healthy = None
    previous = None
    for row in rows:
        require('error' not in row, 'primary peer observation failed')
        primary(row['primary'])
        if meta is not None:
            require(row['boot'] == meta['boot'], 'peer observation boot differs from writer')
        at = row['at_ns']
        if previous is not None:
            require(0 < at-previous <= 250000000, 'primary peer observation gap exceeds 250 ms')
        previous = at
        state = peer_state(row['peers'], replica)
        if down is None and state != 'healthy':
            require(row is not rows[0], 'peer was not Healthy at baseline')
            down = at
        elif down is not None and healthy is None and state == 'healthy':
            healthy = at
        elif healthy is not None:
            require(state == 'healthy', 'peer went down again after return')
    require(rows[0]['at_ns'] <= phases['baseline'] and rows[-1]['at_ns'] >= phases['stop'], 'peer observations do not cover the measured window')
    require(down is not None and healthy is not None, 'no primary-observed down-to-Healthy interval')
    require(phases['baseline'] < down < healthy < phases['stop'], 'peer interval outside measurement')
    require(healthy >= phases['recover'], 'peer Healthy before process start request')
    # recover is recorded on the guest BEFORE the remote process start. This
    # conservative upper bound can fail late but cannot grant extra time.
    if a.rf == '3':
        require((healthy-phases['recover'])/1e9 <= 120, 'peer did not become Healthy within 120 s of process start request')
    return down, healthy


def judge(a):
    budgets(a)
    meta = None
    if a.require_block_device:
        meta=json.loads((a.work/'writer-meta.json').read_text())
        require(meta['block_device'] and not meta['file_control'] and not meta['dd_override'],'file/mock device cannot count as qualification')
        require((a.work/'writer-stopped.json').exists(),'writer has not stopped')
    phases={}
    for row in load(a.work/'phases.jsonl'):
        require(row['phase'] not in phases,'duplicate phase')
        phases[row['phase']]=row['at_ns']
    required=['baseline','fault-pending','fault','recover','after','stop']
    require(all(k in phases for k in required),'missing phase')
    require(all(phases[x]<phases[y] for x,y in zip(required,required[1:])),'phase order')
    open_ops={};seen=set();ends=[];durations=[]
    for row in load(a.work/'io.jsonl'):
        op=row['op']
        if row['kind']=='begin':
            require(op not in seen,'duplicate operation');require(not open_ops,'overlapping single-flight writes');seen.add(op);open_ops[op]=row
        else:
            require(row['kind']=='end' and op in open_ops,'end without begin')
            start=open_ops.pop(op);require(row['at_ns']>=start['at_ns'],'operation clock order')
            row['start_ns']=start['at_ns'];ends.append(row)
            durations.append((row['at_ns']-start['at_ns'])/1e6)
    require(not open_ops,'unfinished write: pending time cannot disappear')
    if a.require_block_device:
        facts={};acked=set()
        for line in (a.work/'ledger').read_text().splitlines():
            kind,op,round_,offset,length,sha=line.split();fact=(round_,offset,length,sha)
            if kind=='intent':require(op not in facts,'duplicate ledger intent');facts[op]=fact
            else:
                require(kind=='ack' and op in facts and facts[op]==fact and op not in acked,'unmatched ledger ACK');acked.add(op)
        require(set(facts)==seen and acked=={r['op'] for r in ends if r['ok']},'timing records do not cover every attempted and ACKed write')
        for row in load(a.work/'phases.jsonl'):
            require(row['boot']==meta['boot'],'phase boot differs from writer');primary(row['primary'])
        require(next(row for row in load(a.work/'phases.jsonl') if row['phase']=='fault')['fault_key'].startswith('node-3/'),'fault must name the replica VM')
    require(ends and durations,'no attempts')
    acks=sorted(row['at_ns'] for row in ends if row['ok'])
    require(any(t<phases['fault'] for t in acks) and any(t>=phases['after'] for t in acks),'missing baseline or recovered ACK')
    down, healthy = peer_window(a, phases, meta)
    fault_ops = [r for r in ends if down <= r['at_ns'] < healthy]
    require(any(r['start_ns'] < healthy and r['at_ns'] >= down for r in ends),
            'writer made no attempt through primary-observed peer outage')
    errors = [r for r in ends if not r['ok']]
    if a.rf == '2':
        require(not any(r['ok'] for r in fault_ops), 'RF2 ACKed while the primary reports its only peer down')
        require(not any(not down <= r['at_ns'] < healthy for r in errors), 'guest I/O errors outside primary-observed outage')
    else:
        require(not errors, 'RF3 guest I/O errors')
    max_write = max(durations)  # observation only; the ruled stall metric is ACK gap
    def gap(lo, hi):
        points = [lo] + [t for t in acks if lo < t < hi] + [hi]
        return max((b-a)/1e6 for a,b in zip(points, points[1:]))
    global_gap = gap(phases['baseline'], phases['stop'])
    recovered = [t for t in acks if t >= healthy]
    require(recovered, 'no ACK after peer Healthy')
    resume = (recovered[0]-healthy)/1e6
    if a.rf == '2':
        require(resume <= 10000, 'RF2 resume exceeds 10 s after Healthy')
        # The separately ruled ten-second first-ACK allowance is explicit;
        # subsequent progress and all pre-down progress keep the five-second bar.
        bounded_gap = max(gap(phases['baseline'], down), gap(recovered[0], phases['stop']))
    else:
        bounded_gap = global_gap
    require(bounded_gap < 5000, 'ACK-gap must be strictly less than 5 s')
    ordered=sorted(durations)
    return dict(timing_result='PASS',rf=a.rf,ack_mode='sync-quorum',attempts=len(ends),acks=len(acks),
        refused=sum(not r['ok'] for r in ends),max_write_ms=max_write,p50_write_ms=ordered[(len(ordered)-1)//2],
        p99_write_ms=ordered[math.ceil(.99*len(ordered))-1],max_ack_gap_ms=global_gap,
        bounded_ack_gap_ms=bounded_gap,resume_ms=resume,write_budget_ms=a.write_budget_ms,
        resume_budget_ms=a.resume_budget_ms,budget_ref=a.budget_ref,
        peer_down_ns=down,peer_healthy_ns=healthy,peer_recovery_upper_ms=(healthy-phases['recover'])/1e6,
        expected_outage_errors=len(errors) if a.rf=='2' else 0,rf2_outage_excluded_from_ack_gap_budget=(a.rf=='2'),data_readback='separate mandatory scenario step')

def boot():
    path=Path('/proc/sys/kernel/random/boot_id')
    return path.read_text().strip() if path.exists() else 'file-control-only'

def publish(path,row):
    temporary=path.with_suffix('.tmp');temporary.write_text(json.dumps(row)+'\n');os.replace(temporary,path)

def process_start(pid):
    path=Path('/proc')/str(pid)/'stat'
    if not path.exists():return None
    fields=path.read_text().rsplit(')',1)[1].split()
    return fields[19] if fields[0]!='Z' else None

def writer_live(work):
    meta=json.loads((work/'writer-meta.json').read_text())
    return meta.get('start') is not None and process_start(meta['pid'])==meta['start']

def status(url):
    with urllib.request.urlopen(url,timeout=1) as response:return json.load(response)

def primary(payload):
    require(payload.get('VolumeID')=='v1' and payload.get('ReplicaID')=='r1'
            and payload.get('Epoch')==1 and payload.get('AuthorityRole')=='primary','primary identity changed unexpectedly')

def mark(a):
    require(writer_live(a.work) if a.phase!='stop' else (a.work/'writer-stopped.json').exists() and not writer_live(a.work),
            'phase is not bound to the expected writer lifecycle')
    meta=json.loads((a.work/'writer-meta.json').read_text());require(meta['boot']==boot(),'guest boot changed')
    observed=status(a.status);primary(observed)
    row=dict(phase=a.phase,at_ns=time.monotonic_ns(),boot=boot(),primary=observed)
    if a.phase=='fault':
        require(a.fault_key and a.fault_key.startswith('node-3/'),'missing actual replica fault binding')
        row['fault_key']=a.fault_key
    if a.phase == 'stop':
        require(a.peers, 'final primary peer receipt required')
        peers = status(a.peers)
        append(a.work/'peers.jsonl', dict(at_ns=time.monotonic_ns(), boot=boot(), primary=observed, peers=peers))
    append(a.work/'phases.jsonl',row)
    return row

def wait_acks(a):
    end=time.monotonic()+a.seconds
    since=0
    if a.since:
        since=next(r['at_ns'] for r in load(a.work/'phases.jsonl') if r['phase']==a.since)
    while time.monotonic()<end:
        if not (a.work/'writer-meta.json').exists():
            time.sleep(.1);continue
        require(writer_live(a.work),'writer ended before the observation window')
        rows=load(a.work/'io.jsonl',live=True) if (a.work/'io.jsonl').exists() else []
        count=sum(r['kind']=='end' and r['ok'] and r['at_ns']>=since for r in rows)
        if count>=a.minimum:return dict(acks=count,since_ns=since)
        time.sleep(.1)
    raise ValueError('no bounded ACK progress')

def ready_peer(a):
    end = time.monotonic()+a.seconds
    while time.monotonic() < end:
        own = status(a.status)
        primary(own)
        peers = status(a.peers)
        if peer_state(peers, a.replica) == 'healthy':
            return dict(primary=own, peers=peers)
        time.sleep(.1)
    raise ValueError('peer was not Healthy before the baseline writer')


def wait_peer(a):
    # Same primary-view journal as the judge: no second state predicate or
    # independently chosen fault interval. The writer owns ongoing sampling.
    end = time.monotonic()+a.seconds
    while time.monotonic() < end:
        require(writer_live(a.work), 'writer ended before peer recovery')
        if (a.work/'peers.jsonl').exists():
            rows = load(a.work/'peers.jsonl', live=True)
            if rows:
                last = rows[-1]
                require('error' not in last, 'primary peer observation failed')
                state = peer_state(last['peers'], a.replica)
                seen_down = any('error' not in r and peer_state(r['peers'], a.replica) != 'healthy' for r in rows)
                if seen_down and state == 'healthy':
                    publish(a.output, last)
                    return last
        time.sleep(.1)
    raise ValueError('returning replica never became Healthy on the primary')


def observe_peers(a, stopping):
    while not stopping.is_set():
        row = dict(at_ns=time.monotonic_ns(), boot=boot())
        try:
            row['primary'] = status(a.status)
            primary(row['primary'])
            row['peers'] = status(a.peers)
            peer_state(row['peers'], a.replica)
            row['at_ns'] = time.monotonic_ns()
        except Exception as error:
            row['error'] = str(error)
        append(a.work/'peers.jsonl', row)
        stopping.wait(.05)


def stop_writer(a):
    require(writer_live(a.work),'writer was not active at stop request')
    publish(a.work/'stop-request.json',dict(at_ns=time.monotonic_ns()))
    end=time.monotonic()+a.seconds
    while time.monotonic()<end:
        if (a.work/'writer-stopped.json').exists() and not writer_live(a.work):return dict(stopped=True)
        time.sleep(.1)
    raise ValueError('writer outcome unknown: no readback or timing PASS permitted')

def writer(a):
    require(not a.work.exists(),'fresh writer directory required')
    require(a.pace_ms>0 and a.max_ops>=0,'invalid workload bounds')
    block_device=stat.S_ISBLK(a.device.stat().st_mode)
    require(block_device or a.file_control,'real block device required')
    require(a.file_control or not os.environ.get('R4_STALL_DD'),'test IO command forbidden for a gate')
    a.work.mkdir(mode=0o700)
    publish(a.work/'writer-meta.json',dict(pid=os.getpid(),boot=boot(),device=str(a.device),block_device=block_device,
        file_control=a.file_control,dd_override=bool(os.environ.get('R4_STALL_DD')),pace_ms=a.pace_ms,start=process_start(os.getpid())))
    if block_device:
        capacity=int(subprocess.check_output(['blockdev','--getsize64',str(a.device)]))
    else:capacity=a.device.stat().st_size
    maximum=capacity//4096-1 # last block reserved for the dropped-write control
    require(maximum>0,'device too small')
    command=json.loads(os.environ['R4_STALL_DD']) if os.environ.get('R4_STALL_DD') else ['dd']
    require(isinstance(command,list) and command and all(isinstance(v,str) for v in command),'invalid IO command')
    stopping=False
    def stop(signum,frame):
        nonlocal stopping
        stopping=True
    signal.signal(signal.SIGTERM,stop);signal.signal(signal.SIGINT,stop)
    require(a.file_control or (a.status and a.peers and a.replica), 'primary status and peer sampling required')
    observer_stop = threading.Event()
    observer = None
    if a.peers:
        observer = threading.Thread(target=observe_peers, args=(a, observer_stop), daemon=True)
        observer.start()
    attempts=0;payload=a.work/'payload';ledger=a.work/'ledger'
    with ledger.open('x') as journal:
        while not stopping and not (a.work/'stop-request.json').exists() and (a.max_ops==0 or attempts<a.max_ops):
            require(attempts<maximum,'unique LBA space exhausted; never overwrite an ACKed block')
            op='op'+str(attempts);data=os.urandom(4096);digest=hashlib.sha256(data).hexdigest();offset=attempts*4096
            payload.write_bytes(data)
            line=f'{op} quorum-stall {offset} 4096 {digest}\n'
            journal.write('intent '+line);journal.flush();os.fsync(journal.fileno())
            started=time.monotonic_ns();append(a.work/'io.jsonl',dict(kind='begin',op=op,at_ns=started))
            outcome=subprocess.run([*command,'if='+str(payload),'of='+str(a.device),'bs=4096','count=1','seek='+str(attempts),
                'oflag='+('sync' if a.file_control else 'direct,dsync'),'conv=notrunc','status=none'],
                stdin=subprocess.DEVNULL,capture_output=True,timeout=30)
            ended=time.monotonic_ns();ok=outcome.returncode==0
            if ok:
                journal.write('ack '+line);journal.flush();os.fsync(journal.fileno())
            append(a.work/'io.jsonl',dict(kind='end',op=op,at_ns=ended,ok=ok,exit=outcome.returncode,
                error=outcome.stderr.decode(errors='replace')[:400]))
            attempts+=1
            if ok and not (a.work/'ready.json').exists():publish(a.work/'ready.json',dict(at_ns=ended))
            time.sleep(a.pace_ms/1000)
    observer_stop.set()
    if observer is not None:
        observer.join(timeout=3)
        require(not observer.is_alive(), 'peer observer did not stop')
    publish(a.work/'writer-stopped.json',dict(attempts=attempts,at_ns=time.monotonic_ns()))
    return dict(attempts=attempts)

def main():
    p=argparse.ArgumentParser(description=__doc__);sub=p.add_subparsers(dest='command',required=True)
    for cmd in ('judge','policy'):
        parser=sub.add_parser(cmd);parser.add_argument('--rf',choices=('2','3'),required=True)
        parser.add_argument('--write-budget-ms',type=float,required=True);parser.add_argument('--resume-budget-ms',type=float,required=True);parser.add_argument('--budget-ref',required=True)
        if cmd=='judge':parser.add_argument('--work',type=Path,required=True);parser.add_argument('--require-block-device',action='store_true');parser.add_argument('--output',type=Path)
    w=sub.add_parser('writer');w.add_argument('--work',type=Path,required=True);w.add_argument('--device',type=Path,required=True)
    w.add_argument('--max-ops',type=int,default=0);w.add_argument('--pace-ms',type=float,default=10);w.add_argument('--file-control',action='store_true');w.add_argument('--status');w.add_argument('--peers');w.add_argument('--replica',choices=('r2','r3'))
    m=sub.add_parser('mark');m.add_argument('--work',type=Path,required=True);m.add_argument('--phase',choices=('baseline','fault-pending','fault','recover','after','stop'),required=True)
    m.add_argument('--status',required=True);m.add_argument('--fault-key');m.add_argument('--peers')
    q=sub.add_parser('wait-acks');q.add_argument('--work',type=Path,required=True);q.add_argument('--minimum',type=int,required=True);q.add_argument('--since');q.add_argument('--seconds',type=float,default=30)
    r=sub.add_parser('wait-peer');r.add_argument('--work',type=Path,required=True);r.add_argument('--replica',choices=('r2','r3'),required=True);r.add_argument('--output',type=Path,required=True);r.add_argument('--seconds',type=float,default=120)
    ready=sub.add_parser('ready-peer');ready.add_argument('--status',required=True);ready.add_argument('--peers',required=True);ready.add_argument('--replica',choices=('r2','r3'),required=True);ready.add_argument('--seconds',type=float,default=30)
    stop=sub.add_parser('stop');stop.add_argument('--work',type=Path,required=True);stop.add_argument('--seconds',type=float,default=35)
    a=p.parse_args()
    if a.command=='policy':
        budgets(a);require(not os.environ.get('R4_STALL_DD'),'test IO command is not allowed for a gate');result=dict(policy='READY',write_budget_ms=a.write_budget_ms,resume_budget_ms=a.resume_budget_ms,budget_ref=a.budget_ref)
    else:result={'judge':judge,'writer':writer,'mark':mark,'wait-acks':wait_acks,'wait-peer':wait_peer,'ready-peer':ready_peer,'stop':stop_writer}[a.command](a)
    if a.command=='judge' and a.output:publish(a.output,result)
    print(json.dumps(result,sort_keys=True))
if __name__=='__main__':
    try:main()
    except (ValueError,KeyError,FileNotFoundError,json.JSONDecodeError) as error:print(str(error),file=sys.stderr);sys.exit(1)
