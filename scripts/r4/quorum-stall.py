#!/usr/bin/env python3
"""V1.0.1 sync-quorum guest measurements; never changes product authority."""
import argparse,json,math,os,signal,subprocess,sys,time,stat,hashlib,urllib.request,importlib.util
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
    require(all(math.isfinite(v) and v>0 for v in (a.write_budget_ms,a.resume_budget_ms))
            and a.budget_ref.strip(),'owner finite budget and source required')

def judge(a):
    budgets(a)
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
    fault_ops=[r for r in ends if phases['fault']<=r['start_ns']<phases['recover']]
    require(fault_ops,'writer did not attempt through replica fault')
    if a.rf=='2':require(not any(r['ok'] for r in fault_ops),'RF2 ACKed a new write with one replica absent')
    else:require(all(r['ok'] for r in ends),'RF3 refused a write through one replica fault')
    require(not any(not r['ok'] and (r['start_ns']<phases['fault-pending'] or r['start_ns']>=phases['after']) for r in ends),'refused outside expected quorum-loss window')
    max_write=max(durations);require(max_write<=a.write_budget_ms,'write budget exceeded')
    def gap(lo,hi):
        points=[lo]+[t for t in acks if lo<t<hi]+[hi]
        return max((b-a)/1e6 for a,b in zip(points,points[1:]))
    global_gap=gap(phases['baseline'],phases['stop'])
    bounded_gap=global_gap if a.rf=='3' else max(gap(phases['baseline'],phases['fault-pending']),gap(phases['after'],phases['stop']))
    require(bounded_gap<=a.write_budget_ms,'ACK-gap write budget exceeded')
    recovered=[t for t in acks if t>=phases['recover']]
    require(recovered,'no ACK after replica return')
    resume=(recovered[0]-phases['recover'])/1e6
    require(resume<=a.resume_budget_ms,'resume budget exceeded')
    ordered=sorted(durations)
    return dict(timing_result='PASS',rf=a.rf,ack_mode='sync-quorum',attempts=len(ends),acks=len(acks),
        refused=sum(not r['ok'] for r in ends),max_write_ms=max_write,p50_write_ms=ordered[(len(ordered)-1)//2],
        p99_write_ms=ordered[math.ceil(.99*len(ordered))-1],max_ack_gap_ms=global_gap,
        bounded_ack_gap_ms=bounded_gap,resume_ms=resume,write_budget_ms=a.write_budget_ms,
        resume_budget_ms=a.resume_budget_ms,budget_ref=a.budget_ref,
        rf2_outage_excluded_from_ack_gap_budget=(a.rf=='2'),data_readback='separate mandatory scenario step')

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

def wait_peer(a):
    spec=importlib.util.spec_from_file_location('r4_peers',Path(__file__).with_name('auto-p2.py'))
    peer=importlib.util.module_from_spec(spec);spec.loader.exec_module(peer)
    end=time.monotonic()+a.seconds;last={}
    while time.monotonic()<end:
        try:
            own=status(a.status);primary(own)
            last=status(a.peers)
            if own.get('Healthy') is True and own.get('FrontendPrimaryReady') is True and peer.peer_healthy(last,'r1',a.replica,1):
                publish(a.output,dict(primary=own,peers=last));return last
        except (OSError,TimeoutError):pass
        time.sleep(.2)
    publish(a.output,dict(last=last));raise ValueError('returning replica never became Healthy on the primary')

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
    publish(a.work/'writer-stopped.json',dict(attempts=attempts,at_ns=time.monotonic_ns()))
    return dict(attempts=attempts)

def main():
    p=argparse.ArgumentParser(description=__doc__);sub=p.add_subparsers(dest='command',required=True)
    for cmd in ('judge','policy'):
        parser=sub.add_parser(cmd);parser.add_argument('--rf',choices=('2','3'),required=True)
        parser.add_argument('--write-budget-ms',type=float,required=True);parser.add_argument('--resume-budget-ms',type=float,required=True);parser.add_argument('--budget-ref',required=True)
        if cmd=='judge':parser.add_argument('--work',type=Path,required=True);parser.add_argument('--require-block-device',action='store_true');parser.add_argument('--output',type=Path)
    w=sub.add_parser('writer');w.add_argument('--work',type=Path,required=True);w.add_argument('--device',type=Path,required=True)
    w.add_argument('--max-ops',type=int,default=0);w.add_argument('--pace-ms',type=float,default=10);w.add_argument('--file-control',action='store_true')
    m=sub.add_parser('mark');m.add_argument('--work',type=Path,required=True);m.add_argument('--phase',choices=('baseline','fault-pending','fault','recover','after','stop'),required=True)
    m.add_argument('--status',required=True);m.add_argument('--fault-key')
    q=sub.add_parser('wait-acks');q.add_argument('--work',type=Path,required=True);q.add_argument('--minimum',type=int,required=True);q.add_argument('--since');q.add_argument('--seconds',type=float,default=30)
    r=sub.add_parser('wait-peer');r.add_argument('--status',required=True);r.add_argument('--peers',required=True);r.add_argument('--replica',choices=('r2','r3'),required=True);r.add_argument('--output',type=Path,required=True);r.add_argument('--seconds',type=float,default=180)
    stop=sub.add_parser('stop');stop.add_argument('--work',type=Path,required=True);stop.add_argument('--seconds',type=float,default=35)
    a=p.parse_args()
    if a.command=='policy':
        budgets(a);require(not os.environ.get('R4_STALL_DD'),'test IO command is not allowed for a gate');result=dict(policy='READY',write_budget_ms=a.write_budget_ms,resume_budget_ms=a.resume_budget_ms,budget_ref=a.budget_ref)
    else:result={'judge':judge,'writer':writer,'mark':mark,'wait-acks':wait_acks,'wait-peer':wait_peer,'stop':stop_writer}[a.command](a)
    if a.command=='judge' and a.output:publish(a.output,result)
    print(json.dumps(result,sort_keys=True))
if __name__=='__main__':
    try:main()
    except (ValueError,KeyError,FileNotFoundError,json.JSONDecodeError) as error:print(str(error),file=sys.stderr);sys.exit(1)
