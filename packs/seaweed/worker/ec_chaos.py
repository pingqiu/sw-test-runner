"""Stage1a S3-only leaf. Reuses chaos_load payloads and SUBMIT/ACK shapes.
No fault authority, lifecycle, EC capture, status excusal or exposure claim here.
"""
import argparse, hashlib, http.client, importlib.util, json, pathlib, sys, time
from urllib.error import HTTPError
from urllib.parse import quote, urlsplit
from urllib.request import Request, urlopen
spec=importlib.util.spec_from_file_location('ec_chaos_load',pathlib.Path(__file__).with_name('chaos_load.py'))
chaos=importlib.util.module_from_spec(spec);spec.loader.exec_module(chaos)


def request(url, data=None, method='GET'):
    parsed=urlsplit(url)
    if parsed.scheme!='http' or parsed.hostname!='127.0.0.1':raise ValueError('Stage1 requires M02 loopback HTTP only')
    deadline=time.monotonic()+chaos.OP_LIMIT
    try:
        with urlopen(Request(url,data=data,method=method),timeout=chaos.OP_LIMIT) as r:
            body=bytearray()
            while len(body)<=chaos.S3_LENGTH:
                remaining=deadline-time.monotonic()
                if remaining<=0:raise TimeoutError('whole-operation30s deadline')
                # read1 performs one underlying read; bound it by the REMAINING budget.
                r.fp.raw._sock.settimeout(remaining)
                chunk=r.read1(min(65536,chaos.S3_LENGTH+1-len(body)))
                if not chunk:break
                body.extend(chunk)
            if time.monotonic()>deadline:raise TimeoutError('whole-operation30s deadline')
            return r.status,bytes(body)
    except HTTPError as e:
        e.close();return e.code,b''


def journal(path):return pathlib.Path(path).open('x',buffering=1)
def emit(f,row):f.write(json.dumps(row,allow_nan=False)+'\n')

def seed(c,path):
    objects=[];bucket=c['s3'].rstrip('/')+'/'+chaos.bucket_name(c)
    status,_=request(bucket,b'','PUT')
    if status not in (200,201,409):raise ValueError('bucket status%d'%status)
    with journal(path) as f:
        for i in range(c['keys']):
            key='ec-%d'%i;body=chaos.s3_body(c,key)
            emit(f,dict(event='SUBMIT',op=i,kind='s3_put',key=key,started=time.monotonic()))
            status,_=request(bucket+'/'+key,body,'PUT')
            if status not in (200,201):raise ValueError('PUT%d outcome UNKNOWN, no replay'%status)
            # The ACK is retained even if the subsequent metadata lookup refuses.
            emit(f,dict(event='ACK',kind='s3',key=key))
            fids=chaos.fids_of(c,'/buckets/'+chaos.bucket_name(c)+'/'+key)
            if len(fids)!=1 or not fids[0]:raise ValueError('stage1a requires one frozen FID per object')
            objects.append(dict(key=key,fid=fids[0],length=len(body),sha256=hashlib.sha256(body).hexdigest(),ack=True))
    return dict(run_id=c['run_id'],objects=objects,exposure='NOT_OBSERVED')


def reads(c,manifest,route,path,plant=False):
    if manifest['run_id']!=c['run_id'] or route not in ('s3','volume'):raise ValueError('manifest/route mismatch')
    outcomes=[];wrong=0
    with journal(path) as f:
        for i,obj in enumerate(manifest['objects']):
            key=obj['key'];expected=chaos.s3_body(c,key)
            if not obj['ack'] or len(expected)!=obj['length'] or hashlib.sha256(expected).hexdigest()!=obj['sha256']:raise ValueError('manifest independent payload mismatch')
            url=(c['s3'].rstrip('/')+'/'+chaos.bucket_name(c)+'/'+quote(key,safe='')) if route=='s3' else c['volume'].rstrip('/')+'/'+quote(obj['fid'],safe=',')
            row=dict(op=i,kind='s3_get',key=key,started=time.monotonic(),route=route)
            emit(f,dict(event='SUBMIT',**row))
            status=0;body=b'';error=''
            try:status,body=request(url)
            except (OSError,http.client.HTTPException) as e:error=type(e).__name__+': '+str(e)
            if plant and i==0 and body:body=bytes([body[0]^1])+body[1:]
            ended=time.monotonic();bad=status==200 and body!=expected;wrong+=int(bad)
            row.update(ended=ended,exact=status==200 and body==expected and ended-row['started']<=chaos.OP_LIMIT,status=status,length=len(body),sha256=hashlib.sha256(body).hexdigest(),error=error)
            outcomes.append(row)
    return dict(status='OBSERVED',stage='ec-read',run_id=c['run_id'],outcomes=outcomes,wrong=wrong,exposure='NOT_OBSERVED',control='WRONG_BYTE_REJECTED' if plant and wrong==1 else '')


def main():
    p=argparse.ArgumentParser();p.add_argument('--config',required=True);p.add_argument('--stage',choices=('seed','read'),required=True);p.add_argument('--manifest',required=True);p.add_argument('--plant-wrong-byte',action='store_true');p.add_argument('--route',choices=('s3','volume'));p.add_argument('--journal',required=True);a=p.parse_args()
    try:
        c=json.loads(a.config)
        if any(not c[k].startswith('http://127.0.0.1:') for k in ('s3','filer_http','volume') if k in c):raise ValueError('M02 loopback URLs only')
        if not 1<=c['keys']<=128:raise ValueError('keys outside1..128')
        result=seed(c,a.journal) if a.stage=='seed' else reads(c,json.loads(pathlib.Path(a.manifest).read_bytes()),a.route,a.journal,a.plant_wrong_byte)
        if a.stage=='seed':
            with pathlib.Path(a.manifest).open('x') as f:
                __import__('os').fchmod(f.fileno(),0o600);json.dump(result,f);f.flush();__import__('os').fsync(f.fileno())
        print(json.dumps(result,allow_nan=False),flush=True)
        if a.plant_wrong_byte:return 0 if result.get('control')=='WRONG_BYTE_REJECTED' and sum(not r['exact'] for r in result['outcomes'])==1 else 1
        return 0 if a.stage=='seed' or result['wrong']==0 and all(r['exact'] for r in result['outcomes']) else 1
    except Exception as e:
        print(json.dumps(dict(status='FAIL',error=type(e).__name__+': '+str(e))),flush=True);return 1
if __name__=='__main__':sys.exit(main())
