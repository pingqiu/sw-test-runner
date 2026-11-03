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

class RealHTTP(unittest.TestCase):
 def test_status_and_whole_operation_deadline(self):
  import http.server, threading, time, subprocess, hashlib
  spec=importlib.util.spec_from_file_location('ec_real',pathlib.Path(__file__).with_name('ec_chaos.py'));leaf=importlib.util.module_from_spec(spec);spec.loader.exec_module(leaf)
  body=leaf.chaos.s3_body({'run_id':'cpu'},'ec-0')
  class Handler(http.server.BaseHTTPRequestHandler):
   def do_GET(self):
    if self.path=='/headers':
     try:
      self.wfile.write(b'HTTP/1.0 200 OK\r\nX-Test: ');self.wfile.flush()
      for _ in range(50):self.wfile.write(b'x');self.wfile.flush();time.sleep(.02)
      self.wfile.write(b'\r\nContent-Length: 0\r\n\r\n');self.wfile.flush()
     except OSError:pass
     return
    self.send_response(500 if self.path=='/500' else 200);self.send_header('Content-Length',str(20 if self.path=='/slow' else len(body) if self.path.startswith('/testops-') else 1));self.end_headers()
    try:
     if self.path=='/slow':
      for i in range(20):self.wfile.write(b'x');self.wfile.flush();time.sleep(.02)
     else:self.wfile.write(body if self.path.startswith('/testops-') else b'a')
    except OSError:pass
   def log_message(self,*args):pass
  server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler);thread=threading.Thread(target=server.serve_forever);thread.start()
  try:
   url='http://127.0.0.1:'+str(server.server_port)
   self.assertEqual(leaf.request(url+'/ok'),(200,b'a'));self.assertEqual(leaf.request(url+'/500'),(500,b''))
   with tempfile.TemporaryDirectory() as tmp:
    config=dict(root=tmp,keys=1,run_id='cpu',s3=url,filer_http=url)
    manifest={'run_id':'cpu','objects':[dict(key='ec-0',fid='1,abc',length=len(body),sha256=hashlib.sha256(body).hexdigest(),ack=True)]}
    path=pathlib.Path(tmp)/'manifest.json';path.write_text(json.dumps(manifest))
    argv=[sys.executable,'-B',str(pathlib.Path(__file__).with_name('ec_chaos.py')),'--config',json.dumps(config),'--stage','read','--manifest',str(path),'--route','s3','--journal',str(pathlib.Path(tmp)/'control.jsonl'),'--plant-wrong-byte']
    result=subprocess.run(argv,capture_output=True,text=True,timeout=5)
    self.assertEqual(result.returncode,0,result.stdout+result.stderr)
    row=json.loads(result.stdout);self.assertEqual(row['control'],'WRONG_BYTE_REJECTED');self.assertEqual(row['wrong'],1)
    self.assertFalse(row['outcomes'][0]['exact'])
    journal=[json.loads(v) for v in (pathlib.Path(tmp)/'control.jsonl').read_text().splitlines()];self.assertEqual(journal[0]['event'],'SUBMIT')
   with mock.patch.object(leaf.chaos,'OP_LIMIT',.1):
    with self.assertRaises(TimeoutError):leaf.request(url+'/slow')
    if hasattr(leaf.signal,'setitimer'):
     started=time.monotonic()
     with self.assertRaises(OSError):leaf.request(url+'/headers')
     self.assertLess(time.monotonic()-started,.5)
  finally:server.shutdown();server.server_close();thread.join()

if __name__=='__main__':unittest.main()
