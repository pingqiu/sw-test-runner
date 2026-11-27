import json,subprocess,sys,tempfile,unittest,os,threading
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
from pathlib import Path
SCRIPT=Path(__file__).resolve().parents[1]/'r4/quorum-stall.py'

class QuorumStallCLI(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
    def test_file_control_writer_records_real_completion_and_failed_write_without_ack(self):
        device=self.root/'device';device.write_bytes(bytes(8*4096))
        fake=self.root/'dd.py'
        fake.write_text("import sys\na=dict(x.split('=',1) for x in sys.argv[1:])\nif a['seek']=='1':sys.exit(17)\nwith open(a['if'],'rb') as src,open(a['of'],'r+b') as dst:dst.seek(int(a['seek'])*4096);dst.write(src.read())\n")
        env=dict(os.environ,R4_STALL_DD=json.dumps([sys.executable,str(fake)]))
        work=self.root/'writer'
        p=subprocess.run([sys.executable,str(SCRIPT),'writer','--work',str(work),'--device',str(device),
            '--max-ops','3','--pace-ms','1','--file-control'],env=env,capture_output=True,text=True,timeout=10)
        self.assertEqual(p.returncode,0,p.stderr)
        rows=[json.loads(x) for x in (work/'io.jsonl').read_text().splitlines()]
        self.assertEqual([x['ok'] for x in rows if x['kind']=='end'],[True,False,True])
        ledger=(work/'ledger').read_text();self.assertEqual(sum(x.startswith('ack ') for x in ledger.splitlines()),2)
        self.assertNotIn('ack op1 ',ledger)
        self.assertEqual(json.loads((work/'writer-stopped.json').read_text())['attempts'],3)



    def test_startup_wait_does_not_mistake_unpublished_writer_metadata_for_exit(self):
        p=subprocess.run([sys.executable,str(SCRIPT),'wait-acks','--work',str(self.root),'--minimum','1','--seconds','0.01'],capture_output=True,text=True,timeout=5)
        self.assertNotEqual(p.returncode,0);self.assertEqual(p.stderr.strip(),'no bounded ACK progress')

    def test_writer_samples_the_primary_peer_http_view_during_io(self):
        count=[0]
        class Handler(BaseHTTPRequestHandler):
            def log_message(self,*args):pass
            def do_GET(self):
                payload=dict(VolumeID='v1',ReplicaID='r1',Epoch=1,AuthorityRole='primary')
                if self.path=='/peers':
                    count[0]+=1
                    state='degraded' if 2<=count[0]<5 else 'healthy'
                    payload['Peers']=[dict(ReplicaID='r2',Epoch=1,State=state)]
                body=json.dumps(payload).encode();self.send_response(200);self.end_headers();self.wfile.write(body)
        server=ThreadingHTTPServer(('127.0.0.1',0),Handler)
        thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        try:
            device=self.root/'device';device.write_bytes(bytes(32*4096))
            url='http://127.0.0.1:'+str(server.server_port);work=self.root/'observed'
            fake=self.root/'observed-dd.py'
            fake.write_text("import sys\na=dict(x.split('=',1) for x in sys.argv[1:])\nwith open(a['if'],'rb') as src,open(a['of'],'r+b') as dst:dst.seek(int(a['seek'])*4096);dst.write(src.read())\n")
            env=dict(os.environ,R4_STALL_DD=json.dumps([sys.executable,str(fake)]))
            ready=subprocess.run([sys.executable,str(SCRIPT),'ready-peer','--status',url+'/status','--peers',url+'/peers','--replica','r2','--seconds','1'],capture_output=True,text=True,timeout=5)
            self.assertEqual(ready.returncode,0,ready.stderr);count[0]=0
            p=subprocess.run([sys.executable,str(SCRIPT),'writer','--work',str(work),'--device',str(device),'--max-ops','20','--pace-ms','30','--file-control','--status',url+'/status','--peers',url+'/peers','--replica','r2'],env=env,capture_output=True,text=True,timeout=10)
            self.assertEqual(p.returncode,0,p.stderr)
            rows=[json.loads(line) for line in (work/'peers.jsonl').read_text().splitlines()]
            self.assertTrue(all('error' not in r for r in rows),rows)
            self.assertEqual({r['peers']['Peers'][0]['State'] for r in rows},{'healthy','degraded'})
            self.assertEqual(len([r for r in (work/'ledger').read_text().splitlines() if r.startswith('ack ')]),20)
        finally:
            server.shutdown();server.server_close();thread.join(timeout=2)

if __name__=='__main__':unittest.main()
