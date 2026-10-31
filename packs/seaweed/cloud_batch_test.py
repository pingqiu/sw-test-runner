"""Local process/HTTP-boundary controls; no native, cloud or lab calls."""
import contextlib, importlib.util, io, json, pathlib, subprocess, threading, unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('cloud_batch', pathlib.Path(__file__).with_name('worker')/'cloud_batch.py')
batch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(batch)

class CloudBatchTests(unittest.TestCase):
    def exercise(self, mode):
        store = {}
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args): pass
            def do_PUT(self):
                store[self.path] = self.rfile.read(int(self.headers.get('Content-Length',0)))
                self.send_response(200); self.end_headers()
            def do_GET(self):
                if self.path.startswith('/dir/lookup?'):
                    body = json.dumps(dict(locations=[dict(url='10.84.1.21:8080')])).encode()
                else:
                    body = b'corrupt' if mode=='wrong-s3' else store[self.path]
                self.send_response(200); self.end_headers(); self.wfile.write(body)
        server = ThreadingHTTPServer(('127.0.0.1',0), Handler)
        thread = threading.Thread(target=server.serve_forever,daemon=True); thread.start()
        config = dict(transport='http',client='client1',run_id='cpu-only',keys=16,length=1048576)
        calls = []
        def kv(argv, **kwargs):
            calls.append(argv)
            self.assertEqual(argv[:2],[batch.sys.executable,'-I'])
            self.assertEqual(pathlib.Path(argv[2]).name,'kv_smoke.py')
            self.assertTrue(pathlib.Path(argv[2]).is_file())
            if mode=='kv-error': return subprocess.CompletedProcess(argv,1,'explicit failed SET','')
            c = json.loads(argv[-1]); rows = []
            for i in range(16):
                for event in ('ACK','EXACT'):
                    rows.append(dict(event=event,key=c['client']+'-'+str(i),length=1048576,sha256='a'*64,fids=['1,abc']))
            rows.append(dict(status='PASS',ack=16,exact=16,wrong=0))
            return subprocess.CompletedProcess(argv,0,'\n'.join(map(json.dumps,rows)),'')
        url = 'http://127.0.0.1:'+str(server.server_port)
        out = io.StringIO()
        try:
            with patch.object(batch.sys,'argv',['cloud_batch','--config',json.dumps(config),'--s3',url,'--master',url]), patch.object(batch.subprocess,'run',side_effect=kv), patch.object(batch.time,'sleep'), contextlib.redirect_stdout(out):
                if mode=='ok': batch.main()
                else:
                    with self.assertRaises((AssertionError,RuntimeError)): batch.main()
        finally:
            server.shutdown(); server.server_close(); thread.join(timeout=2)
        rows = [json.loads(x) for x in out.getvalue().splitlines()]
        if mode=='ok':
            self.assertEqual(len(calls),32)
            self.assertEqual(rows[-1],dict(status='PASS',ack=512,exact=512,wrong=0,s3_ack=32,s3_exact=32))
            self.assertEqual(sum(r.get('event')=='S3_EXACT' for r in rows),32)
            self.assertEqual(sum(r.get('event')=='OWNER' for r in rows),1)
        else:
            self.assertEqual(len(calls),1)
            self.assertFalse(any(r.get('status')=='PASS' for r in rows))
            self.assertEqual(sum(r.get('event')=='S3_EXACT' for r in rows),0)
    def test_complete_http_roundtrips(self): self.exercise('ok')
    def test_wrong_s3_bytes_refuse(self): self.exercise('wrong-s3')
    def test_explicit_kv_failure_never_retries(self): self.exercise('kv-error')

if __name__=='__main__': unittest.main()
