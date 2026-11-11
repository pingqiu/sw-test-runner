"""Native connector worker for the accepted file-backed rotation soak.

The parent owns per-key guards and ACK state. Children never choose expected
bytes from a GET result, and never retry a mutation with an unknown outcome.
"""
import datetime, hashlib, json, os, select, time
from urllib.parse import quote
from urllib.request import urlopen


def payload(seed, length):
    block = hashlib.sha256(seed.encode()).digest()
    return bytearray((block * ((length + 31)//32))[:length])


UNSETTLED_BUFFERS = []  # Hold caller allocations until owned process teardown.

def finish(conn, future, timeout=30, abandoned=None, late=None):
    """abandoned (stage 3 F3 only): this worker process's futures that hit the deadline, fid -> operation id; see
    finish_tracked. Without it (every other row, r14) the set is empty: the whole batch is still processed, and every
    fid but the waiting op's, in any position, is fatal, including a late completion of an op that timed out."""
    return finish_tracked(conn, future, timeout, {} if abandoned is None else abandoned, late)


def finish_tracked(conn, future, timeout, abandoned, late):
    """F3 (r13, codex01 on r12): every drained batch is processed WHOLE before returning, in any order. The waiting
    op's fid is taken once (a second one, in this batch or a later drain, is fatal); each abandoned fid is consumed
    once and reported through late(row); any other fid is fatal ('unowned completion'). A worker has exactly one op in
    flight (work() submits one future and waits for it), so no other fid can legitimately be owned."""
    end=time.monotonic()+timeout
    while time.monotonic()<end:
        if not select.select([conn.event_fd()],[],[],max(0,end-time.monotonic()))[0]: continue
        got=None
        for fid,ok,error,results in conn.drain_completions():
            fid=int(fid)
            if fid==future:
                if got is not None: raise RuntimeError('duplicate completion')
                got=(bool(ok), str(error), list(results) if results is not None else None)
            elif fid in abandoned:
                op=abandoned.pop(fid)
                late(dict(operation_id=op,fid=fid,ok=bool(ok),error=str(error),
                          results=list(results) if results is not None else None,mono=time.monotonic()))
            else:
                raise RuntimeError('unowned completion')
        if got is not None:
            return got
    raise TimeoutError('native completion deadline')


def k8_refusal_misses(stats):
    """The K8 budget-refusal miss counter (refusal, its deadline or a hold-off) of one stats object, or None
    when malformed. The one strict decoder (worker and assessor): every present counter a non-negative,
    non-bool int; no native_budget_refusal_process at all is 0 (a build without K8)."""
    if not isinstance(stats,dict): return None
    b=stats.get('native_budget_refusal_process')
    if b is None: return 0
    if not isinstance(b,dict): return None
    total=0
    for k in ('missing_keys_refused','missing_keys_deadline'):
        v=b.get(k,0)
        if isinstance(v,bool) or not isinstance(v,int) or v<0: return None
        total+=v
    return total


def refusal_misses(conn):
    return k8_refusal_misses(json.loads(conn.stats_json()))


def work(conn, command, submitted=None, abandoned=None, late=None):
    action=command['action']; key=command['key']
    expected=payload(command['seed'],command['length'])
    if action not in ('set','get'): raise ValueError('unknown operation')
    # One op in flight per worker process: the counter's delta across this GET is this GET's miss cause.
    refused_before=refusal_misses(conn) if action=='get' else None
    buf=expected if action=='set' else bytearray(v^255 for v in expected)
    start=time.monotonic();start_epoch=time.time()
    future=int(getattr(conn,'submit_batch_'+action)([key],[memoryview(buf)]))
    if submitted is not None:
        # This is a submission witness, NOT a DMA-completion/absence proof.
        submitted(dict(event='submitted',operation_id=command['operation_id'],
                       action=action,future=future,pid=os.getpid(),monotonic=time.monotonic()))
    try:
        ok,error,results=finish(conn,future) if abandoned is None else finish(conn,future,abandoned=abandoned,late=late)
    except TimeoutError:
        UNSETTLED_BUFFERS.append(buf)
        if abandoned is not None: abandoned[future]=command['operation_id']  # F3: its completion may still arrive
        raise
    except BaseException:
        UNSETTLED_BUFFERS.append(buf)
        raise
    row=dict(operation_id=command['operation_id'], action=action, key=key,
             submitted=start, submitted_epoch=start_epoch, completed=time.monotonic(), ok=ok,error=error,results=results)
    if action=='get':
        row.update(length=len(buf),sha256=hashlib.sha256(buf).hexdigest(),
                   untouched=buf==bytearray(v^255 for v in expected))
        refused_after=refusal_misses(conn)
        if refused_before is not None and refused_after is not None and refused_after>refused_before:
            row['cause']='budget_refusal'
    return row


def validate_ack_metadata(metadata,command,row):
    if metadata.get('FileSize')!=command['length']:raise ValueError('ACK metadata length differs')
    stamp=datetime.datetime.fromisoformat(metadata['Mtime'].replace('Z','+00:00'))
    if stamp.tzinfo is None or stamp.timestamp()<row['submitted_epoch']-.001:
        raise ValueError('ACK metadata version predates this submitted write')
    chunks=metadata.get('chunks',[])
    if not chunks:raise ValueError('ACK metadata has no FID')
    fids=[c.get('file_id',c.get('fileId')) for c in chunks]
    if any(not isinstance(fid,str) or ',' not in fid for fid in fids):
        raise ValueError('unsupported full FID metadata shape')
    return fids

def validate_api(cls):
    for name in ('submit_batch_get','submit_batch_set','event_fd','drain_completions','stats_json','close'):
        if not callable(getattr(cls,name,None)):raise TypeError('native connector API missing '+name)

def child(pipe,config):
    # 'rc' (#999) must never silently become loader-http: it is loader-rdma
    # with DC push off, which the parent's read_route set and this asserts.
    if config.get('transport') not in ('dc','rc','http'):
        raise ValueError('unsupported soak transport: '+repr(config.get('transport')))
    if config['transport']=='rc' and os.environ.get('SW_RDMA_KVCACHE_PUSH_DC')!='0':
        raise ValueError('rc soak worker needs SW_RDMA_KVCACHE_PUSH_DC=0')
    from seaweedkv_native._native import SeaweedKVConnector
    validate_api(SeaweedKVConnector)
    backing='loader-http' if config['transport']=='http' else 'loader-rdma'
    if config['transport']=='http':os.environ['SW_RDMA_KVCACHE_PUSH_DC']='0'
    conn=SeaweedKVConnector(config['filer_grpc'],backing,4,json.dumps(config['native_options']))
    import threading
    from pathlib import Path
    from soak_stats import native_stats
    recorder_stop=threading.Event()
    recorder=threading.Thread(target=native_stats,args=(conn,Path(config['root']),recorder_stop,config['stats_horizon_s']),daemon=True)
    recorder.start()
    pipe.send(dict(event='ready',pid=os.getpid(),stats=json.loads(conn.stats_json()),backing=backing))
    # F3 only (the load sets late_completions for link_flap rows): this process's abandoned futures, empty at start
    abandoned={} if config.get('late_completions') is True else None
    late=(lambda row: pipe.send(dict(row,event='late'))) if abandoned is not None else None
    try:
        while True:
            command=pipe.recv()
            if command['action']=='stop': return
            if command['action']=='stats':
                pipe.send(dict(event='stats',stats=json.loads(conn.stats_json())));continue
            try:
                row=work(conn,command,pipe.send) if abandoned is None else work(conn,command,pipe.send,abandoned,late)
                if row['ok'] and command['action']=='set':
                    path=config['native_options']['root'].rstrip('/')+'/'+command['key']
                    url=config['filer_http']+quote(path,safe='/')+'?metadata=true&resolveManifest=true'
                    with urlopen(url,timeout=8) as response: metadata=json.load(response)
                    row['metadata']=metadata
                    row['fids']=validate_ack_metadata(metadata,command,row)
                pipe.send(dict(event='complete',row=row))
            except BaseException as error:
                pipe.send(dict(event='error',operation_id=command.get('operation_id'),
                               action=command['action'],error=repr(error),epoch=time.time()))
    finally:
        # Connector destructor joins its native workers. Caller-owned buffers
        # remain alive through completion; an unknown operation stops the run.
        recorder_stop.set();recorder.join(5)
        if recorder.is_alive():raise RuntimeError("native stats recorder did not stop")
        del conn
