"""Explicit CPU structural-schema/Go CLI contract check; import does no work."""
import copy,json,os,pathlib,subprocess,tempfile

def main():
 import jsonschema
 root=pathlib.Path(__file__).resolve().parent.parent
 schema=json.loads((root/'inventory/schema.json').read_text());jsonschema.Draft202012Validator.check_schema(schema)
 validator=jsonschema.Draft202012Validator(schema,format_checker=jsonschema.FormatChecker())
 checks=0
 with tempfile.TemporaryDirectory(prefix='inventory-cli-') as tmp:
  tmp=pathlib.Path(tmp);binary=tmp/('inventory.exe' if os.name=='nt' else 'inventory')
  subprocess.run(['go','build','-o',str(binary),'./cmd/seaweed-inventory'],cwd=root,check=True)
  def validate(doc,expected):
   nonlocal checks
   assert validator.is_valid(doc)==expected,list(validator.iter_errors(doc))
   path=tmp/'case.json';path.write_text(json.dumps(doc))
   result=subprocess.run([str(binary),'validate',str(path)],capture_output=True,text=True,timeout=5)
   assert (result.returncode==0)==expected,(result.stdout,result.stderr)
   envelope=json.loads(result.stdout);assert envelope['execution_authorized'] is False;assert envelope['code']==('OK' if expected else 'INVENTORY_INVALID');assert envelope['remedy']
   checks+=1
  for transport in ('http','dc'):
   reqpath=root/f'examples/inventory/{transport}.requested.json';obspath=root/f'examples/inventory/{transport}.resolved.json'
   req=json.loads(reqpath.read_text());obs=json.loads(obspath.read_text());validate(req,True);validate(obs,True)
   p=subprocess.run([str(binary),'join',str(reqpath),str(obspath)],capture_output=True,text=True,timeout=5);assert p.returncode==0,p.stderr;assert json.loads(p.stdout)['execution_authorized'] is False;checks+=1
   for key,value in [('transport','rdma'),('go',True),('hosts',None),('state','approved')]:
    bad=copy.deepcopy(req);bad[key]=value;validate(bad,False)
   bad=copy.deepcopy(req);bad['hosts'][0]['management']['private_key']='forbidden';validate(bad,False)
   bad=copy.deepcopy(req);del bad['hosts'][0]['disks'][0]['format_allowed'];validate(bad,False)
   bad=copy.deepcopy(obs);del bad['observation'];validate(bad,False)
   if transport=='dc':
    for name in ('device','port','gid_index','gid','link_layer','active_mtu','fabric_evidence_sha256'):
     bad=copy.deepcopy(obs);del bad['hosts'][0]['rdma'][name];validate(bad,False)
   # This is valid observation structure but must fail the separate request join.
   bad=copy.deepcopy(obs);bad['hosts'][0]['disks'][0]['format_allowed']=True
   assert validator.is_valid(bad);badpath=tmp/'changed-format.json';badpath.write_text(json.dumps(bad))
   p=subprocess.run([str(binary),'join',str(reqpath),str(badpath)],capture_output=True,text=True,timeout=5);assert p.returncode==2;checks+=1
  # Review cases: schema and the real compiled CLI must reject the same bytes.
  base=json.loads((root/'examples/inventory/http.requested.json').read_text())
  for name in ('envelope_alias','format_alias'):
   bad=copy.deepcopy(base)
   if name=='envelope_alias':bad['transport']='typo';bad['Transport']='http'
   else:bad['hosts'][0]['disks'][0]['Format_Allowed']=True
   validate(bad,False)
   # Valid request/resolution except the same alias on the observed input.
   observed=json.loads((root/'examples/inventory/http.resolved.json').read_text())
   if name=='envelope_alias':observed['transport']='typo';observed['Transport']='http'
   else:observed['hosts'][0]['disks'][0]['Format_Allowed']=True
   badpath=tmp/'alias-observed.json';badpath.write_text(json.dumps(observed))
   p=subprocess.run([str(binary),'--json','join',str(root/'examples/inventory/http.requested.json'),str(badpath)],capture_output=True,text=True,timeout=5)
   assert p.returncode==2 and json.loads(p.stdout)['code']=='INVENTORY_JOIN_REFUSED';checks+=1
  dc=json.loads((root/'examples/inventory/dc.requested.json').read_text())
  for field in ('device','port','gid','link_layer','active_mtu','fabric_evidence_sha256'):
   bad=copy.deepcopy(dc);bad['hosts'][0]['rdma'][field]=None;validate(bad,False)
  # Schema explicitly permits a requested nullable GID index; do not tighten it accidentally.
  bad=copy.deepcopy(dc);bad['hosts'][0]['rdma']['gid_index']=None;validate(bad,True)
  p=subprocess.run([str(binary),'run'],capture_output=True,text=True,timeout=5);assert p.returncode==2;checks+=1
 print(json.dumps(dict(status='PASS',checks=checks,network_calls=0,authority_granted=False)))
if __name__=='__main__':main()
