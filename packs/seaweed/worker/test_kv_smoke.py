"""Explicit CPU controls; discovery/import never contacts hosts or imports native."""
import copy, datetime, importlib.util, json, os, pathlib, sys, unittest, tempfile, subprocess, shutil, io, types, contextlib
from unittest import mock
ROOT=pathlib.Path(__file__).resolve().parent
sys.path.insert(0,str(ROOT))
import kv_smoke

class FakeConnector:
    def __init__(self,mode='good'):
        self.mode=mode;self.r,self.w=os.pipe();self.pending=[];self.future=0;self.values={};self.sets=0
        self.stats={'read_errors':0,'loader':dict(transport='rdma',http_fallback_allowed=False,**{x:0 for x in ('read_bytes','read_objects','direct_rc_bytes','direct_rc_reads','direct_dc_bytes','direct_dc_reads','copied_bytes','copied_reads','write_bytes','write_objects')})}
        if mode in ('explicit-http','http-with-dc'): self.stats['loader']['transport']='http'
    def event_fd(self):return self.r
    def stats_json(self):return json.dumps(self.stats)
    def submit_batch_set(self,keys,buffers):
        self.sets+=1;self.values[keys[0]]=bytes(buffers[0]);self.stats['loader']['write_bytes']+=len(buffers[0]);self.stats['loader']['write_objects']+=1
        return self.complete(self.mode!='ambiguous',None,'connection lost' if self.mode=='ambiguous' else '')
    def submit_batch_get(self,keys,buffers):
        data=self.values[keys[0]];n=len(data)
        if self.mode!='miss':buffers[0][:]=data if self.mode!='wrong' else b'x'*n
        route='copied' if self.mode in ('http','explicit-http') or getattr(self,'http',False) else 'direct_dc'
        s=self.stats['loader'];s['read_bytes']+=n;s['read_objects']+=1;s[route+'_bytes']+=n;s[route+'_reads']+=1
        return self.complete(True,[self.mode!='miss'],'')
    def complete(self,ok,results,error):
        self.future+=1;self.pending.append((self.future,ok,error,results));os.write(self.w,b'x');return self.future
    def drain_completions(self):os.read(self.r,1);out=self.pending;self.pending=[];return out
    def close(self):os.close(self.r);os.close(self.w)

class SmokeControls(unittest.TestCase):
    def test_http_main_constructs_explicit_loader_and_scrubs_rdma_environment(self):
        c=dict(run_id='cpu',client='m02',transport='http',keys=1,length=64,root='/testops/cpu/m02',helper_dir=str(ROOT),native_dir='/unused-native',filer_grpc='192.0.2.2:18888',filer_http='http://192.0.2.2:8888')
        calls=[]
        class Native(FakeConnector):
            def __init__(self, endpoint, backing, concurrency, options):
                calls.append((endpoint,backing,concurrency,json.loads(options)))
                super().__init__('explicit-http')
        package=types.ModuleType('seaweedkv_native');package.__path__=[]
        native=types.ModuleType('seaweedkv_native._native');native.SeaweedKVConnector=Native
        metadata=lambda *a,**kw:io.BytesIO(json.dumps(dict(FileSize=64,Mtime=datetime.datetime.now(datetime.timezone.utc).isoformat(),chunks=[{'file_id':'1,abc123'}])).encode())
        out=io.StringIO()
        with mock.patch.dict(sys.modules,{'seaweedkv_native':package,'seaweedkv_native._native':native}),mock.patch.object(sys,'argv',['kv_smoke','--config',json.dumps(c)]),mock.patch.object(sys,'path',list(sys.path)),mock.patch.dict(os.environ,{'SW_RDMA_KVCACHE_PUSH_DC':'1'}),mock.patch.object(kv_smoke,'urlopen',metadata),contextlib.redirect_stdout(out):
            self.assertEqual(kv_smoke.main(),0)
            self.assertFalse(any(k.startswith('SW_RDMA_') for k in os.environ))
        self.assertEqual(calls[0][:3],(c['filer_grpc'],'loader-http',4))
        summary=json.loads(out.getvalue().splitlines()[-1])
        self.assertEqual((summary['status'],summary['ack'],summary['exact'],summary['witness']['read_transport']),('PASS',1,1,'http'))
    def test_http_selector_conflict_refuses(self):
        with self.assertRaisesRegex(ValueError,'RDMA selectors'):
            kv_smoke.configure(dict(transport='http',keys=1,length=1,gid_index=0))
    def test_http_dry_run_requires_no_native_module_or_endpoints(self):
        result=subprocess.run([sys.executable,'-I','-B',str(ROOT/'kv_smoke.py'),'--config',json.dumps(dict(transport='http',keys=1,length=64)),'--dry-run'],capture_output=True,text=True,timeout=5)
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        row=json.loads(result.stdout)
        self.assertEqual((row['status'],row['backing'],row['effects']),('PLAN','loader-http',[]))
    def test_explicit_http_uses_copied_bytes_with_zero_rdma(self):
        result,events,_=self.run_mode('explicit-http','http')
        self.assertEqual(result['witness']['read_transport'],'http')
        self.assertEqual([e['event'] for e in events],['ACK','EXACT','ACK','EXACT'])
    def test_http_with_rdma_counters_refuses(self):
        with self.assertRaisesRegex(AssertionError,'transport witness'): self.run_mode('http-with-dc','http')
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
    def run_mode(self,mode,transport='dc'):
        c=dict(run_id='cpu',client='m01',transport=transport,keys=2,length=64,root='/testops/cpu/m01')
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
        for transport in ('typo','RC','rdma'):
            with self.subTest(transport=transport),self.assertRaises(ValueError):kv_smoke.configure(dict(transport=transport,keys=1,length=1))
if __name__=='__main__':unittest.main()
