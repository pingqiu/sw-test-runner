"""Read-only independent joins over the local pack's settled evidence."""
from pathlib import Path
import json,tarfile,hashlib,math,collections
import sys
p=Path(sys.argv[1]);r=json.loads((p/'RESULT.json').read_bytes());ec=r['ec'];bundle=next((p/'worker-service/results').iterdir());a=json.loads((bundle/'artifacts/authority.json').read_bytes());assert r['worker_state']=='pass' and r['lease_outcome']=='RESOURCES_SETTLED';assert len(a['Jobs'])==32 and all(j['State']=='EXIT_SETTLED' and j['Exit']['Settled'] for j in a['Jobs'].values());assert json.loads((p/'disposal.json').read_bytes()) in (None,[])
with tarfile.open(p/'target-output.tar.gz') as tf:
 members={m.name.removeprefix('./'):m for m in tf.getmembers()};read=lambda n:tf.extractfile(members[n]).read();m=json.loads(read('manifest.json'));vids={o['fid'].split(',')[0] for o in m['objects']};assert len(m['objects'])==64 and vids=={'1'}
 expected={o['key']:hashlib.sha256((m['run_id']+'/'+o['key']).encode()).digest()*8192 for o in m['objects']}
 for o in m['objects']:assert o['ack'] and o['length']==262144 and o['sha256']==hashlib.sha256(expected[o['key']]).hexdigest()
 rows=[]
 for name in ['planted','s1-s3','s1-volume']+[f's2-{i}-{route}' for i in range(4) for route in ['s3','volume']]:
  events=[json.loads(s) for s in read(name+'.jsonl').splitlines()];sub={v['op']:v for v in events if v['event']=='SUBMIT'};outs=[v for v in events if v['event']=='OUTCOME'];assert len(events)==128 and len(sub)==len(outs)==64 and {v['op'] for v in outs}==set(range(64))
  delays=[]
  for v in outs:
   i=v['op'];key='ec-'+str(i);assert v['key']==key and sub[i]['key']==key and sub[i]['started']==v['started'];dt=v['ended']-v['started'];assert math.isfinite(dt) and 0<=dt<=30;delays.append(dt);check=v
   if name=='planted' and i==0:
    check=v['original'];mutated=bytes([expected[key][0]^1])+expected[key][1:];assert v['exact'] is False and v['sha256']==hashlib.sha256(mutated).hexdigest()
   assert check['exact'] is True and check['status']==200 and check['length']==262144 and check['sha256']==hashlib.sha256(expected[key]).hexdigest() and check['error']==''
  rows.append(dict(leaf=name,real_gets=64,original_exact_200=64,ordinary_exact_200=0 if name=='planted' else 64,intentionally_rejected=1 if name=='planted' else 0,ordinary_wrong=0,max_op_seconds=max(delays),mean_op_seconds=sum(delays)/64))
 seed=[json.loads(s) for s in read('seed.jsonl').splitlines()];assert len([v for v in seed if v['event']=='ACK'])==64
 files=ec['capture']['unrelated_volumes'];assert all(v['Volume'] not in vids for v in files)
 for v in files:
  raw=read(v['Job']+'/'+v['Name']);assert len(raw)==v['Bytes'] and hashlib.sha256(raw).hexdigest()==v['SHA256']
 assert not any(name.endswith('.dat') and name.split('/')[-1].split('.dat')[0].rsplit('_',1)[-1] in vids for name in members)
 losses=[];unavailable=[]
 for i,loss in enumerate(ec['losses']):
  victim='h'+str(i);assert loss['Plan']['victim']==victim and loss['Plan']['hold'] and loss['State']=='OLD_SETTLED' and loss['Intent']['Identity']==loss['Kill']['Identity']==loss['OldExit']['Identity'];assert loss['Kill']['AliveBefore'] and loss['OldExit']['Settled'] and loss['OldExit']['Signal']==9 and not loss['OldExit']['CleanupStopped'];shard=next(s for s in ec['capture']['shards'] if s['Job']==victim);unavailable.append(shard['Shard']);losses.append(dict(episode=i+1,victim=victim,pid=loss['Kill']['Identity']['PID'],unavailable_shards=sorted(unavailable),state=loss['State'],original_exit_signal=9,s3_exact=64,volume_exact=64,exposure='NOT_COVERED'))
 counts=collections.Counter((v['Exit']['Code'],v['Exit']['Signal'],v['Exit']['CleanupStopped']) for v in a['Jobs'].values());summary=dict(run_id=m['run_id'],bundle=bundle.name,scope='LOCAL real Go weed13230 S3 HTTP, not lab/Rust/RDMA/cloud',seed_ack=64,manifest_vids=sorted(vids),ordinary_exact_gets=640,ordinary_wrong=0,planted_original_exact=64,intentional_rejections=1,total_real_gets=704,rows=rows,losses=losses,unrelated_volume_files=files,all_jobs_settled=32,exit_counts=[dict(code=c[0],signal=c[1],cleanup_stopped=c[2],count=n) for c,n in counts.items()],forced_disposal_signals=0,s1=ec['s1'],s2=ec['s2'],exposure=ec['exposure'],mandatory_bytes_deadlines=ec['mandatory_bytes_deadlines'],lease=r['lease_outcome'])
(p/'INDEPENDENT-ROWS.json').write_text(json.dumps(summary,indent=2)+'\n');print(json.dumps(summary,indent=2))
