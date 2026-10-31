"""Small external-stack CLI around the accepted KV worker/oracle. No lifecycle."""
import argparse, datetime, hashlib, json, os, sys, pathlib, types, time
from urllib.parse import quote
from urllib.request import urlopen


def witness(before, after, config):
    a, b = before['loader'], after['loader']
    route = config['transport']
    if route not in ('http','rc','dc'):
        raise AssertionError('unsupported transport')
    transport = 'http' if route=='http' else 'rdma'
    if a['transport'] != transport or b['transport'] != transport or a['http_fallback_allowed'] is not False or b['http_fallback_allowed'] is not False:
        raise AssertionError('explicit transport required; HTTP fallback refused')
    expected = config['keys'] * config['length']
    def delta(name):
        x, y = a[name], b[name]
        if type(x) is not int or type(y) is not int or x < 0 or y < x:
            raise AssertionError('invalid/missing transport counter')
        return y-x
    for field, value in {'read_bytes': expected, 'read_objects': config['keys'],
                         'direct_rc_bytes': expected if route=='rc' else 0, 'direct_rc_reads': config['keys'] if route=='rc' else 0,
                         'direct_dc_bytes': expected if route=='dc' else 0, 'direct_dc_reads': config['keys'] if route=='dc' else 0,
                         'copied_bytes': expected if route=='http' else 0, 'copied_reads': config['keys'] if route=='http' else 0,
                         'write_bytes': expected, 'write_objects': config['keys']}.items():
        if delta(field) != value:
            raise AssertionError('transport witness differs: '+field)
    if after['read_errors'] != before['read_errors']:
        raise AssertionError('read errors')
    return dict(read_transport=route, read_bytes=expected, fallback=False,
                loader_write_bytes=expected, server_write_join='NOT_OBSERVED')


def exact_helper(name):
    # Read/compile the protected sibling source itself: ordinary imports can
    # prefer a same-name package or bytecode over the pinned/scanned .py file.
    source = pathlib.Path(__file__).resolve().with_name(name+'.py')
    module = types.ModuleType('_seaweed_pinned_'+name)
    module.__file__ = str(source)
    sys.modules[module.__name__] = module  # dataclasses resolves this exact module
    exec(compile(source.read_bytes(), str(source), 'exec'), module.__dict__)
    return module


def run(conn, c, metadata, emit):
    helper = exact_helper('soak_native_worker')
    work, payload = helper.work, helper.payload
    validate_ack_metadata, validate_api = helper.validate_ack_metadata, helper.validate_api
    Oracle = exact_helper('soak_oracle').Oracle
    validate_api(type(conn))
    oracle = Oracle()
    before = json.loads(conn.stats_json())
    for i in range(c['keys']):
        key = c['client']+'-'+str(i)
        command = dict(key=key, seed=c['run_id']+'/'+key, length=c['length'], operation_id=2*i, action='set')
        intent = oracle.begin_write(key, payload(command['seed'], command['length']))
        row = work(conn, command)
        if not row['ok'] or row['results'] not in (None, [True]):
            raise AssertionError('SET failed or ambiguous; never retried')
        fids = validate_ack_metadata(metadata(c['root']+'/'+key), command, row)
        version = oracle.acknowledge(key, intent.sequence, command['length'], fids)
        emit(dict(event='ACK', key=key, sequence=version.sequence, length=version.length, sha256=version.digest, fids=fids))
        reserved = oracle.begin_read(key)
        command.update(action='get', operation_id=2*i+1)
        row = work(conn, command)
        if not row['ok'] or row['results'] not in (None, [True]) or row['length'] != reserved.length or row['sha256'] != reserved.digest:
            raise AssertionError('GET miss/error/wrong latest ACK bytes')
        del oracle.reading[key]
        emit(dict(event='EXACT', key=key, sha256=row['sha256'], length=row['length']))
    after = json.loads(conn.stats_json())
    proof = witness(before, after, c)
    return dict(status='PASS', ack=c['keys'], exact=c['keys'], wrong=0, witness=proof, before=before, after=after)


def configure(c):
    if c['transport'] not in ('http', 'rc', 'dc') or not 1 <= c['keys'] <= 16 or not 1 <= c['length'] <= 4*1024*1024:
        raise ValueError('unsupported transport or smoke bound')
    for key in list(os.environ):
        if key.startswith('SW_RDMA_'):
            del os.environ[key]
    if c['transport']=='http':
        if any(key in c for key in ('rdma_address','rdma_control','dc_control','device','device_port','gid_index')):
            raise ValueError('HTTP profile cannot carry RDMA selectors')
        return 'loader-http'
    os.environ.update(SW_RDMA_KVCACHE_PUSH_DC='1' if c['transport']=='dc' else '0',
        SW_RDMA_KVCACHE_PUSH_RDMA_ADDR=c['rdma_address'],
        SW_RDMA_KVCACHE_PUSH_CONTROL_ADDR=c['rdma_control'],
        SW_RDMA_KVCACHE_PUSH_PIPES='8', SW_RDMA_KVCACHE_PUSH_SOURCE_RANGE_MR='1')
    if c['transport']=='dc':
        os.environ.update(SW_RDMA_KVCACHE_PUSH_DC_CONTROL_ADDR=c['dc_control'],
            SW_RDMA_KVCACHE_PUSH_DC_DEVICE=c['device'],
            SW_RDMA_KVCACHE_PUSH_DC_PORT=str(c['device_port']),
            SW_RDMA_KVCACHE_PUSH_DC_GID_INDEX=str(c['gid_index']))
    return 'loader-rdma'


def read_stage_args(stage, route):
    if stage not in ('seed','load','recovery') or route not in ('dc','http') or (stage=='seed' and route!='dc'):
        raise ValueError('unsupported read-only case stage/path')
    return stage, route


def read_only(conn, c, stage, route):
    read_stage_args(stage,route)
    if stage=='seed':raise ValueError('seed uses the unchanged settled smoke path')
    helper=exact_helper('soak_native_worker')
    helper.validate_api(type(conn))
    before=json.loads(conn.stats_json());started=time.monotonic();epoch=time.time()
    exact=attempts=0;faults=[];outcomes=[];first=last=None
    # One operation at a time, small reusable allocations, bounded rate/output.
    while (stage=='load' and time.monotonic()-started<120) or (stage=='recovery' and attempts<c['keys']):
        key=c['client']+'-'+str(attempts%c['keys'])
        command=dict(action='get',key=key,seed=c['run_id']+'/'+key,length=c['length'],operation_id=attempts)
        row=helper.work(conn,command)  # Unknown completion/timeout is fatal; never retried.
        end=time.time();attempts+=1
        if attempts>1024:raise AssertionError('bounded GET evidence exceeded')
        outcomes.append(dict(operation_id=row['operation_id'],key=key,started=row['submitted'],ended=row['completed'],
            exact=row['ok'] and row['results'] in (None,[True])))
        if not row['ok'] or row['results'] not in (None,[True]):
            if stage!='load':raise AssertionError('recovery GET miss/error')
            if len(faults)>=256:raise AssertionError('bounded completed-failure evidence exceeded')
            faults.append(dict(action='get',key=key,operation_id=row['operation_id'],target=c['rdma_address'],
                transport=route,started=row['submitted_epoch'],ended=end,ok=row['ok'],error=row['error'],results=row['results']))
        else:
            digest=hashlib.sha256(helper.payload(command['seed'],c['length'])).hexdigest()
            if row['length']!=c['length'] or row['sha256']!=digest:raise AssertionError('GET wrong pre-seeded bytes')
            exact+=1;first=first if first is not None else end;last=end
        if stage=='load':time.sleep(.25)
    after=json.loads(conn.stats_json());a,b=before['loader'],after['loader']
    expected_transport='rdma' if route=='dc' else 'http'
    if a['transport']!=expected_transport or b['transport']!=expected_transport:raise AssertionError('wrong read transport')
    if route=='dc' and (a['http_fallback_allowed'] is not False or b['http_fallback_allowed'] is not False):raise AssertionError('fallback refused')
    def delta(field):
        x,y=a[field],b[field]
        if type(x) is not int or type(y) is not int or x<0 or y<x:raise AssertionError('missing/regressed read counter')
        return y-x
    selected='direct_dc' if route=='dc' else 'copied'
    reads=delta('read_objects');size=delta('read_bytes')
    if reads<exact or size<exact*c['length'] or delta(selected+'_reads')!=reads or delta(selected+'_bytes')!=size:
        raise AssertionError('read path witness differs')
    for other in ('direct_dc','direct_rc','copied'):
        if other!=selected and (delta(other+'_reads') or delta(other+'_bytes')):raise AssertionError('foreign read path')
    if delta('write_objects') or delta('write_bytes'):raise AssertionError('read-only stage wrote')
    errors=after['read_errors']-before['read_errors']
    if errors<0 or errors>len(faults):raise AssertionError('unattributed read error counter')
    return dict(status='OBSERVED',stage=stage,transport=route,exact=exact,attempts=attempts,faults=faults,
        outcomes=outcomes,first_exact=first,last_exact=last,started=epoch,ended=time.time(),elapsed=time.monotonic()-started,
        before=before,after=after,run_id=c['run_id'],client=c['client'],root=c['root'])


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--config', required=True)
    p.add_argument('--dry-run', action='store_true')
    p.add_argument('--stage', choices=('seed','load','recovery'))
    p.add_argument('--read-path', choices=('dc','http'))
    args = p.parse_args()
    emit = lambda obj: print(json.dumps(obj, allow_nan=False), flush=True)
    conn = None
    try:
        c = json.loads(args.config)
        if args.stage or args.read_path:
            read_stage_args(args.stage,args.read_path)
            if c['transport']!='dc':raise ValueError('read-only restart requires DC inventory')
        backing = configure(c)
        if args.dry_run:
            if args.stage:
                emit(dict(status='PLAN',code='OK',effects=[],stage=args.stage,read_path=args.read_path,
                    backing='loader-http' if args.read_path=='http' else 'loader-rdma',fallback=False,
                    duration_seconds=120 if args.stage=='load' else 0,operations=None if args.stage=='load' else c['keys']*(2 if args.stage=='seed' else 1)))
            else:
                emit(dict(status='PLAN', code='OK', remedy='obtain independent S0b admission', effects=[], backing=backing, fallback=False, operations=2*c['keys']))
            return 0
        sys.path[:0] = [c['helper_dir'], c['native_dir']]
        from seaweedkv_native._native import SeaweedKVConnector
        options = dict(capacity_bytes=1, root=c['root'], collection='lmcache', replication='000')
        if args.read_path:
            backing='loader-http' if args.read_path=='http' else 'loader-rdma'
        if args.read_path=='http':os.environ['SW_RDMA_KVCACHE_PUSH_DC']='0'
        conn = SeaweedKVConnector(c['filer_grpc'], backing, 4, json.dumps(options))
        def metadata(key):
            with urlopen(c['filer_http']+quote(key,safe='/')+'?metadata=true&resolveManifest=true', timeout=8) as r:
                return json.load(r)
        result = read_only(conn,c,args.stage,args.read_path) if args.stage in ('load','recovery') else run(conn, c, metadata, emit)
        conn.close()
        conn = None
        emit(dict(result, code='OK', remedy='inspect ACK/exact/transport records'))
        return 0
    except Exception as error:
        emit(dict(status='FAIL', code='KV_SMOKE_FAILED', remedy='retain buffers and owned-process evidence; no retry', error=str(error)))
        return 1
    finally:
        # The accepted worker retains unknown DMA buffers through process exit.
        # S0b owns termination/settlement if close itself cannot complete.
        if conn is not None:
            conn.close()

if __name__ == '__main__':
    raise SystemExit(main())
