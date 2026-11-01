"""Bounded cloud HTTP batches around the pinned KV CLI; no lifecycle or retries."""
import argparse, hashlib, json, pathlib, subprocess, sys, time
from urllib.request import Request, urlopen
from urllib.parse import urlencode

def main():
    p = argparse.ArgumentParser()
    p.add_argument('--config', required=True)
    p.add_argument('--s3', required=True)
    p.add_argument('--master', required=True)
    a = p.parse_args()
    c = json.loads(a.config)
    emit = lambda r: print(json.dumps(r, allow_nan=False), flush=True)
    assert c['transport'] == 'http'
    replicas = {'000': 1, '001': 2}[c.get('replication', '000')]
    bucket = a.s3 + '/testops-' + c['run_id'] + '-' + c['client']
    def request(url, data=None, method='GET'):
        with urlopen(Request(url, data=data, method=method), timeout=10) as r:
            return r.read(4*1024*1024+1)
    request(bucket, b'', 'PUT')
    started, seen = time.monotonic(), set()
    for i in range(32):
        cfg = dict(c, client=c['client']+'-b'+str(i))
        child = subprocess.run([sys.executable, '-I', str(pathlib.Path(__file__).with_name('kv_smoke.py')), '--config', json.dumps(cfg)], capture_output=True, text=True, timeout=24)
        if child.returncode:
            raise RuntimeError(child.stdout + child.stderr)
        rows = [json.loads(x) for x in child.stdout.splitlines()]
        assert rows[-1]['status'] == 'PASS' and (rows[-1]['ack'],rows[-1]['exact'],rows[-1]['wrong']) == (16,16,0)
        for row in rows[:-1]:
            emit(row)
            if row['event'] == 'ACK':
                for fid in row['fids']:
                    vid = fid.split(',')[0]
                    if vid not in seen:
                        lookup = json.loads(request(a.master+'/dir/lookup?'+urlencode(dict(volumeId=vid))))
                        urls = sorted(x['url'] for x in lookup['locations'])
                        assert len(urls) == replicas == len(set(urls)), 'volume copies differ from replication'
                        emit(dict(event='OWNER', volume=vid, url=urls[0]) if replicas == 1 else dict(event='OWNER', volume=vid, urls=urls))
                        seen.add(vid)
        key = bucket + '/' + str(i)
        body = hashlib.sha256((c['run_id']+c['client']+str(i)).encode()).digest()*131072
        digest = hashlib.sha256(body).hexdigest()
        request(key, body, 'PUT')
        emit(dict(event='S3_ACK',key=key,length=len(body),sha256=digest))
        assert request(key) == body, 'S3 wrong bytes'
        emit(dict(event='S3_EXACT',key=key,length=len(body),sha256=digest))
        time.sleep(max(0, started+(i+1)*27-time.monotonic()))
    emit(dict(status='PASS',ack=512,exact=512,wrong=0,s3_ack=32,s3_exact=32))

def before(root, hosts):
    root = pathlib.Path(root)
    topology = json.loads((root/'before/meta/status.json').read_text())['Topology']
    assert {n['Url'] for dc in topology['DataCenters'] for rack in dc['Racks'] for n in rack['DataNodes']} == set(hosts.values())
    for host in hosts:
        volumes = json.loads((root/'before'/host/'status.json').read_text())['Volumes']
        assert volumes and any(v['ReadOnly'] is False for v in volumes), 'no writable owner '+host

def assess(root, hosts, replicas=1):
    root = pathlib.Path(root)
    read = lambda p: json.loads((root/p).read_text())
    expected, owners, used, counters = set(hosts.values()), {}, set(), {}
    for phase in ('before','after'):
        topology = read(phase+'/meta/status.json')['Topology']
        assert {n['Url'] for dc in topology['DataCenters'] for rack in dc['Racks'] for n in rack['DataNodes']} == expected
    for host, url in hosts.items():
        before, after = [read(p+'/'+host+'/status.json')['Volumes'] for p in ('before','after')]
        assert before and after and any(v['ReadOnly'] is False for v in before)
        assert all(type(v[k]) is int and v[k]>=0 for v in before+after for k in ('Id','Size','FileCount'))
        delta = {k:sum(v[k] for v in after)-sum(v[k] for v in before) for k in ('Size','FileCount')}
        assert all(v>0 for v in delta.values()), 'idle owner '+host
        counters[host] = delta
        for v in after:
            owners.setdefault(str(v['Id']), set()).add(url)
            assert len(owners[str(v['Id'])]) <= replicas, 'duplicate volume owner'
    assert all(len(s) == replicas for s in owners.values()), 'volume copies differ from replication'
    for client in ('client1','client2','client3'):
        rows = [json.loads(x) for x in (root/'load'/client/'cloud-kv.jsonl').read_text().splitlines()]
        lookups = {r['volume']:set(r['urls'] if 'urls' in r else [r['url']]) for r in rows if r.get('event')=='OWNER'}
        assert all(owners[v]==urls for v,urls in lookups.items())
        for prefix,count in (('',512),('S3_',32)):
            writes = [r for r in rows if r.get('event')==prefix+'ACK']
            reads = [r for r in rows if r.get('event')==prefix+'EXACT']
            a = {r['key']:(r['length'],r['sha256']) for r in writes}
            b = {r['key']:(r['length'],r['sha256']) for r in reads}
            assert len(writes)==len(reads)==len(a)==count and a==b, 'missing/duplicate/wrong journal'
            assert all(n==(1048576 if not prefix else 4194304) and len(h)==64 and set(h)<=set('0123456789abcdef') for n,h in a.values())
            if not prefix:
                for r in writes:
                    assert r['fids']
                    for fid in r['fids']:
                        vid = fid.split(',')[0]
                        assert lookups[vid]==owners[vid]
                        used |= owners[vid]
    assert used==expected, 'owner not exercised'
    print(json.dumps(dict(status='PASS',ack=1536,exact=1536,wrong=0,s3_ack=96,s3_exact=96,owners=sorted(used),server_storage_deltas=counters,evidence_scope='client_ack_exact_owner_join_and_server_storage_deltas')))

if __name__ == '__main__':
    try:
        if sys.argv[1:2]==['--before']:
            before(sys.argv[2],json.loads(sys.argv[3]))
        elif sys.argv[1:2]==['--assess']:
            assess(sys.argv[2],json.loads(sys.argv[3]),int(sys.argv[4]) if len(sys.argv)>4 else 1)
        else:
            main()
    except Exception as e:
        print(json.dumps(dict(status='FAIL',error=str(e))), flush=True)
        sys.exit(1)
