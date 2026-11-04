"""CPU controls of the pack CLI; accepted worker/oracle leaves stay unchanged."""
import copy, hashlib, json, pathlib, sys, time, unittest, contextlib, io, types, os
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parent))
import kv_smoke
from test_kv_smoke import FakeConnector

class ReadControls(unittest.TestCase):
    def run_case(self, mode='good', route='dc', stage='recovery', transport='dc'):
        c=dict(run_id='cpu',client='m01',transport=transport,keys=2,length=64,root='/testops/cpu/m01')
        if transport=='dc':c['rdma_address']='10.0.2.3:20980'
        conn=FakeConnector(mode)
        for i in range(c['keys']):
            key=c['client']+'-'+str(i)
            conn.values[key]=kv_smoke.exact_helper('soak_native_worker').payload(c['run_id']+'/'+key,c['length'])
        if route=='http':conn.stats['loader']['transport']='http'
        conn.http=transport=='http'
        try:return kv_smoke.read_only(conn,c,stage,route),conn
        finally:conn.close()
    def test_recovery_exact_has_no_sets_and_dc_witness(self):
        row,conn=self.run_case()
        self.assertEqual((row['status'],row['exact'],conn.sets),('OBSERVED',2,0))
        self.assertEqual(row['transport'],'dc')
        self.assertEqual(row['faults'],[])
        self.assertEqual([o['operation_id'] for o in row['outcomes']],[0,1])
        self.assertTrue(all(o['exact'] and o['ended']>=o['started'] for o in row['outcomes']))
    def test_http_control_is_copied_not_dc(self):
        row,conn=self.run_case('http','http')
        self.assertEqual((row['exact'],conn.sets),(2,0))
    def test_first_exact_is_completion_not_submission(self):
        import itertools
        clock=itertools.count(100.)
        with patch.object(kv_smoke.time,'monotonic',side_effect=lambda:next(clock)):
            row,_=self.run_case()
        self.assertEqual(row['first_exact'],row['outcomes'][0]['ended'])
        self.assertGreater(row['first_exact'],row['outcomes'][0]['started'])
        self.assertEqual(row['last_exact'],row['outcomes'][-1]['ended'])
        self.assertEqual(row['elapsed'],row['ended']-row['started'])
        self.assertTrue(all(row['started']<=o['started']<=o['ended']<=row['ended'] for o in row['outcomes']))
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
    def test_http_inventory_load_records_failure_without_rdma_target(self):
        now=[0.0]
        with patch.object(kv_smoke.time,'monotonic',lambda:now[0]),patch.object(kv_smoke.time,'sleep',lambda delay:now.__setitem__(0,now[0]+1)):
            row,_=self.run_case('miss','http','load','http')
        self.assertTrue(row['faults'] and all(f['target']=='' and f['transport']=='http' for f in row['faults']))
    def test_wrong_bytes_never_become_an_outage_observation(self):
        with self.assertRaises(AssertionError):self.run_case('wrong',stage='load')
    def test_seed_stage_is_the_existing_smoke_path(self):
        self.assertEqual(kv_smoke.read_stage_args('seed','dc'),('seed','dc'))
        for stage,route in [('seed','http'),('load','rc'),('arbitrary','dc')]:
            with self.assertRaises(ValueError):kv_smoke.read_stage_args(stage,route)

    def test_http_inventory_reads_only_over_http(self):
        for stage in ('seed','load','recovery'):
            self.assertEqual(kv_smoke.read_stage_args(stage,'http','http'),(stage,'http'))
            with self.assertRaises(ValueError):kv_smoke.read_stage_args(stage,'dc','http')
        with self.assertRaises(ValueError):kv_smoke.read_stage_args('load','http','rc')

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

class HeldSets(FakeConnector):
    """SET completions wait until `release` GETs have run: the pool-holding stall."""
    def __init__(self,release):
        super().__init__();self.release=release;self.held=[];self.peak=0;self.gets=0
    def submit_batch_set(self,keys,buffers):
        self.sets+=1;self.values[keys[0]]=bytes(buffers[0])
        s=self.stats['loader'];s['write_bytes']+=len(buffers[0]);s['write_objects']+=1
        self.future+=1;self.held.append(self.future);self.peak=max(self.peak,len(self.held))
        if self.gets>=self.release:
            for f in self.held:
                self.pending.append((f,True,"",[True]));os.write(self.w,b"x")
            self.held=[];return self.future
        if self.sets==1:
            self.pending.append((self.future,True,'',[True]));self.held.pop();os.write(self.w,b'x')  # one ACK, so GETs have a key
        return self.future
    def submit_batch_get(self,keys,buffers):
        self.gets+=1
        if self.gets>=self.release:
            for f in self.held:
                self.pending.append((f,True,'',[True]));os.write(self.w,b'x')
            self.held=[]
        return super().submit_batch_get(keys,buffers)

class WriteLoadControls(unittest.TestCase):
    """Report-only load across a master restart: fresh keys, no retries, no threshold."""
    C=dict(run_id='cpu',client='m01',transport='dc',keys=2,length=64,root='/testops/cpu/m01')
    def load(self,mode='good',duration=.05,conn=None):
        conn=conn or FakeConnector(mode);rows=[]
        try:return kv_smoke.write_load(conn,self.C,rows.append,duration=duration,pace=.005),rows,conn
        finally:conn.close()
    def test_every_set_is_one_fresh_key_and_read_back(self):
        end,rows,conn=self.load()
        sets=[r for r in rows if r['event']=='SET']
        self.assertEqual(end['sets'],len(sets));self.assertGreater(len(sets),0)
        self.assertEqual(len({r['key'] for r in sets}),len(sets))
        self.assertFalse({r['key'] for r in sets}&{'m01-0','m01-1'})
        self.assertEqual(sorted(r['operation_id'] for r in sets),list(range(len(sets))))
        self.assertEqual((end['status'],end['stage'],end['transport']),('OBSERVED','write-load','dc'))
        self.assertEqual((end['acked'],end['failed'],end['readback_exact'],end['readback_missing']),(len(sets),0,len(sets),0))
        self.assertEqual(conn.sets,len(sets))
    def test_gets_run_on_the_same_connector_during_the_load(self):
        end,rows,_=self.load()
        gets=[r for r in rows if r['event']=='GET']
        self.assertEqual(end['gets'],len(gets));self.assertGreater(len(gets),0)
        self.assertTrue(all(r['ok'] for r in gets))
    def test_held_sets_stop_at_four_and_gets_keep_going(self):
        conn=HeldSets(5)
        end,rows,_=self.load(duration=.2,conn=conn)
        self.assertEqual(conn.peak,4)
        order=[r['event'] for r in rows]
        self.assertGreaterEqual(order[:order.index('SET',1)].count('GET'),4)
        self.assertEqual(end['failed'],0)
    def test_failed_set_is_recorded_not_fatal_and_not_read_back(self):
        end,rows,_=self.load('ambiguous')
        sets=[r for r in rows if r['event']=='SET']
        self.assertTrue(sets and all(not r['ok'] and r['error'] for r in sets))
        self.assertEqual((end['acked'],end['failed'],end['readback_exact'],end['gets']),(0,len(sets),0,0))
    def test_wrong_bytes_stay_fatal(self):
        with self.assertRaisesRegex(AssertionError,'wrong'):self.load('wrong')
    def test_a_miss_is_counted_not_excused(self):
        end,rows,_=self.load('miss')
        self.assertEqual(end['readback_exact'],0);self.assertEqual(end['readback_missing'],end['acked'])
        self.assertTrue(any(r['event']=='GET' and not r['ok'] for r in rows))
    def test_fresh_gets_read_each_key_once_and_rereads_stay_on_one_key(self):
        end,rows,_=self.load(duration=.2)
        gets=[r for r in rows if r['event']=='GET']
        fresh=[r['key'] for r in gets if r.get('fresh')];again={r['key'] for r in gets if not r.get('fresh')}
        self.assertGreater(len(fresh),0);self.assertEqual(len(set(fresh)),len(fresh))
        self.assertEqual(len(again),1);self.assertFalse(again&set(fresh))
    def test_building_a_get_buffer_is_fast_and_exact(self):
        expected=bytearray(b'a'*1048576)
        begun=time.perf_counter();built=kv_smoke.inverted(expected);took=time.perf_counter()-begun
        self.assertEqual(built,bytearray(v^255 for v in expected));self.assertLess(took,.01)
    def test_completions_are_read_before_a_get_is_submitted(self):
        waiting=[]
        class Order(FakeConnector):
            def submit_batch_get(self,keys,buffers):
                waiting.append(len(self.pending))
                return super().submit_batch_get(keys,buffers)
        self.load(conn=Order())
        self.assertGreater(len(waiting),0);self.assertEqual(set(waiting),{0})
    def retained(self):
        return sys.modules['_seaweed_pinned_soak_native_worker'].UNSETTLED_BUFFERS
    def test_wrong_get_retains_every_outstanding_buffer(self):
        class Late(HeldSets):
            def submit_batch_get(self,keys,buffers):
                self.mode='wrong' if len(self.held)>=2 else 'good'
                return super().submit_batch_get(keys,buffers)
        conn=Late(10**6)
        with self.assertRaisesRegex(AssertionError,'wrong'):self.load(duration=.2,conn=conn)
        self.assertGreater(len(conn.held),0)
        self.assertEqual(len(self.retained()),len(conn.held)+1)  # the held SETs and the failing GET's own buffer
    def test_emitter_failure_retains_outstanding_buffers(self):
        conn=HeldSets(10**6);rows=[]
        def emit(row):
            if row['event']=='GET' and len(conn.held)>=2:raise OSError('stdout gone')
        try:
            with self.assertRaisesRegex(OSError,'stdout'):kv_smoke.write_load(conn,self.C,emit,duration=.2,pace=.005)
            self.assertEqual(len(self.retained()),len(conn.held));self.assertGreater(len(conn.held),0)
        finally:conn.close()
    def test_future_collision_refuses_and_retains_both(self):
        class Same(FakeConnector):
            def submit_batch_set(self,keys,buffers):
                self.sets+=1;self.values[keys[0]]=bytes(buffers[0]);return 7
        conn=Same()
        with self.assertRaisesRegex(RuntimeError,'collision'):self.load(duration=.2,conn=conn)
        self.assertEqual(len(self.retained()),2)
    def test_submit_failure_retains_its_buffer(self):
        class Boom(FakeConnector):
            def submit_batch_set(self,keys,buffers):raise OSError('submit lost')
        with self.assertRaisesRegex(OSError,'submit lost'):self.load(conn=Boom())
        self.assertEqual(len(self.retained()),1)
    def test_full_duration_stdout_fits_the_capture_cap(self):
        """The real 120 s on a virtual clock, serialized as main serializes it."""
        for mode in ('good','miss','ambiguous'):
            class Clock:
                t=1000000.0
                @classmethod
                def monotonic(k):
                    k.t+=.0005
                    return k.t
                @staticmethod
                def sleep(_):pass
            conn=FakeConnector(mode);rows=[]
            try:
                with patch.object(kv_smoke,'time',Clock):
                    end=kv_smoke.write_load(conn,self.C,rows.append)
            finally:conn.close()
            size=sum(len(kv_smoke.compact(r))+1 for r in rows+[dict(end,code='OK',remedy='inspect ACK/exact/transport records')])
            self.assertGreaterEqual(end['sets'],400)
            self.assertLess(size,131072,mode)
            self.assertEqual(sum(r['event']=='SET' for r in rows),end['sets'])
            self.assertEqual(sum(r['event']=='GET' for r in rows),end['gets'])
    def test_evidence_over_the_budget_is_fatal_not_truncated(self):
        class Loud(FakeConnector):
            def complete(self,ok,results,error):return super().complete(False,None,'x'*2000)
        with self.assertRaisesRegex(AssertionError,'write-load evidence'):self.load('good',duration=20,conn=Loud())
    def test_http_write_load_uses_same_schedule_and_exact_readback(self):
        conn=FakeConnector('explicit-http');rows=[]
        try:
            end=kv_smoke.write_load(conn,dict(self.C,transport='http'),rows.append,duration=.05,pace=.005)
        finally:conn.close()
        self.assertEqual(end['transport'],'http')
        self.assertGreater(end['acked'],0)
        self.assertEqual(end['readback_exact'],end['acked'])
        self.assertEqual(end['readback_missing'],0)
        conn=FakeConnector('good')
        try:
            with self.assertRaisesRegex(AssertionError,'transport'):
                kv_smoke.write_load(conn,dict(self.C,transport='http'),lambda _:None,duration=.02,pace=.005)
        finally:conn.close()
    def test_stage_transport_is_explicit(self):
        self.assertEqual(kv_smoke.read_stage_args('write-load','dc'),('write-load','dc'))
        self.assertEqual(kv_smoke.read_stage_args('write-load','http','http'),('write-load','http'))
        for route,transport in (('http','dc'),('dc','http'),('http','rc')):
            with self.assertRaises(ValueError):kv_smoke.read_stage_args('write-load',route,transport)

if __name__=='__main__':unittest.main()
