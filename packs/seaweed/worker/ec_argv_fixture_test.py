"""Execute the Engine fixture's declared argv against local HTTP and a fake product.
No EC/product qualification: capture's /proc join has its separate real-process test.
"""
import hashlib,http.server,json,os,pathlib,subprocess,sys,tempfile,threading

def check(jobs):
 store={};state={'metadata_failure':False}
 class Handler(http.server.BaseHTTPRequestHandler):
  def do_PUT(self):
   body=self.rfile.read(int(self.headers.get('Content-Length','0')))
   if body:store[self.path]=body
   self.send_response(200);self.send_header('Content-Length','0');self.end_headers()
  def do_GET(self):
   if '?metadata=' in self.path:
    status=500 if state['metadata_failure'] else 200;body=json.dumps({'chunks':[{'file_id':'1,abcdef12345678'}]}).encode()
   else:
    body=next(iter(store.values())) if self.path=='/1,abcdef12345678' else store.get(self.path,b'')
    status=200 if body or self.path=='/' else 404
   self.send_response(status);self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
  def log_message(self,*args):pass
 server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler);thread=threading.Thread(target=server.serve_forever);thread.start()
 try:
  with tempfile.TemporaryDirectory() as tmp:
   root=pathlib.Path(tmp)/'run';root.mkdir(mode=0o700)
   product=pathlib.Path(tmp)/'product';product.write_text('#!/usr/bin/python3\nimport json,sys\nprint(json.dumps({"fixture_product_exec":True,"argv":sys.argv[1:]}))\n');product.chmod(0o700)
   digest=hashlib.sha256(product.read_bytes()).hexdigest();worker=pathlib.Path(__file__).parent
   base='http://127.0.0.1:'+str(server.server_port)
   argv={}
   for label,j in jobs.items():
    a=list(j['argv']);index=3 if a[2]=='--config' else 2;c=json.loads(a[index]);old=c['root']
    def relocate(v):
     if isinstance(v,str):return v.replace(old,str(root))
     if isinstance(v,list):return [relocate(x) for x in v]
     if isinstance(v,dict):return {k:relocate(x) for k,x in v.items()}
     return v
    a=relocate(a);c=relocate(c);c.update(exe=str(product),sha256=digest,weed=str(product),s3=base,filer_http=base,volume=base,urls=[base+'/'])
    a[1]=str(worker/pathlib.PurePosixPath(a[1]).name);a[index]=json.dumps(c);argv[label]=a
   def run(label,expected=0,args=None):
    r=subprocess.run([sys.executable]+(args or argv[label]),capture_output=True,text=True,timeout=10)
    assert r.returncode==expected,(label,r.returncode,r.stdout,r.stderr)
    return json.loads(r.stdout) if r.stdout.strip().startswith('{') else r.stdout
   for label in ['master','filer','s3']+['h'+str(i) for i in range(14)]:assert run(label)['fixture_product_exec']
   assert (root/'filer.toml').stat().st_mode&0o777==0o600
   run('ready');seed=run('seed');assert len(seed['objects'])==1 and seed['objects'][0]['ack']
   assert [json.loads(v)['event'] for v in (root/'seed.jsonl').read_text().splitlines()]==['SUBMIT','ACK']
   run('encode')
   for label in ['s1-s3','s1-volume']+['s2-'+str(i)+'-'+route for i in range(4) for route in ('s3','volume')]:
    row=run(label);assert row['wrong']==0 and all(x['exact'] for x in row['outcomes'])
    events=[json.loads(v)['event'] for v in (root/(label+'.jsonl')).read_text().splitlines()];assert events==['SUBMIT','OUTCOME']
   assert run('planted')['control']=='WRONG_BYTE_REJECTED'
   state['metadata_failure']=True;a=list(argv['seed']);a[a.index('--journal')+1]=str(root/'unknown.jsonl');a[a.index('--manifest')+1]=str(root/'unknown-manifest.json')
   run('seed',1,a)
   assert [json.loads(v)['event'] for v in (root/'unknown.jsonl').read_text().splitlines()]==['SUBMIT','ACK']
   assert not (root/'unknown-manifest.json').exists()
 finally:server.shutdown();server.server_close();thread.join()

if __name__=='__main__':
 os.umask(0o002)
 check(json.loads(pathlib.Path(sys.argv[1]).read_text()))
 print('DECLARED_ARGV_COMPATIBILITY_PASS: service exec, ready, seed ACK/unknown persistence, encode, S3/direct reads, plant, terminal journals; CPU fixture only')
