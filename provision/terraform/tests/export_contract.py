"""CPU-only real Terraform output -> inventory CLI contract; import does no work.
The local state below is synthetic and contains no resources/provider config.
It does not prove apply, live observation, S0b admission or provider absence.
"""
import argparse, hashlib, json, os, pathlib, subprocess, tempfile

def main():
 p=argparse.ArgumentParser(description=__doc__)
 p.add_argument('--terraform',required=True)
 p.add_argument('--inventory-cli',required=True)
 p.add_argument('--fixtures',type=pathlib.Path,required=True)
 a=p.parse_args()
 tf=str(pathlib.Path(a.terraform).resolve());cli=str(pathlib.Path(a.inventory_cli).resolve())
 checks=0
 with tempfile.TemporaryDirectory(prefix='seaweed-export-') as folder:
  root=pathlib.Path(folder)
  env=dict(os.environ,TF_DATA_DIR=str(root/'tf-data'),TF_IN_AUTOMATION='1',CHECKPOINT_DISABLE='1')
  for transport in ('http','dc'):
   requested=json.loads((a.fixtures/(transport+'.requested.json')).read_text())
   resolved=json.loads((a.fixtures/(transport+'.resolved.json')).read_text())
   raw=json.dumps(requested,separators=(',',':')).encode()
   state={'version':4,'terraform_version':'1.17.1','serial':1,'lineage':'11111111-1111-1111-1111-111111111111','outputs':{'inventory_json':{'value':raw.decode(),'type':'string','sensitive':False}},'resources':[]}
   (root/'terraform.tfstate').write_text(json.dumps(state))
   # Real Terraform raw-string output, without a provider/init/plan/apply.
   result=subprocess.run([tf,'-chdir='+str(root),'output','-raw','inventory_json'],capture_output=True,check=True,timeout=15,env=env)
   assert result.stdout==raw;checks+=1
   reqpath=root/'requested.json';reqpath.write_bytes(result.stdout)
   got=subprocess.run([cli,'--json','validate',str(reqpath)],capture_output=True,timeout=5)
   obj=json.loads(got.stdout);assert got.returncode==0 and obj['code']=='OK' and obj['execution_authorized'] is False;checks+=1
   resolved['observation']['requested_sha256']=hashlib.sha256(result.stdout).hexdigest()
   obspath=root/'resolved.json';obspath.write_text(json.dumps(resolved))
   got=subprocess.run([cli,'--json','join',str(reqpath),str(obspath)],capture_output=True,timeout=5)
   obj=json.loads(got.stdout);assert got.returncode==0 and obj['code']=='OK' and obj['execution_authorized'] is False;checks+=1
   quoted=subprocess.run([tf,'-chdir='+str(root),'output','-json','inventory_json'],capture_output=True,check=True,timeout=15,env=env)
   reqpath.write_bytes(quoted.stdout)
   got=subprocess.run([cli,'--json','validate',str(reqpath)],capture_output=True,timeout=5)
   obj=json.loads(got.stdout);assert got.returncode==2 and obj['code']=='INVENTORY_INVALID';checks+=1
   # A reserialized payload is a different request: old observations must refuse.
   reqpath.write_text(json.dumps(requested,indent=2))
   got=subprocess.run([cli,'--json','join',str(reqpath),str(obspath)],capture_output=True,timeout=5)
   obj=json.loads(got.stdout);assert got.returncode==2 and obj['code']=='INVENTORY_JOIN_REFUSED';checks+=1
 print(json.dumps({'status':'PASS','checks':checks,'fixture_scope':'synthetic local state; real output/Decode/Join only','provider_plan_apply':False,'execution_authorized':False}))
if __name__=='__main__':main()
