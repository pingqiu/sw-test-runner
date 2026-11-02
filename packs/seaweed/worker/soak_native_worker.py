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

def finish(conn, future, timeout=30):
    end=time.monotonic()+timeout
    while time.monotonic()<end:
        if not select.select([conn.event_fd()],[],[],max(0,end-time.monotonic()))[0]: continue
        for fid,ok,error,results in conn.drain_completions():
            if int(fid)!=future: raise RuntimeError('unowned completion')
            return bool(ok), str(error), list(results) if results is not None else None
    raise TimeoutError('native completion deadline')


def work(conn, command, submitted=None):
    action=command['action']; key=command['key']
    expected=payload(command['seed'],command['length'])
    if action not in ('set','get'): raise ValueError('unknown operation')
    buf=expected if action=='set' else bytearray(v^255 for v in expected)
    start=time.monotonic();start_epoch=time.time()
    future=int(getattr(conn,'submit_batch_'+action)([key],[memoryview(buf)]))
    if submitted is not None:
        # This is a submission witness, NOT a DMA-completion/absence proof.
        submitted(dict(event='submitted',operation_id=command['operation_id'],
                       action=action,future=future,pid=os.getpid(),monotonic=time.monotonic()))
    try:
        ok,error,results=finish(conn,future)
    except BaseException:
        UNSETTLED_BUFFERS.append(buf)
        raise
    row=dict(operation_id=command['operation_id'], action=action, key=key,
             submitted=start, submitted_epoch=start_epoch, completed=time.monotonic(), ok=ok,error=error,results=results)
    if action=='get':
        row.update(length=len(buf),sha256=hashlib.sha256(buf).hexdigest(),
                   untouched=buf==bytearray(v^255 for v in expected))
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
    # Only these two modes were qualified by this soak. In particular 'rc'
    # must never silently become loader-http; an RC adapter constructs its
    # own loader-rdma connector explicitly.
    if config.get('transport') not in ('dc','http'):
        raise ValueError('unsupported soak transport: '+repr(config.get('transport')))
    from seaweedkv_native._native import SeaweedKVConnector
    validate_api(SeaweedKVConnector)
    backing='loader-rdma' if config['transport']=='dc' else 'loader-http'
    if config['transport']=='http':os.environ['SW_RDMA_KVCACHE_PUSH_DC']='0'
    conn=SeaweedKVConnector(config['filer_grpc'],backing,4,json.dumps(config['native_options']))
    import threading
    from pathlib import Path
    from soak_stats import native_stats
    recorder_stop=threading.Event()
    recorder=threading.Thread(target=native_stats,args=(conn,Path(config['root']),recorder_stop,config['stats_horizon_s']),daemon=True)
    recorder.start()
    pipe.send(dict(event='ready',pid=os.getpid(),stats=json.loads(conn.stats_json()),backing=backing))
    try:
        while True:
            command=pipe.recv()
            if command['action']=='stop': return
            if command['action']=='stats':
                pipe.send(dict(event='stats',stats=json.loads(conn.stats_json())));continue
            try:
                row=work(conn,command,pipe.send)
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
