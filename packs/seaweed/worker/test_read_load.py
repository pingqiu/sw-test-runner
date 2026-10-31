"""CPU controls of the pack CLI; accepted worker/oracle leaves stay unchanged."""
import copy, hashlib, json, pathlib, sys, unittest, contextlib, io, types, os
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parent))
import kv_smoke
from test_kv_smoke import FakeConnector

class ReadControls(unittest.TestCase):
    def run_case(self, mode='good', route='dc', stage='recovery'):
        c=dict(run_id='cpu',client='m01',transport='dc',keys=2,length=64,root='/testops/cpu/m01',rdma_address='10.0.2.3:20980')
        conn=FakeConnector(mode)
        for i in range(c['keys']):
            key=c['client']+'-'+str(i)
            conn.values[key]=kv_smoke.exact_helper('soak_native_worker').payload(c['run_id']+'/'+key,c['length'])
        if route=='http':conn.stats['loader']['transport']='http'
        try:return kv_smoke.read_only(conn,c,stage,route),conn
        finally:conn.close()
    def test_recovery_exact_has_no_sets_and_dc_witness(self):
        row,conn=self.run_case()
        self.assertEqual((row['status'],row['exact'],conn.sets),('OBSERVED',2,0))
        self.assertEqual(row['transport'],'dc')
        self.assertEqual(row['faults'],[])
    def test_http_control_is_copied_not_dc(self):
        row,conn=self.run_case('http','http')
        self.assertEqual((row['exact'],conn.sets),(2,0))
    def test_first_exact_is_completion_not_submission(self):
        with patch.object(kv_smoke.time,'time',side_effect=[100.,101.,102.,103.,104.,105.]):
            row,_=self.run_case()
        self.assertEqual((row['first_exact'],row['last_exact']),(102.,104.))
    def test_wrong_bytes_miss_and_wrong_route_refuse_recovery(self):
        for mode in ('wrong','miss','http'):
            with self.subTest(mode=mode),self.assertRaises(AssertionError):self.run_case(mode)
    def test_load_records_completed_failure_without_expected_credit(self):
        # Tiny fake clock advances one second per sleep; real entry fixes 120 s.
        original_now,original_sleep=kv_smoke.time.monotonic,kv_smoke.time.sleep
        now=[0.0]
        kv_smoke.time.monotonic=lambda:now[0]
        kv_smoke.time.sleep=lambda delay:now.__setitem__(0,now[0]+max(delay,1))
        try:
            row,conn=self.run_case('miss',stage='load')
            self.assertEqual(row['status'],'OBSERVED')
            self.assertEqual(conn.sets,0)
            self.assertGreater(len(row['faults']),0)
            self.assertNotIn('expected_errors',row)
            self.assertTrue(all(f['action']=='get' and f['target']=='10.0.2.3:20980' for f in row['faults']))
        finally:kv_smoke.time.monotonic,kv_smoke.time.sleep=original_now,original_sleep
    def test_seed_stage_is_the_existing_smoke_path(self):
        self.assertEqual(kv_smoke.read_stage_args('seed','dc'),('seed','dc'))
        for stage,route in [('seed','http'),('load','rc'),('arbitrary','dc')]:
            with self.assertRaises(ValueError):kv_smoke.read_stage_args(stage,route)

    def test_real_main_dispatch_has_no_sets(self):
        for stage,route in [('load','dc'),('recovery','dc'),('recovery','http')]:
            with self.subTest(stage=stage,route=route):
                c=dict(run_id='cpu',client='m01',transport='dc',keys=2,length=64,root='/testops/cpu/m01',
                    rdma_address='10.0.2.3:20980',rdma_control='10.0.2.3:20981',dc_control='10.0.2.3:20983',
                    device='cpu-fake',device_port=1,gid_index=3,helper_dir=str(pathlib.Path(__file__).parent),native_dir='/cpu/fake',filer_grpc='cpu:1')
                conn=FakeConnector('http' if route=='http' else 'good')
                for i in range(2):
                    key='m01-'+str(i);conn.values[key]=kv_smoke.exact_helper('soak_native_worker').payload('cpu/'+key,64)
                if route=='http':conn.stats['loader']['transport']='http'
                module=types.ModuleType('seaweedkv_native._native');backings=[]
                def construct(address,backing,*unused):backings.append(backing);return conn
                module.SeaweedKVConnector=construct
                now=[0.0];output=io.StringIO()
                with patch.dict(sys.modules,{'seaweedkv_native._native':module}),patch.dict(os.environ),patch.object(sys,'argv',['kv_smoke.py','--config',json.dumps(c),'--stage',stage,'--read-path',route]),patch.object(kv_smoke.time,'monotonic',lambda:now[0]),patch.object(kv_smoke.time,'sleep',lambda delay:now.__setitem__(0,now[0]+1)),contextlib.redirect_stdout(output):
                    self.assertEqual(kv_smoke.main(),0,output.getvalue())
                self.assertEqual(conn.sets,0)
                self.assertEqual(backings,['loader-http' if route=='http' else 'loader-rdma'])
                self.assertEqual(json.loads(output.getvalue())['status'],'OBSERVED')

if __name__=='__main__':unittest.main()
