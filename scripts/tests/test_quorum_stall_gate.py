import json,subprocess,sys,tempfile,unittest
from pathlib import Path
SCRIPT=Path(__file__).resolve().parents[1]/'r4/quorum-stall.py'

class ApprovedQuorumGate(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
    def write(self,name,rows):
        (self.root/name).write_text(''.join(json.dumps(r)+'\n' for r in rows))
    def seed(self,rf='3',healthy_ms=21000):
        self.phases=[dict(phase=p,at_ns=t*1000000) for p,t in [('baseline',0),('fault-pending',900),('fault',1500),('recover',20000),('after',healthy_ms+2000),('stop',healthy_ms+5000)]]
        self.io=[]
        for n in range(1,(healthy_ms+5000)//1000):
            t=n*1000
            self.io += [dict(kind='begin',op=str(n),at_ns=(t-10)*1000000),dict(kind='end',op=str(n),at_ns=t*1000000,ok=rf=='3' or not 2000<=t<healthy_ms)]
        self.peers=[dict(at_ns=t*1000000,boot='test',primary=dict(VolumeID='v1',ReplicaID='r1',Epoch=1,AuthorityRole='primary'),peers=dict(VolumeID='v1',ReplicaID='r1',Peers=[dict(ReplicaID='r3' if rf=='3' else 'r2',Epoch=1,State='degraded' if 2000<=t<healthy_ms else 'healthy')])) for t in range(0,healthy_ms+5001,100)]
        self.flush()
    def flush(self):
        self.write('phases.jsonl',self.phases);self.write('io.jsonl',self.io);self.write('peers.jsonl',self.peers)
    def judge(self,rf='3'):
        return subprocess.run([sys.executable,str(SCRIPT),'judge','--work',str(self.root),'--rf',rf,'--write-budget-ms','5000','--resume-budget-ms','10000','--budget-ref','ruling-6b0a1756'],capture_output=True,text=True,timeout=5)
    def test_rf3_strict_five_second_ack_gap(self):
        self.seed();p=self.judge();self.assertEqual(p.returncode,0,p.stderr)
        self.io=[r for r in self.io if r['op'] not in ('5','6','7','8')];self.flush()
        p=self.judge();self.assertNotEqual(p.returncode,0);self.assertIn('ACK-gap',p.stderr)
    def test_rf3_zero_guest_errors(self):
        self.seed();self.io[5]['ok']=False;self.flush();p=self.judge()
        self.assertNotEqual(p.returncode,0);self.assertIn('guest I/O errors',p.stderr)
    def test_rf2_errors_are_reported_only_in_primary_observed_outage(self):
        self.seed('2');p=self.judge('2');self.assertEqual(p.returncode,0,p.stderr)
        r=json.loads(p.stdout);self.assertEqual(r['peer_down_ns'],2000000000);self.assertEqual(r['peer_healthy_ns'],21000000000);self.assertEqual(r['expected_outage_errors'],19)
        self.io[-1]['ok']=False;self.flush();p=self.judge('2');self.assertNotEqual(p.returncode,0);self.assertIn('outside primary',p.stderr)
    def test_rf2_ack_is_forbidden_by_completion_time_even_when_started_before_down(self):
        self.seed('2');self.io[2]['at_ns']=1500000000;self.io[3]['ok']=True;self.flush()
        p=self.judge('2');self.assertNotEqual(p.returncode,0);self.assertIn('RF2 ACKed',p.stderr)
    def test_test_chosen_fault_markers_cannot_hide_peer_window(self):
        self.seed('2');self.io[3]['ok']=True
        self.io=[r for r in self.io if r['op'] != '20']
        next(r for r in self.phases if r['phase']=='fault')['at_ns']=2500000000
        next(r for r in self.phases if r['phase']=='fault-pending')['at_ns']=2400000000
        self.flush();p=self.judge('2');self.assertNotEqual(p.returncode,0);self.assertIn('RF2 ACKed',p.stderr)
    def test_missing_or_foreign_primary_peer_view_refuses(self):
        self.seed('2');self.peers[100]['peers']['ReplicaID']='r9';self.flush()
        p=self.judge('2');self.assertNotEqual(p.returncode,0);self.assertIn('peer view',p.stderr)
    def test_rf3_peer_must_be_healthy_within_120_seconds_of_start_request(self):
        self.seed(healthy_ms=141000);p=self.judge();self.assertNotEqual(p.returncode,0);self.assertIn('120 s',p.stderr)
    def test_rf2_resume_within_ten_seconds_of_healthy(self):
        self.seed('2')
        for row in self.peers:
            if row['at_ns']>=3000000000:row['peers']['Peers'][0]['State']='healthy'
        next(r for r in self.phases if r['phase']=='recover')['at_ns']=2500000000
        self.io=[r for r in self.io if r['op'] not in tuple(str(n) for n in range(3,22))];self.flush()
        p=self.judge('2');self.assertNotEqual(p.returncode,0);self.assertIn('resume',p.stderr)

    def test_unfinished_write_and_missing_peer_samples_cannot_disappear(self):
        self.seed();self.io.append(dict(kind='begin',op='pending',at_ns=25500000000));self.flush()
        p=self.judge();self.assertNotEqual(p.returncode,0);self.assertIn('unfinished write',p.stderr)
        self.seed();self.peers=[r for r in self.peers if not 5000000000<r['at_ns']<6000000000];self.flush()
        p=self.judge();self.assertNotEqual(p.returncode,0);self.assertIn('observation gap',p.stderr)
    def test_fixed_approved_policy_cannot_be_loosened(self):
        self.seed()
        for value in ('0','-1','nan','inf','5001'):
            p=subprocess.run([sys.executable,str(SCRIPT),'judge','--work',str(self.root),'--rf','3','--write-budget-ms',value,'--resume-budget-ms','10000','--budget-ref','ruling-6b0a1756'],capture_output=True,text=True,timeout=5)
            self.assertNotEqual(p.returncode,0,p.stdout)
    def test_rf2_queued_attempt_can_finish_only_after_peer_healthy(self):
        self.seed('2')
        self.io=[r for r in self.io if r['op'] not in tuple(str(n) for n in range(2,22))]
        self.io[2]['at_ns']=1500000000
        self.flush();p=self.judge('2');self.assertEqual(p.returncode,0,p.stderr)

if __name__=='__main__':unittest.main()
