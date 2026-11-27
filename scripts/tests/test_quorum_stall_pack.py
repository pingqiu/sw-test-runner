import hashlib,json,os,subprocess,sys,tempfile,unittest
from pathlib import Path
ROOT=Path(__file__).resolve().parents[1]/'r4'

class QuorumPackCLI(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=Path(self.tmp.name)
        self.policy=dict(write_budget_ms=5000,resume_budget_ms=10000,budget_ref='test-only-bound')
        (self.root/'quorum-stall.py').write_bytes((ROOT/'quorum-stall.py').read_bytes())
        (self.root/'pins.json').write_text('{}');(self.root/'swblock').write_bytes(b'control')
        (self.root/'cloud-pack.py').write_text('import json\nfrom pathlib import Path\ndef main(root,**kw):\n (Path(root)/"delegated.json").write_text(json.dumps(kw));return 0\n')
    def seal(self):
        (self.root/'quorum-stall-policy.json').write_text(json.dumps(self.policy))
        self.manifest={p.name:hashlib.sha256(p.read_bytes()).hexdigest() for p in self.root.iterdir() if p.is_file() and p.name!='manifest.json'}
        (self.root/'manifest.json').write_text(json.dumps(self.manifest))
    def run_entry(self,*options):
        env=dict(os.environ);env.pop('R4_STALL_DD',None)
        return subprocess.run([sys.executable,*options,str(ROOT/'quorum-stall-pack.py'),str(self.root)],env=env,capture_output=True,text=True,timeout=10)
    def test_verified_policy_delegates_to_the_existing_controller(self):
        self.seal();p=self.run_entry();self.assertEqual(p.returncode,0,p.stderr)
        self.assertEqual(json.loads((self.root/'delegated.json').read_text()),dict(scenario='scenarios/v3block/quorum-stall.yaml',extra_env=self.policy))
    def test_missing_bound_refuses_before_import_or_network(self):
        self.policy['write_budget_ms']=0;self.seal();p=self.run_entry()
        self.assertNotEqual(p.returncode,0);self.assertFalse((self.root/'delegated.json').exists())
    def test_every_executed_input_must_be_pinned(self):
        for name in ('pins.json','swblock','cloud-pack.py','quorum-stall.py','quorum-stall-policy.json'):
            with self.subTest(name=name):
                self.seal();self.manifest.pop(name);(self.root/'manifest.json').write_text(json.dumps(self.manifest))
                p=self.run_entry();self.assertNotEqual(p.returncode,0);self.assertIn(name,p.stderr)
                self.assertFalse((self.root/'delegated.json').exists())
    def test_optimized_python_refuses_before_inputs(self):
        p=self.run_entry('-O');self.assertNotEqual(p.returncode,0);self.assertIn('refuses optimized',p.stderr)

if __name__=='__main__':unittest.main()
