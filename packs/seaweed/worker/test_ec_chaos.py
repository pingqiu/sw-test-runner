"""Local HTTP fixtures for the thin EC leaf, no product processes or hosts."""
import importlib.util, pathlib, tempfile, unittest, json, sys
from unittest import mock

class ECLeaf(unittest.TestCase):
 def test_ack_manifest_and_no_error_excusal(self):
  path=pathlib.Path(__file__).with_name('ec_chaos.py')
  spec=importlib.util.spec_from_file_location('ec_leaf',path);leaf=importlib.util.module_from_spec(spec);spec.loader.exec_module(leaf)
  c={'run_id':'cpu','keys':1,'s3':'http://s3','filer_http':'http://filer','generation':'capture-later'}
  with tempfile.TemporaryDirectory() as d:
   with mock.patch.object(leaf,'request',return_value=(200,leaf.chaos.s3_body(c,'ec-0'))):
    manifest={'run_id':'cpu','objects':[{'key':'ec-0','fid':'1,abc','length':262144,'sha256':__import__('hashlib').sha256(leaf.chaos.s3_body(c,'ec-0')).hexdigest(),'ack':True}]}
    row=leaf.reads(c,manifest,'s3',pathlib.Path(d)/'good.jsonl')
    self.assertTrue(row['outcomes'][0]['exact']);self.assertEqual(row['outcomes'][0]['status'],200)
   with mock.patch.object(leaf,'request',return_value=(500,b'')):
    row=leaf.reads(c,manifest,'s3',pathlib.Path(d)/'bad.jsonl')
    self.assertFalse(row['outcomes'][0]['exact']);self.assertEqual(row['outcomes'][0]['status'],500)
   with mock.patch.object(leaf,'request',return_value=(200,b'wrong')):
    row=leaf.reads(c,manifest,'s3',pathlib.Path(d)/'wrong.jsonl')
    self.assertFalse(row['outcomes'][0]['exact']);self.assertEqual(row['wrong'],1)
   c['observe_errors']=True;manifest['objects']*=2
   with mock.patch.object(leaf,'request',return_value=(200,b'wrong')) as req:
    row=leaf.reads(c,manifest,'s3',pathlib.Path(d)/'stop.jsonl')
    self.assertEqual(req.call_count,1);self.assertEqual(len(row['outcomes']),1)

class RealHTTP(unittest.TestCase):
 def test_status_and_whole_operation_deadline(self):
  import http.server, threading, time, subprocess, hashlib
  spec=importlib.util.spec_from_file_location('ec_real',pathlib.Path(__file__).with_name('ec_chaos.py'));leaf=importlib.util.module_from_spec(spec);spec.loader.exec_module(leaf)
  body=leaf.chaos.s3_body({'run_id':'cpu'},'ec-0')
  class Handler(http.server.BaseHTTPRequestHandler):
   def do_GET(self):
    if self.path in ('/empty','/eof','/chunked'):
     headers=b'Content-Length: 0\r\n' if self.path=='/empty' else b'Transfer-Encoding: chunked\r\n' if self.path=='/chunked' else b''
     self.wfile.write(b'HTTP/1.0 200 OK\r\n'+headers+b'\r\n'+(b'1\r\na\r\n0\r\n\r\n' if self.path=='/chunked' else b'a' if self.path=='/eof' else b''));return
    if self.path=='/headers':
     try:
      self.wfile.write(b'HTTP/1.0 200 OK\r\nX-Test: ');self.wfile.flush()
      for _ in range(50):self.wfile.write(b'x');self.wfile.flush();time.sleep(.02)
      self.wfile.write(b'\r\nContent-Length: 0\r\n\r\n');self.wfile.flush()
     except OSError:pass
     return
    self.send_response(500 if self.path=='/500' else 200);self.send_header('Content-Length',str(20 if self.path=='/slow' else len(body)+10 if getattr(self.server,'mode','exact')=='incomplete' else len(body) if self.path.startswith('/testops-') else 1));self.end_headers()
    try:
     if self.path=='/slow':
      for i in range(20):self.wfile.write(b'x');self.wfile.flush();time.sleep(.02)
     else:
      payload=body if self.path.startswith('/testops-') else b'a'
      if getattr(self.server,'mode','exact')=='corrupt':payload=bytes([payload[0]^2])+payload[1:]
      if getattr(self.server,'mode','exact')=='short':payload=b'wrong'
      self.wfile.write(payload)
    except OSError:pass
   def log_message(self,*args):pass
  server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler);thread=threading.Thread(target=server.serve_forever);thread.start()
  try:
   url='http://127.0.0.1:'+str(server.server_port)
   self.assertEqual(leaf.request(url+'/ok'),(200,b'a'));self.assertEqual(leaf.request(url+'/500'),(500,b''))
   self.assertEqual(leaf.request(url+'/empty'),(200,b''));self.assertEqual(leaf.request(url+'/eof'),(200,b'a'));self.assertEqual(leaf.request(url+'/chunked'),(200,b'a'))
   with tempfile.TemporaryDirectory() as tmp:
    config=dict(root=tmp,keys=1,run_id='cpu',s3=url,filer_http=url)
    manifest={'run_id':'cpu','objects':[dict(key='ec-0',fid='1,abc',length=len(body),sha256=hashlib.sha256(body).hexdigest(),ack=True)]}
    path=pathlib.Path(tmp)/'manifest.json';path.write_text(json.dumps(manifest))
    argv=[sys.executable,'-B',str(pathlib.Path(__file__).with_name('ec_chaos.py')),'--config',json.dumps(config),'--stage','read','--manifest',str(path),'--route','s3','--journal',str(pathlib.Path(tmp)/'control.jsonl'),'--plant-wrong-byte']
    result=subprocess.run(argv,capture_output=True,text=True,timeout=5)
    self.assertEqual(result.returncode,0,result.stdout+result.stderr)
    row=json.loads(result.stdout);self.assertEqual(row['control'],'WRONG_BYTE_REJECTED');self.assertEqual(row['wrong'],1)
    self.assertFalse(row['outcomes'][0]['exact'])
    self.assertTrue(row['outcomes'][0]['original']['exact'])
    journal=[json.loads(v) for v in (pathlib.Path(tmp)/'control.jsonl').read_text().splitlines()];self.assertEqual(journal[0]['event'],'SUBMIT')
    self.assertEqual(journal[1]['event'],'OUTCOME')
    for mode in ('corrupt','short','incomplete'):
     server.mode=mode;argv[argv.index('--journal')+1]=str(pathlib.Path(tmp)/(mode+'.jsonl'))
     bad=subprocess.run(argv,capture_output=True,text=True,timeout=5)
     self.assertEqual(bad.returncode,1,bad.stdout+bad.stderr)
     badrow=json.loads(bad.stdout);self.assertNotEqual(badrow.get('control'),'WRONG_BYTE_REJECTED')
     if mode=='incomplete':
      self.assertFalse(badrow['outcomes'][0]['exact']);self.assertIn('IncompleteRead',badrow['outcomes'][0]['error'])
      argv[-1:] = []
      argv[argv.index('--journal')+1]=str(pathlib.Path(tmp)/'incomplete-ordinary.jsonl')
      ordinary=subprocess.run(argv,capture_output=True,text=True,timeout=5)
      self.assertEqual(ordinary.returncode,1,ordinary.stdout+ordinary.stderr)
      self.assertFalse(json.loads(ordinary.stdout)['outcomes'][0]['exact'])
    server.mode='exact'
   with mock.patch.object(leaf.chaos,'OP_LIMIT',.1):
    with self.assertRaises(TimeoutError):leaf.request(url+'/slow')
    if hasattr(leaf.signal,'setitimer'):
     started=time.monotonic()
     with self.assertRaises(OSError):leaf.request(url+'/headers')
     self.assertLess(time.monotonic()-started,.5)
  finally:server.shutdown();server.server_close();thread.join()

if __name__=='__main__':unittest.main()
