"""Local HTTP fixtures for the thin EC leaf, no product processes or hosts."""
import importlib.util, pathlib, tempfile, unittest
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
  import http.server, threading, time
  spec=importlib.util.spec_from_file_location('ec_real',pathlib.Path(__file__).with_name('ec_chaos.py'));leaf=importlib.util.module_from_spec(spec);spec.loader.exec_module(leaf)
  class Handler(http.server.BaseHTTPRequestHandler):
   def do_GET(self):
    self.send_response(500 if self.path=='/500' else 200);self.end_headers()
    try:
     if self.path=='/slow':
      for i in range(20):self.wfile.write(b'x');self.wfile.flush();time.sleep(.02)
     else:self.wfile.write(b'a')
    except OSError:pass
   def log_message(self,*args):pass
  server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler);thread=threading.Thread(target=server.serve_forever);thread.start()
  try:
   url='http://127.0.0.1:'+str(server.server_port)
   self.assertEqual(leaf.request(url+'/ok'),(200,b'a'));self.assertEqual(leaf.request(url+'/500'),(500,b''))
   with mock.patch.object(leaf.chaos,'OP_LIMIT',.1):
    with self.assertRaises(TimeoutError):leaf.request(url+'/slow')
  finally:server.shutdown();server.server_close();thread.join()

if __name__=='__main__':unittest.main()
