import hashlib,http.client,http.server,importlib.util,json,os,pathlib,subprocess,sys,tempfile,threading,unittest
from unittest import mock
worker=pathlib.Path(__file__).parent
def load(name):
 s=importlib.util.spec_from_file_location(name,worker/(name+'.py'));m=importlib.util.module_from_spec(s);s.loader.exec_module(m);return m
class Regressions(unittest.TestCase):
 def test_R3_terminal_rows_survive_before_next_operation(self):
  leaf=load('ec_chaos');c=dict(run_id='cpu',s3='http://127.0.0.1:1',keys=2)
  objects=[]
  for i in range(2):
   key='ec-'+str(i);body=leaf.chaos.s3_body(c,key);objects.append(dict(key=key,fid='1,abc',length=len(body),sha256=hashlib.sha256(body).hexdigest(),ack=True))
  with tempfile.TemporaryDirectory() as d:
   path=pathlib.Path(d)/'journal'
   with mock.patch.object(leaf,'request',side_effect=[(200,leaf.chaos.s3_body(c,'ec-0')),TimeoutError('second operation')]):leaf.reads(c,dict(run_id='cpu',objects=objects),'s3',path)
   rows=[json.loads(v) for v in path.read_text().splitlines()]
   self.assertEqual([r.get('event') for r in rows],['SUBMIT','OUTCOME','SUBMIT','OUTCOME'])
   self.assertTrue(rows[1]['exact']);self.assertFalse(rows[3]['exact'])
 @unittest.skipUnless(sys.platform=='linux','requires real /proc holder identities')
 def test_R4_holder_local_metadata_required(self):
  cap=load('ec_capture')
  with tempfile.TemporaryDirectory() as tmp:
   root=pathlib.Path(tmp);holders={};procs=[]
   try:
    for i in range(14):
     d=root/('h'+str(i));d.mkdir(mode=0o700);holders[d.name]=str(d)
     (d/('bucket_1.ec%02d'%i)).write_bytes(b'shard')
     (d/'bucket_1.ecx').write_bytes(b'index')
     (d/'bucket_1.vif').write_text(json.dumps(dict(ecShardConfig=dict(dataShards=10,parityShards=4,encodeTsNs='12345'))))
     procs.append(subprocess.Popen([sys.executable,'-c','import time;time.sleep(20)','--dir='+str(d)]))
    c=dict(root=tmp,holders=holders,run_id='cpu',volume_exe=sys.executable,volume_sha256=hashlib.sha256(pathlib.Path(sys.executable).read_bytes()).hexdigest())
    m=dict(run_id='cpu',objects=[dict(fid='1,abc')]);cap.capture(c,m)
    (root/'h1/bucket_1.vif').unlink()
    with self.assertRaises(ValueError):cap.capture(c,m)
    vif=root/'h1/bucket_1.vif';good=(root/'h0/bucket_1.vif').read_bytes()
    vif.write_bytes(good);raw=json.loads(good);raw['ecShardConfig']['encodeTsNs']='12346';vif.write_text(json.dumps(raw))
    with self.assertRaisesRegex(ValueError,'generation'):cap.capture(c,m)
    vif.write_bytes(good);index=root/'h1/bucket_1.ecx';index.write_bytes(b'foreign index')
    with self.assertRaisesRegex(ValueError,'index'):cap.capture(c,m)
    index.unlink()
    with self.assertRaisesRegex(ValueError,'index'):cap.capture(c,m)
   finally:
    for p in procs:p.terminate()
    for p in procs:p.wait(timeout=3)
 def test_R5_real_http_eof_closed_response(self):
  leaf=load('ec_chaos')
  class Handler(http.server.BaseHTTPRequestHandler):
   def do_GET(self):self.send_response(200);self.send_header('Content-Length','1');self.end_headers();self.wfile.write(b'a')
   def log_message(self,*args):pass
  server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler);thread=threading.Thread(target=server.serve_forever);thread.start()
  original=http.client.HTTPResponse.read1
  def close_at_content_length(response,*args):
   data=original(response,*args)
   if response.length==0 and response.fp is not None:response._close_conn()
   return data
  try:
   with mock.patch.object(http.client.HTTPResponse,'read1',close_at_content_length):self.assertEqual(leaf.request('http://127.0.0.1:'+str(server.server_port)),(200,b'a'))
  finally:server.shutdown();server.server_close();thread.join()
if __name__=='__main__':unittest.main(verbosity=2)
