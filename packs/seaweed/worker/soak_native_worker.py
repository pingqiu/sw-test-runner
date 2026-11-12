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
STOP_DRAIN_S = 10  # r26 F5 ownership: at stop, abandoned futures are drained for at most this long, then `final`


class PhaseError(Exception):
    """r26 F5 ownership mode (plan v4 C, v5 C1): an op that raised, with the phase it raised in.
    submit: no fid, and enqueue cannot be excluded, so the connector is POISONED (no further op or attribution);
    wait: its future missed the deadline and is ABANDONED (the worker is blocked until it drains);
    ack: a settled ok=true SET whose ACK metadata read failed (its native row rides along);
    protocol: anything else while waiting (a duplicate or unowned completion): fatal for the run."""

    def __init__(self, phase, cause, row=None):
        super().__init__(phase, repr(cause))
        self.phase, self.cause, self.row = phase, cause, row


def failed_ops(stats, action):
    """r26: the connector's GENERIC per-operation failure count (connector_failed_ops[action]) of one stats object, or
    None when absent or malformed (a present object whose entry is a non-negative, non-bool int). A count names that
    an op failed, never why: it is no refusal and no cause."""
    if not isinstance(stats,dict): return None
    f=stats.get('connector_failed_ops')
    if not isinstance(f,dict) or action not in f: return None
    v=f[action]
    if isinstance(v,bool) or not isinstance(v,int) or v<0: return None
    return v


def stats_or_none(conn):
    """r26 ownership mode: one stats read; a failed or non-object read is None (the op stays UNCOUNTED), never an op
    failure."""
    try:
        s=json.loads(conn.stats_json())
    except Exception:
        return None
    return s if isinstance(s,dict) else None

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


def unavailable_gets(stats):
    """#1187: the connector's volume-unavailable GET counter (connector_unavailable_ops.get) of one stats object, or
    None when malformed. Only an ABSENT connector_unavailable_ops is 0 (a build before #1187); a present one must be
    an object whose `get` is a non-negative, non-bool int, so a present null, a non-object, a missing or a non-int
    `get` is malformed (None): only a real numeric delta can flag a GET."""
    if not isinstance(stats,dict): return None
    if 'connector_unavailable_ops' not in stats: return 0
    u=stats['connector_unavailable_ops']
    if not isinstance(u,dict) or 'get' not in u: return None
    v=u['get']
    if isinstance(v,bool) or not isinstance(v,int) or v<0: return None
    return v


def refusal_misses(conn):
    return k8_refusal_misses(json.loads(conn.stats_json()))


def work(conn, command, submitted=None, abandoned=None, late=None, own=False):
    """own (r26 F5 ownership mode): every raise is a PhaseError, and SETs as well as GETs carry the strict per-op
    connector_failed_ops delta; without it (every other row) the op runs exactly as before."""
    action=command['action']; key=command['key']
    expected=payload(command['seed'],command['length'])
    if action not in ('set','get'): raise ValueError('unknown operation')
    # One op in flight per worker process: a counter's delta across this GET is this GET's (its miss cause, and #1187's
    # volume-unavailable answer). r26: in ownership mode the worker also takes no new op while an abandoned future is
    # outstanding (child()), so a delta across this op cannot include another op's failure.
    stats_before=(stats_or_none(conn) if own else json.loads(conn.stats_json())) if action=='get' or own else None
    refused_before=k8_refusal_misses(stats_before) if action=='get' else None
    unavailable_before=unavailable_gets(stats_before) if action=='get' else None
    buf=expected if action=='set' else bytearray(v^255 for v in expected)
    start=time.monotonic();start_epoch=time.time()
    try:
        future=int(getattr(conn,'submit_batch_'+action)([key],[memoryview(buf)]))
    except BaseException as error:
        if not own: raise
        UNSETTLED_BUFFERS.append(buf)  # enqueue cannot be excluded: the buffer may be the native layer's
        raise PhaseError('submit',error)
    if submitted is not None:
        # This is a submission witness, NOT a DMA-completion/absence proof.
        submitted(dict(event='submitted',operation_id=command['operation_id'],
                       action=action,future=future,pid=os.getpid(),monotonic=time.monotonic()))
    try:
        ok,error,results=finish(conn,future) if abandoned is None else finish(conn,future,abandoned=abandoned,late=late)
    except TimeoutError as error:
        UNSETTLED_BUFFERS.append(buf)
        if abandoned is not None: abandoned[future]=command['operation_id']  # F3: its completion may still arrive
        if own: raise PhaseError('wait',error)
        raise
    except BaseException as error:
        UNSETTLED_BUFFERS.append(buf)
        if own: raise PhaseError('protocol',error)
        raise
    row=dict(operation_id=command['operation_id'], action=action, key=key,
             submitted=start, submitted_epoch=start_epoch, completed=time.monotonic(), ok=ok,error=error,results=results)
    stats_after=None
    if own:
        # r26: strict per-op attribution on this process's one connector; a decrease (a reset), a missing or a malformed
        # reading leaves failed_delta ABSENT (the op is uncounted; nothing is inferred).
        stats_after=stats_or_none(conn)
        b,a=failed_ops(stats_before,action),failed_ops(stats_after,action)
        row['pid']=os.getpid()
        if b is not None and a is not None and a>=b:
            row['failed_delta']=a-b
    if action=='get':
        row.update(length=len(buf),sha256=hashlib.sha256(buf).hexdigest(),
                   untouched=buf==bytearray(v^255 for v in expected))
        if not own:
            stats_after=json.loads(conn.stats_json())
        refused_after=k8_refusal_misses(stats_after)
        if refused_before is not None and refused_after is not None and refused_after>refused_before:
            row['cause']='budget_refusal'
        # #1187: True when the connector counted this GET as volume-unavailable, False when it did not; absent when
        # either reading is malformed (nothing is inferred then).
        unavailable_after=unavailable_gets(stats_after)
        if unavailable_before is not None and unavailable_after is not None:
            row['unavailable']=unavailable_after>unavailable_before
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
    # r26 F5: the ownership mode (the load sets it for mr_pressure rows only); every other row runs as before
    own=config.get('ownership') is True
    # F3 (link_flap) and F5 (ownership): this process's abandoned futures, empty at start
    abandoned={} if config.get('late_completions') is True or own else None
    late=(lambda row: pipe.send(dict(row,event='late'))) if abandoned is not None else None
    poisoned=[False]

    def idle(timeout):
        """r26 ownership, no op in flight: drain completions; each abandoned fid is consumed once (late), a fid on a
        poisoned connector is evidence only (orphan, never attributed); any other fid is fatal."""
        if not select.select([conn.event_fd()],[],[],max(0,timeout))[0]: return
        for fid,ok,error,results in conn.drain_completions():
            fid=int(fid)
            row=dict(fid=fid,ok=bool(ok),error=str(error),results=list(results) if results is not None else None,mono=time.monotonic())
            if fid in abandoned: late(dict(row,operation_id=abandoned.pop(fid)))
            elif poisoned[0]: pipe.send(dict(row,event='orphan'))
            else: raise RuntimeError('unowned completion')

    try:
        while True:
            if own and (abandoned or poisoned[0]):
                # The reuse barrier: a blocked (abandoned future outstanding) or poisoned worker takes no op; it drains.
                had=bool(abandoned)
                idle(0.05)
                if had and not abandoned and not poisoned[0]:
                    pipe.send(dict(event='unblocked',pid=os.getpid(),mono=time.monotonic()))
                if not pipe.poll(0): continue
            command=pipe.recv()
            if command['action']=='stop':
                if own:
                    # The final ownership record, sent BEFORE the destructor runs; the parent's exit proof is the
                    # observed process exit, never this row.
                    end=time.monotonic()+STOP_DRAIN_S
                    while abandoned and time.monotonic()<end: idle(min(.1,end-time.monotonic()))
                    pipe.send(dict(event='final',pid=os.getpid(),poisoned=poisoned[0],
                                   abandoned_outstanding=sorted(abandoned.values()),unsettled_buffers=len(UNSETTLED_BUFFERS),
                                   stats=stats_or_none(conn),mono=time.monotonic()))
                return
            if command['action']=='stats':
                pipe.send(dict(event='stats',stats=json.loads(conn.stats_json())));continue
            if own and (abandoned or poisoned[0]):
                pipe.send(dict(event='refused',operation_id=command.get('operation_id'),
                               reason='poisoned' if poisoned[0] else 'blocked'));continue
            try:
                if own:
                    row=work(conn,command,pipe.send,abandoned,late,own=True)
                else:
                    row=work(conn,command,pipe.send) if abandoned is None else work(conn,command,pipe.send,abandoned,late)
                if row['ok'] and command['action']=='set':
                    try:
                        path=config['native_options']['root'].rstrip('/')+'/'+command['key']
                        url=config['filer_http']+quote(path,safe='/')+'?metadata=true&resolveManifest=true'
                        with urlopen(url,timeout=8) as response: metadata=json.load(response)
                        row['metadata']=metadata
                        row['fids']=validate_ack_metadata(metadata,command,row)
                    except BaseException as error:
                        if own: raise PhaseError('ack',error,row)
                        raise
                pipe.send(dict(event='complete',row=row))
            except PhaseError as error:
                if error.phase=='submit': poisoned[0]=True
                pipe.send(dict(event='error',operation_id=command.get('operation_id'),action=command['action'],
                               error=repr(error.cause),phase=error.phase,epoch=time.time(),
                               **({} if error.row is None else dict(row=error.row))))
            except BaseException as error:
                pipe.send(dict(event='error',operation_id=command.get('operation_id'),
                               action=command['action'],error=repr(error),epoch=time.time()))
    finally:
        # Connector destructor joins its native workers. Caller-owned buffers
        # remain alive through completion; an unknown operation stops the run.
        recorder_stop.set();recorder.join(5)
        if recorder.is_alive():raise RuntimeError("native stats recorder did not stop")
        del conn
