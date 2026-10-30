"""Explicit CPU schema/real CLI Docker contract. Import does not build or run anything."""
import copy,hashlib,json,os,pathlib,subprocess,tempfile

def main():
 import jsonschema
 root=pathlib.Path(__file__).resolve().parent.parent
 schema=json.loads((root/'inventory/schema.json').read_text());jsonschema.Draft202012Validator.check_schema(schema)
 validator=jsonschema.Draft202012Validator(schema,format_checker=jsonschema.FormatChecker())
 fixture=json.loads((root/'examples/inventory/docker.requested.json').read_text());checks=0
 with tempfile.TemporaryDirectory(prefix='docker-inventory-cli-') as temp:
  temp=pathlib.Path(temp);binary=temp/('inventory.exe' if os.name=='nt' else 'inventory')
  subprocess.run(['go','build','-o',str(binary),'./cmd/seaweed-inventory'],cwd=root,check=True)
  def validate(d,expected,structural=True):
   nonlocal checks
   if structural:assert validator.is_valid(d)==expected,list(validator.iter_errors(d))
   path=temp/'candidate.json';path.write_text(json.dumps(d))
   p=subprocess.run([str(binary),'--json','validate',str(path)],capture_output=True,text=True,timeout=5)
   out=json.loads(p.stdout);assert p.returncode==(0 if expected else 2),(p.stdout,p.stderr)
   assert out['code']==('OK' if expected else 'INVENTORY_INVALID') and out['execution_authorized'] is False and out['remedy'];checks+=1
  def observed(request):
   d=copy.deepcopy(request);d['state']='resolved';parent=d['docker']['parent'];parent.update(boot_id='11111111-1111-1111-1111-111111111111',root_device='2049',root_inode='8000');d['docker']['network']['id']=format(999,'064x')
   facts=[]
   for i,h in enumerate(d['hosts'],1):
    h['management']['host_key']=parent['management']['host_key'];h['data'].update(nic='eth0',mtu=1500);h['container'].update(id=format(i,'064x'),netns_inode=str(10000+i))
    for m in h['container']['mounts']:m.update(source_device='2049',source_inode=str(9000+i))
    if h['rdma']['status']=='requested':h['rdma']=dict(status='observed',capabilities=['rc'],device='rxe0',port=1,gid_index=0,gid=format(i,'032x'),link_layer='ethernet',active_mtu=1024,fabric_evidence_sha256=format(88,'064x'))
    facts.append(dict(host_id=h['id'],boot_id=parent['boot_id'],captured_at='2026-10-30T00:00:00Z',evidence_sha256=format(i+88,'064x')))
   d['observation']=dict(requested_sha256=hashlib.sha256(json.dumps(request,separators=(',',':')).encode()).hexdigest(),hosts=facts)
   return d
  for rc in (False,True):
   request=copy.deepcopy(fixture)
   if rc:
    request['transport']='rc';request['docker']['rdma_mode']='rxe';request['docker']['unavailable_transports'][0]['reason']='rxe_no_dc'
    for h in request['hosts']:
     if h['roles'][0] in ('client','volume'):h['rdma']=dict(status='requested',capabilities=['rc'])
     if h['roles'][0]=='volume':h['services'] += [dict(name='rdma',port=18515),dict(name='rdma_control',port=18516)]
   d=observed(request);validate(request,True);validate(d,True)
   reqpath=temp/'requested.json';reqpath.write_bytes(json.dumps(request,separators=(',',':')).encode());obspath=temp/'observed.json';obspath.write_text(json.dumps(d))
   p=subprocess.run([str(binary),'--json','join',str(reqpath),str(obspath)],capture_output=True,text=True,timeout=5);out=json.loads(p.stdout);assert p.returncode==0 and out['code']=='OK' and out['execution_authorized'] is False;checks+=1
   # Otherwise valid observations may not replace a requested image pin.
   changed=copy.deepcopy(d);changed['hosts'][0]['container']['image_digest']='sha256:'+format(123,'064x');validate(changed,True);obspath.write_text(json.dumps(changed))
   p=subprocess.run([str(binary),'--json','join',str(reqpath),str(obspath)],capture_output=True,text=True,timeout=5);assert p.returncode==2 and json.loads(p.stdout)['code']=='INVENTORY_JOIN_REFUSED';checks+=1
  cases=[]
  for field,value in [('privileged',True),('id','1234'),('image_digest','latest')]:
   d=observed(fixture);d['hosts'][0]['container'][field]=value;cases.append(d)
  for field in ('parent_id','privileged','mounts'):
   d=copy.deepcopy(fixture);del d['hosts'][0]['container'][field];cases.append(d)
  d=copy.deepcopy(fixture);d['docker']=None;cases.append(d)
  d=copy.deepcopy(fixture);d['transport']='dc';cases.append(d)
  d=copy.deepcopy(fixture);d['target']='docker-sriov';cases.append(d)
  d=copy.deepcopy(fixture);d['docker']['unavailable_transports']=[];cases.append(d)
  d=copy.deepcopy(fixture);d['docker']['metadata_store']='leveldb2';cases.append(d)
  d=copy.deepcopy(fixture);d['docker']['parent']['Executor_Ref']='alias';cases.append(d)
  d=copy.deepcopy(fixture);d['hosts'][0]['container']['Privileged']=True;cases.append(d)
  d=copy.deepcopy(fixture);d['hosts'][5]['disks'][0]['format_allowed']=True;cases.append(d)
  d=copy.deepcopy(fixture);d['hosts'][5]['disks'][0]['device_id']='/dev/disk/by-id/not-exposed';cases.append(d)
  for d in cases:validate(d,False)
  for role in ('master','filer','volume','client','metadata'):
   d=copy.deepcopy(fixture);d['hosts'].remove(next(h for h in d['hosts'] if h['roles']==[role]));validate(d,False)
  # Cross-host reference/identity checks are semantic in Go, beyond structural schema.
  for field in ('parent_id','netns_inode'):
   d=observed(fixture);d['hosts'][1]['container'][field]=('other' if field=='parent_id' else d['hosts'][0]['container']['netns_inode']);validate(d,False,False)
  d=observed(fixture);d['hosts'][3]['metadata_host_id']='another-mysql';validate(d,False,False)
  for field,value in [('docker',None),('container',None),('metadata_host_id','')]:
   d=json.loads((root/'examples/inventory/http.requested.json').read_text())
   if field=='docker':d[field]=value
   else:d['hosts'][0][field]=value
   validate(d,False)
 print(json.dumps(dict(status='PASS',checks=checks,scope='schema/Decode/Join and real CLI only',docker_host_cloud_calls=0,authority_granted=False)))
if __name__=='__main__':main()
