"""Explicit CPU controls; discovery/import never contacts hosts or imports native."""
import copy, datetime, importlib.util, json, os, pathlib, sys, unittest, tempfile, subprocess, shutil
ROOT=pathlib.Path(__file__).resolve().parent
sys.path.insert(0,str(ROOT))
import kv_smoke

class FakeConnector:
    def __init__(self,mode='good'):
        self.mode=mode;self.r,self.w=os.pipe();self.pending=[];self.future=0;self.values={};self.sets=0
        self.stats={'read_errors':0,'loader':dict(transport='rdma',http_fallback_allowed=False,**{x:0 for x in ('read_bytes','read_objects','direct_rc_bytes','direct_rc_reads','direct_dc_bytes','direct_dc_reads','copied_bytes','copied_reads','write_bytes','write_objects')})}
    def event_fd(self):return self.r
    def stats_json(self):return json.dumps(self.stats)
    def submit_batch_set(self,keys,buffers):
        self.sets+=1;self.values[keys[0]]=bytes(buffers[0]);self.stats['loader']['write_bytes']+=len(buffers[0]);self.stats['loader']['write_objects']+=1
        return self.complete(self.mode!='ambiguous',None,'connection lost' if self.mode=='ambiguous' else '')
    def submit_batch_get(self,keys,buffers):
        data=self.values[keys[0]];n=len(data)
        if self.mode!='miss':buffers[0][:]=data if self.mode!='wrong' else b'x'*n
        route='copied' if self.mode=='http' else 'direct_dc'
        s=self.stats['loader'];s['read_bytes']+=n;s['read_objects']+=1;s[route+'_bytes']+=n;s[route+'_reads']+=1
        return self.complete(True,[self.mode!='miss'],'')
    def complete(self,ok,results,error):
        self.future+=1;self.pending.append((self.future,ok,error,results));os.write(self.w,b'x');return self.future
    def drain_completions(self):os.read(self.r,1);out=self.pending;self.pending=[];return out
    def close(self):os.close(self.r);os.close(self.w)

class SmokeControls(unittest.TestCase):
    def test_exact_helpers_win_over_competing_import_packages(self):
        with tempfile.TemporaryDirectory() as temp:
            root=pathlib.Path(temp)
            for name in ('kv_smoke.py','test_kv_smoke.py','soak_native_worker.py','soak_oracle.py'):
                shutil.copyfile(ROOT/name,root/name)
            for name in ('soak_native_worker','soak_oracle'):
                (root/name).mkdir()
                (root/name/'__init__.py').write_text("raise AssertionError('shadow package executed')\n")
            code="import sys;sys.path.insert(0,sys.argv[1]);import test_kv_smoke;t=test_kv_smoke.SmokeControls();r,_,_=t.run_mode('good');assert r['status']=='PASS'"
            result=subprocess.run([sys.executable,'-I','-B','-c',code,str(root)],capture_output=True,text=True,timeout=10)
            self.assertEqual(result.returncode,0,result.stderr)
    def run_mode(self,mode):
        c=dict(run_id='cpu',client='m01',transport='dc',keys=2,length=64,root='/testops/cpu/m01')
        conn=FakeConnector(mode);events=[]
        metadata=lambda key:dict(FileSize=64,Mtime=datetime.datetime.now(datetime.timezone.utc).isoformat(),chunks=[{'file_id':'1,abc123'}])
        try:return kv_smoke.run(conn,c,metadata,events.append),events,conn
        finally:conn.close()
    def test_ack_and_exact_with_direct_transport(self):
        result,events,_=self.run_mode('good');self.assertEqual((result['ack'],result['exact'],result['wrong']),(2,2,0));self.assertEqual([e['event'] for e in events],['ACK','EXACT','ACK','EXACT'])
    def test_wrong_latest_ack_bytes_fail(self):
        with self.assertRaisesRegex(AssertionError,'wrong latest ACK'):self.run_mode('wrong')
    def test_live_key_miss_fails(self):
        with self.assertRaisesRegex(AssertionError,'miss/error'):self.run_mode('miss')
    def test_correct_bytes_over_http_still_fail(self):
        with self.assertRaisesRegex(AssertionError,'transport witness'):self.run_mode('http')
    def test_ambiguous_write_fails_without_retry(self):
        c=dict(run_id='cpu',client='m01',transport='dc',keys=2,length=64,root='/testops/cpu/m01');conn=FakeConnector('ambiguous')
        try:
            with self.assertRaisesRegex(AssertionError,'never retried'):kv_smoke.run(conn,c,lambda key:None,lambda row:None)
            self.assertEqual(conn.sets,1)
        finally:conn.close()
    def test_false_fallback_claim_and_missing_counter_fail(self):
        base=FakeConnector();before=copy.deepcopy(base.stats);base.close()
        for mode in ('fallback','missing','negative'):
            after=copy.deepcopy(before)
            if mode=='fallback':after['loader']['http_fallback_allowed']=True
            elif mode=='missing':del after['loader']['direct_dc_bytes']
            else:after['loader']['read_bytes']=-1
            with self.subTest(mode=mode),self.assertRaises((AssertionError,KeyError)):kv_smoke.witness(before,after,dict(keys=1,length=1,transport='dc'))
    def test_unsupported_transport_refuses(self):
        with self.assertRaises(ValueError):kv_smoke.configure(dict(transport='http',keys=1,length=1))
if __name__=='__main__':unittest.main()
