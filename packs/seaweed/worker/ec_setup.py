"""Run the pinned product's existing ec.encode, then the existing bounded readiness gates."""
import json,os,pathlib,subprocess,sys,time
from urllib.request import urlopen
c=json.loads(sys.argv[1])
os.environ.update(HOME=c['root'],TMPDIR=c['root']);os.chdir(c['root'])
if c['stage']=='ready':
    deadline=time.monotonic()+30
    for url in c['urls']:
        if not url.startswith('http://127.0.0.1:'):raise ValueError('M02 loopback readiness only')
        while True:
            try:
                with urlopen(url,timeout=min(2,max(.01,deadline-time.monotonic()))) as r:
                    if r.status!=200:raise ValueError('HTTP readiness status')
                break
            except OSError:
                if time.monotonic()>=deadline:raise TimeoutError('EC readiness30s')
                time.sleep(.1)
    print(json.dumps({'ready':True}),flush=True)
else:
    if not c['master'].startswith('127.0.0.1:'):raise ValueError('M02 loopback master only')
    m=json.loads(pathlib.Path(c['manifest']).read_bytes());volumes=sorted({o['fid'].split(',')[0] for o in m['objects']})
    if not volumes or any(not x.isdigit() for x in volumes):raise ValueError('volume id not canonical numeric')
    command='ec.encode -volumeIds='+','.join(volumes)+' -batchSize=0 -shardReplicaPlacement=001\n'
    subprocess.run([c['weed'],'shell','-master='+c['master']],input=command,text=True,check=True,timeout=180)
