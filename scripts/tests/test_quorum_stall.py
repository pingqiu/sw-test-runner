import json,subprocess,sys,tempfile,unittest,os
from pathlib import Path
SCRIPT=Path(__file__).resolve().parents[1]/'r4/quorum-stall.py'

class QuorumStallCLI(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
    def judge(self,rf='3',budget='500'):
        return subprocess.run([sys.executable,str(SCRIPT),'judge','--work',str(self.root),'--rf',rf,
            '--write-budget-ms',budget,'--resume-budget-ms','500','--budget-ref','test:literal-500ms'],capture_output=True,text=True,timeout=5)
    def seed(self):
        phases=[('baseline',0),('fault-pending',90),('fault',100),('recover',200),('after',300),('stop',500)]
        (self.root/'phases.jsonl').write_text(''.join(json.dumps(dict(phase=p,at_ns=t*1000000))+'\n' for p,t in phases))
        records=[]
        for i,(start,end) in enumerate([(10,20),(110,120),(310,320)]):
            records += [dict(kind='begin',op=str(i),at_ns=start*1000000),dict(kind='end',op=str(i),at_ns=end*1000000,ok=True)]
        (self.root/'io.jsonl').write_text(''.join(json.dumps(x)+'\n' for x in records))
    def test_rf3_budget_is_measured_and_a_slow_completed_write_fails(self):
        self.seed();p=self.judge();self.assertEqual(p.returncode,0,p.stderr)
        result=json.loads(p.stdout);self.assertEqual(result['max_write_ms'],10)
        p=self.judge(budget='5');self.assertNotEqual(p.returncode,0,p.stdout)
        self.assertEqual(p.stderr.strip(),'write budget exceeded')

    def test_rf2_refuses_single_copy_ack_but_reports_the_outage_separately(self):
        self.seed();p=self.judge(rf='2');self.assertNotEqual(p.returncode,0);self.assertIn('RF2 ACKed',p.stderr)
        phases=[('baseline',0),('fault-pending',90),('fault',100),('recover',1000),('after',1100),('stop',1300)]
        (self.root/'phases.jsonl').write_text(''.join(json.dumps(dict(phase=p,at_ns=t*1000000))+'\n' for p,t in phases))
        records=[json.loads(line) for line in (self.root/'io.jsonl').read_text().splitlines()]
        records[3]['ok']=False;records[4]['at_ns']=1110*1000000;records[5]['at_ns']=1120*1000000
        (self.root/'io.jsonl').write_text(''.join(json.dumps(r)+'\n' for r in records))
        p=self.judge(rf='2');self.assertEqual(p.returncode,0,p.stderr)
        result=json.loads(p.stdout);self.assertEqual(result['max_ack_gap_ms'],1100)
        self.assertEqual(result['bounded_ack_gap_ms'],180);self.assertEqual(result['resume_ms'],120)

    def test_unfinished_write_cannot_be_dropped_from_the_report(self):
        self.seed()
        with (self.root/'io.jsonl').open('a') as f:f.write(json.dumps(dict(kind='begin',op='pending',at_ns=450000000))+'\n')
        p=self.judge();self.assertNotEqual(p.returncode,0);self.assertIn('unfinished write',p.stderr)

    def test_fast_operations_do_not_hide_a_long_ack_gap(self):
        self.seed()
        for filename in ('phases.jsonl','io.jsonl'):
            rows=[json.loads(line) for line in (self.root/filename).read_text().splitlines()]
            for row in rows:
                if row['at_ns']>=200000000:row['at_ns']+=1000000000
            (self.root/filename).write_text(''.join(json.dumps(r)+'\n' for r in rows))
        p=self.judge();self.assertNotEqual(p.returncode,0);self.assertIn('ACK-gap',p.stderr)

    def test_budget_must_be_explicit_finite_and_sourced(self):
        self.seed()
        for value in ('0','-1','nan','inf'):
            p=self.judge(budget=value);self.assertNotEqual(p.returncode,0,p.stdout)

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



    def test_recovery_without_bounded_progress_fails(self):
        self.seed()
        rows=[json.loads(line) for line in (self.root/'phases.jsonl').read_text().splitlines()]
        for row in rows:
            if row['phase']=='recover':row['at_ns']=121000000
        (self.root/'phases.jsonl').write_text(''.join(json.dumps(r)+'\n' for r in rows))
        p=subprocess.run([sys.executable,str(SCRIPT),'judge','--work',str(self.root),'--rf','3','--write-budget-ms','500','--resume-budget-ms','5','--budget-ref','test-only'],capture_output=True,text=True,timeout=5)
        self.assertNotEqual(p.returncode,0);self.assertIn('resume budget',p.stderr)

    def test_startup_wait_does_not_mistake_unpublished_writer_metadata_for_exit(self):
        p=subprocess.run([sys.executable,str(SCRIPT),'wait-acks','--work',str(self.root),'--minimum','1','--seconds','0.01'],capture_output=True,text=True,timeout=5)
        self.assertNotEqual(p.returncode,0);self.assertEqual(p.stderr.strip(),'no bounded ACK progress')

if __name__=='__main__':unittest.main()
