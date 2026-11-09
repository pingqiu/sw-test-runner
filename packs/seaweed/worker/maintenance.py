"""Runner-owned maintenance leaf: real product commands, fixed stages, no fault authority.

All times in this leaf are on this host's boot-relative clock. The controller
samples that same clock around fault dispatch/GONE; no victim clock is joined.
"""
import base64
from contextlib import contextmanager
import hashlib
import json
import math
import os
import pathlib
import re
import selectors
import signal
import subprocess
import sys
import time
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen

TASKS = ('volume.balance', 'volume.fix.replication', 'ec_repair', 'ec_vacuum')
LIMIT = 2 * 1024 * 1024


def strict(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError('duplicate JSON key')
            result[key] = value
        return result
    return json.loads(raw, object_pairs_hook=pairs,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError('nonfinite JSON')))


def write(path, value, exclusive=False):
    data = json.dumps(value, allow_nan=False, sort_keys=True).encode()
    temporary=path.with_name('.'+path.name+'.tmp-'+str(os.getpid())+'-'+str(time.monotonic_ns()))
    fd = os.open(temporary, os.O_WRONLY|os.O_CREAT|os.O_NOFOLLOW|os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        if exclusive:
            os.link(temporary,path,follow_symlinks=False)
            temporary.unlink()
        else:
            os.replace(temporary,path)
        parent=os.open(path.parent,os.O_RDONLY|os.O_DIRECTORY)
        try:os.fsync(parent)
        finally:os.close(parent)
    finally:
        temporary.unlink(missing_ok=True)


@contextmanager
def http_deadline(seconds):
    # read1/socket timeouts otherwise restart per read: a trickling peer can
    # spend the next step's budget. This leaf is single-threaded on Linux.
    if seconds <= 0 or signal.getitimer(signal.ITIMER_REAL)[0]:
        raise ValueError('invalid or nested maintenance HTTP deadline')
    previous = signal.getsignal(signal.SIGALRM)
    def expired(*_):
        raise TimeoutError('maintenance HTTP operation deadline')
    signal.signal(signal.SIGALRM, expired)
    signal.setitimer(signal.ITIMER_REAL, seconds)
    try:
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)


def request(url, data=None, method='GET', timeout=30, maximum=LIMIT):
    with http_deadline(timeout):
        return bounded_request(url, data, method, timeout, maximum)


def bounded_request(url, data=None, method='GET', timeout=30, maximum=LIMIT):
    if not re.fullmatch(r'http://[A-Za-z0-9.:-]+/[^\s]*', url):
        raise ValueError('non-HTTP product URL')
    deadline = time.monotonic() + timeout
    headers = {'Content-Type': 'application/json'} if isinstance(data, dict) else {}
    body = json.dumps(data).encode() if isinstance(data, dict) else data
    try:
        response = urlopen(Request(url, data=body, method=method, headers=headers), timeout=timeout)
    except HTTPError as error:
        response = error
    try:
        with response:
            out = bytearray()
            while len(out) <= maximum:
                if time.monotonic() >= deadline:
                    raise TimeoutError('maintenance HTTP operation deadline')
                part = response.read1(min(65536, maximum + 1 - len(out)))
                if not part:
                    if getattr(response, 'length', None) not in (None, 0):
                        raise ValueError('incomplete HTTP body')
                    break
                out.extend(part)
            if len(out) > maximum:
                raise ValueError('maintenance HTTP body cap')
            return response.status, bytes(out)
    finally:
        response.close()


def api(url, data=None, method='GET', timeout=10):
    status, body = request(url, data, method, timeout)
    if status != 200:
        raise ValueError('product API status %d: %s' % (status, url.split('?')[0]))
    return strict(body)


def shell(c, command, seconds=180):
    # Command text is constructed only by this leaf from fixed verbs/numeric
    # VIDs and an admitted run collection. Nothing is passed to a local shell.
    q = subprocess.run([c['weed'], 'shell', '-master=' + c['master']],
                       input='lock\n' + command + '\nunlock\n', text=True,
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=seconds)
    text = q.stdout
    if len(text.encode()) > LIMIT:
        raise ValueError('product shell output cap')
    if q.returncode or re.search(r'(?im)^(?:error|failed)\b', text):
        raise ValueError('product shell failed: ' + text[-1000:])
    return text


def payload(run, key):
    return hashlib.sha256((run + '/' + key).encode()).digest() * (1024 * 1024 // 32)


def seed(c, root):
    collection = c['collection']
    query = dict(collection=collection, replication='000' if c['task'].startswith('ec_') else '001',
                 count='16', dataNode=c['seed_node'])
    if c['task'].startswith('ec_'):
        query['disk'] = 'ssd'
    assigned = api('http://' + c['master'] + '/dir/assign?' + urlencode(query))
    match = re.fullmatch(r'([1-9][0-9]*),([0-9a-fA-F]+)([0-9a-fA-F]{8})', assigned.get('fid', ''))
    if not match or int(assigned.get('count', 0)) < 16:
        raise ValueError('assign did not reserve the full seed key range')
    vid, first, cookie = match[1], int(match[2], 16), match[3]
    rows = []
    for index in range(16):
        key = 'maintenance-%d' % index
        fid = '%s,%x%s' % (vid, first + index, cookie)
        body = payload(c['run_id'], key)
        boundary = 'swtestmaintenance'
        multipart = (('--' + boundary + '\r\nContent-Disposition: form-data; name="file"; filename="data"\r\n'
                      'Content-Type: application/octet-stream\r\n\r\n').encode() + body +
                     ('\r\n--' + boundary + '--\r\n').encode())
        # urlopen's HTTP PUT also accepts a raw needle body. Explicit multipart
        # is used here to exercise the regular volume upload interface.
        req = Request('http://' + assigned['url'] + '/' + fid, data=multipart, method='POST',
                      headers={'Content-Type': 'multipart/form-data; boundary=' + boundary})
        with urlopen(req, timeout=30) as response:
            if response.status not in (200, 201):
                raise ValueError('seed upload failed; no replay')
            response.read(LIMIT)
        rows.append(dict(key=key, fid=fid, expected_sha256=hashlib.sha256(body).hexdigest(),
                         expected_length=len(body), deleted=False))
        # Persist each individual ACK before any later lookup/encode can fail.
        with (root / 'acks.jsonl').open('ab', buffering=0) as stream:
            stream.write(json.dumps(rows[-1], allow_nan=False).encode() + b'\n')
            os.fsync(stream.fileno())
    manifest = dict(run_id=c['run_id'], task=c['task'], collection=collection, vid=vid, objects=rows)
    write(root / 'manifest.json', manifest, True)
    return manifest


def readback(c, root):
    manifest = strict((root / 'manifest.json').read_bytes())
    if manifest['run_id'] != c['run_id'] or manifest['collection'] != c['collection']:
        raise ValueError('foreign manifest')
    rows = []
    for obj in manifest['objects']:
        status, data = request('http://' + c['master'] + '/' + obj['fid'], maximum=1024 * 1024)
        rows.append(dict(key=obj['key'], actual_sha256=hashlib.sha256(data).hexdigest(),
                         actual_length=len(data), status=status, observed=True))
    return rows


def encode(c, root):
    manifest = strict((root / 'manifest.json').read_bytes())
    shell(c, 'ec.encode -volumeIds=' + manifest['vid'] +
          ' -batchSize=0 -diskType=ssd -shardReplicaPlacement=001')
    if c['task'] == 'ec_vacuum':
        for obj in manifest['objects'][::2]:
            status, _ = request('http://' + c['master'] + '/' + obj['fid'], method='DELETE')
            if status not in (200, 202):
                raise ValueError('fixture DELETE not acknowledged; no replay')
            obj['deleted'] = True
            with (root / 'deletes.jsonl').open('ab', buffering=0) as stream:
                stream.write(json.dumps(dict(fid=obj['fid'], status=status)).encode()+b'\n');os.fsync(stream.fileno())
        write(root / 'manifest.json', manifest)
    return manifest


def plugin_preflight(c, root):
    deadline=time.monotonic()+20
    while True:
        types=api(c['admin']+'/api/plugin/job-types')
        if any(x.get('job_type')==c['task'] for x in types):break
        if time.monotonic()>=deadline:raise TimeoutError('maintenance worker registration20s')
        time.sleep(.1)
    config=dict(job_type=c['task'],admin_runtime=dict(enabled=False,max_jobs_per_detection=1),
                worker_config_values=dict(collection_filter=dict(string_value=c['collection']),
                    min_interval_seconds=dict(int64_value='1'),ec_min_interval_seconds=dict(int64_value='1'),
                    ec_deleted_ratio_threshold=dict(double_value=.01)))
    api(c['admin']+'/api/plugin/job-types/'+c['task']+'/config',config,'PUT')
    # Never manufacture license_valid in cluster_context: the admin builds it.
    status,raw=request(c['admin']+'/api/plugin/job-types/'+c['task']+'/detect',
                       dict(max_results=1,timeout_seconds=15),'POST',timeout=20)
    report=strict(raw)
    write(root/'preflight.json',dict(status=status,report=report),True)
    if status!=200 or report.get('error'):raise ValueError('maintenance product detection preflight refused: '+str(report.get('error','status%d'%status))[:500])
    if report.get('count')!=0:raise ValueError('unexpected pre-existing EC maintenance candidate')
    return dict(capability=c['task'],detection_status=status,count=0)


def detect(c, root):
    manifest=strict((root/'manifest.json').read_bytes())
    report=api(c['admin']+'/api/plugin/job-types/'+c['task']+'/detect',dict(max_results=1,timeout_seconds=15),'POST',timeout=20)
    proposals=report.get('proposals',[])
    if report.get('count')!=1 or len(proposals)!=1:raise ValueError('fixture has no unique actual maintenance proposal')
    p=proposals[0];params=p.get('parameters',{})
    if str(params.get('volume_id',{}).get('int64_value',''))!=manifest['vid'] or params.get('collection',{}).get('string_value')!=c['collection']:
        raise ValueError('foreign maintenance proposal')
    write(root/'proposal.json',p,True)
    return p


def scope(c, parent):
    if not re.fullmatch(r'[1-9][0-9]*', c['vid']):
        raise ValueError('noncanonical target VID')
    folder = parent / 'volume'
    if folder.is_symlink() or folder.resolve() != folder:
        raise ValueError('foreign volume directory')
    prefix = c['collection'] + '_' + c['vid']
    files = []
    raw = {}
    for p in sorted(folder.glob(prefix + '.*')):
        if p.is_symlink() or not p.is_file() or p.stat().st_nlink != 1:
            raise ValueError('non-exclusive target file')
        st = p.stat()
        item = dict(name=p.name, size=st.st_size, inode=st.st_ino, uid=st.st_uid)
        if c.get('hash_files'):
            item['sha256'] = hashlib.sha256(p.read_bytes()).hexdigest()
        if re.fullmatch(re.escape(prefix)+r'\.(?:active|prepared|last_committed|vif(?:\.v[1-9][0-9]*)?)', p.name):
            body = p.read_bytes()
            if len(body)>LIMIT:raise ValueError('protocol metadata cap')
            raw[p.name] = strict(body)
        files.append(item)
    active = raw.get(prefix+'.active')
    generation = active['version'] if active is not None else 0
    if type(generation) is not int or generation<0:raise ValueError('invalid active generation')
    vif = raw.get(prefix+'.vif'+('.v%d'%generation if generation else ''),raw.get(prefix+'.vif',{}))
    cfg = vif.get('ecShardConfig',vif.get('ec_shard_config',{}))
    encode_ns = str(cfg.get('encodeTsNs',cfg.get('encode_ts_ns','')))
    shards=[]
    for item in files:
        match=re.fullmatch(re.escape(prefix)+r'\.ec([0-9]{2})'+(r'\.v'+str(generation) if generation else ''),item['name'])
        if match:shards.append(int(match[1]))
    if any(x>=14 for x in shards):raise ValueError('foreign shard index')
    return dict(collection=c['collection'],vid=c['vid'],generation=generation,encode_ns=encode_ns,
                shards=shards,files=files,metadata=raw,pending=int(prefix+'.prepared' in raw),
                ordinary=any(x['name']==prefix+'.dat' for x in files))


def quarantine(c, parent):
    # Setup-only: controller already proved this exact service generation GONE.
    # Prevalidate every fixed member before any rename; adjacent data is untouched.
    current=scope(dict(c,hash_files=True),parent)
    selected=[]
    for expected in c['files']:
        matches=[x for x in current['files'] if x['name']==expected['name']]
        if len(matches)!=1 or matches[0]!=expected:raise ValueError('fixture source identity/hash changed')
        selected.append(parent/'volume'/expected['name'])
    if not selected:raise ValueError('empty missing-copy fixture')
    target=parent/'maintenance-quarantine'
    target.mkdir(mode=0o700)
    for p in selected:os.rename(p,target/p.name)
    for directory in [parent/'volume',target]:
        fd=os.open(directory,os.O_RDONLY|os.O_DIRECTORY);os.fsync(fd);os.close(fd)
    return dict(renamed=[p.name for p in selected],retained=True)


def proc_identity(pid):
    p = pathlib.Path('/proc') / str(pid)
    fields = (p / 'stat').read_text().rsplit(') ', 1)[1].split()
    return dict(pid=pid, start=fields[19], boot=pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip())


def transfer_progress(text, vid, state, at):
    # Every announced copy is a new operation, including another copy of the
    # SAME VID. Joining two copies across their idle gap would fake overlap.
    if re.search(r'\b(?:moving \S+ volume \S+|replicating volume [0-9]+|copying volume [0-9]+)\b', text) or re.search(r'\bmoved volume [0-9]+\b', text):
        state['phase'] = None
    match = re.search(r'\bvolume ([0-9]+) processed ([0-9]+(?:\.[0-9]+)?)\s*([KMGTPE]?i?B)(?: bytes)?(?:\s|$)', text)
    if match and match[1] == vid:
        unit = match[3]
        power = 'KMGTPE'.index(unit[0])+1 if unit[0] != 'B' else 0
        amount = float(match[2]) * ((1024 if 'i' in unit else 1000)**power)
        if math.isfinite(amount) and amount >= 1:
            if state['phase'] is None:
                state['operation'] += 1
                state['phase'] = dict(begin=at, bytes=int(amount))
            state['phase']['bytes'] = max(state['phase']['bytes'], int(amount))
    phase = state['phase']
    return dict(state='in_flight' if phase else 'idle', vid=vid,
                operation=str(state['operation']), bytes=phase['bytes'] if phase else 0,
                begin=phase['begin'] if phase else 0)


def task(c, root):
    manifest = strict((root / 'manifest.json').read_bytes())
    identity = proc_identity(os.getpid())
    identity.update(task=c['task'], run_id=c['run_id'], config_sha256=c['config_sha256'])
    write(root / 'identity.json', identity, True)
    start = time.monotonic()
    transcript = root / 'task.log'
    events = root / 'events.jsonl'
    job = c['run_id'] + '-maintenance'
    if c['task'].startswith('ec_'):
        proposal=strict((root/'proposal.json').read_bytes())
        params=proposal['parameters']
        if str(params.get('volume_id',{}).get('int64_value',''))!=manifest['vid'] or params.get('collection',{}).get('string_value')!=c['collection']:
            raise ValueError('foreign stored proposal')
        request_body = dict(job=dict(job_id=job, job_type=c['task'],parameters=params),attempt=1,timeout_seconds=570)
        argv = None
    else:
        command = ('volume.balance -apply -collection=^' + c['collection'] + '$ -maxParallelization=1 -ioBytePerSecond=1048576'
                   if c['task'] == 'volume.balance' else
                   'volume.fix.replication -apply -collectionPattern=' + c['collection'] + ' -maxParallelization=1 -maxParallelizationPerServer=1 -retry=0')
        argv = [c['weed'], 'shell', '-master=' + c['master']]
    result = dict(job=job, attempt='1', terminal='unknown', refusal='', effects_known=False)
    try:
        if argv:
            process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                       stderr=subprocess.STDOUT, start_new_session=True)
            write(root/'child.json',proc_identity(process.pid),True)
            try:
                process.stdin.write(('lock\n' + command + '\nunlock\n').encode())
                process.stdin.close()
                selector = selectors.DefaultSelector(); selector.register(process.stdout, selectors.EVENT_READ)
                retained = bytearray(); pending = bytearray(); progress_state=dict(phase=None,operation=0)
                with events.open('xb', buffering=0) as out, transcript.open('xb', buffering=0) as log:
                    while selector.get_map():
                        if time.monotonic() - start > 570:
                            raise TimeoutError('task570s')
                        for key, _ in selector.select(.1):
                            chunk = os.read(key.fd, 65536)
                            if not chunk:
                                selector.unregister(key.fileobj); continue
                            retained.extend(chunk); pending.extend(chunk);log.write(chunk)
                            if len(retained) > LIMIT:
                                raise ValueError('task output cap')
                            while b'\n' in pending:
                                line, _, pending = pending.partition(b'\n')
                                text = line.decode('utf-8', 'replace')
                                at = time.monotonic()
                                progress=transfer_progress(text,manifest['vid'],progress_state,at)
                                write(root/'progress.json',progress)
                                if progress['state']=='in_flight':
                                    out.write(json.dumps(dict(event='actual_progress', at=at, vid=manifest['vid'], bytes=progress['bytes'])).encode()+b'\n')
                    rc = process.wait(timeout=max(.01, 570 - (time.monotonic()-start)))
                    write(root/'progress.json',dict(state='idle',vid=manifest['vid'],operation=str(progress_state['operation']),bytes=0,begin=0))
                text = retained.decode('utf-8', 'replace')
                result.update(exit=rc, terminal='succeeded' if rc == 0 and not re.search(r'(?im)^(?:error|failed)\b', text) else 'failed')
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGTERM)
                    try: process.wait(timeout=2)
                    except subprocess.TimeoutExpired:
                        os.killpg(process.pid, signal.SIGKILL);process.wait(timeout=2)
        else:
            # The blocking execute RPC is bounded; the controller's separate
            # snapshot stage reads the same admin's job/activities while it runs.
            status, raw = request(c['admin'] + '/api/plugin/jobs/execute', request_body, 'POST', timeout=570)
            transcript.write_bytes(raw)
            done = strict(raw)
            completion = done.get('completion', done)
            result.update(terminal='succeeded' if status == 200 and completion.get('success') is True else 'failed', completion=done)
    except Exception as error:
        result['diagnostic'] = type(error).__name__ + ': ' + str(error)[:1000]
    result.update(ended=time.monotonic(), started=start, identity=identity)
    write(root / 'result.json', result, True)
    return result


def sample(c, root):
    before = time.monotonic()
    identity = strict((root / 'identity.json').read_bytes()) if (root / 'identity.json').exists() else None
    alive = False
    if identity:
        try:
            alive = all(proc_identity(identity['pid'])[k] == identity[k] for k in ('pid', 'start', 'boot'))
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            pass
    report = dict(before=before, after=time.monotonic(), identity=identity, alive=alive,
                  boot=pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
                  result=strict((root/'result.json').read_bytes()) if (root/'result.json').exists() else None)
    report['progress']=None
    if c['task'].startswith('ec_'):
        job = c['run_id'] + '-maintenance'
        status, body = request(c['admin'] + '/api/plugin/jobs/' + job + '/detail',timeout=5)
        report['job'] = strict(body) if status==200 else None
        if status not in (200,404):raise ValueError('job detail unreadable status%d'%status)
        report['scratch'] = []
        scratch = root / 'worker/task_scratch'
        for p in scratch.rglob('*') if scratch.exists() else []:
            if p.is_symlink(): raise ValueError('symlink in task scratch')
            if p.is_file(): report['scratch'].append(dict(path=str(p.relative_to(scratch)), size=p.stat().st_size))
    else:
        report['events'] = [strict(x) for x in (root/'events.jsonl').read_bytes().splitlines()] if (root/'events.jsonl').exists() else []
        if alive and (root/'child.json').exists():
            child=strict((root/'child.json').read_bytes())
            try:
                observed=proc_identity(child['pid'])
                args=(pathlib.Path('/proc')/str(child['pid'])/'cmdline').read_bytes().split(b'\0')
                if observed==child and args[:3]==[c['weed'].encode(),b'shell',('-master='+c['master']).encode()] and hashlib.sha256(pathlib.Path(c['weed']).read_bytes()).hexdigest()==c['weed_sha256']:
                    report['progress']=strict((root/'progress.json').read_bytes()) if (root/'progress.json').exists() else None
            except (FileNotFoundError,ProcessLookupError,PermissionError):pass
    report['after'] = time.monotonic()
    return report


def main():
    c = strict(sys.argv[1])
    if c['task'] not in TASKS or not re.fullmatch(r'[a-z0-9][a-z0-9-]{0,60}', c['collection']):
        raise ValueError('unadmitted maintenance task/collection')
    parent = pathlib.Path(c['root'])
    if parent.resolve() != parent or parent.is_symlink() or not parent.is_dir():
        raise ValueError('foreign run root')
    if c['stage'] in ('scope','quarantine'):
        action={'scope':scope,'quarantine':quarantine}[c['stage']]
        print(json.dumps(action(c,parent),allow_nan=False),flush=True);return
    root = parent / 'maintenance'
    if c['stage']=='init':
        root.mkdir(mode=0o700)
        write(root/'config.json',dict(run_id=c['run_id'],task=c['task'],config_sha256=c['config_sha256']),True)
        print(json.dumps(dict(initialized=True)),flush=True);return
    if root.is_symlink() or root.stat().st_uid != os.getuid() or root.stat().st_mode & 0o077:
        raise ValueError('foreign maintenance directory')
    owned=strict((root/'config.json').read_bytes())
    if owned!=dict(run_id=c['run_id'],task=c['task'],config_sha256=c['config_sha256']):raise ValueError('foreign maintenance config')
    action = {'seed': seed,'plugin_preflight':plugin_preflight,'detect':detect, 'encode':encode, 'task': task, 'sample': sample, 'readback': readback}.get(c['stage'])
    if action is None: raise ValueError('unknown maintenance stage')
    print(json.dumps(action(c, root), allow_nan=False), flush=True)


if __name__ == '__main__': main()
