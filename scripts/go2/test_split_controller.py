import importlib.util
import pathlib
import unittest
import tempfile
import os
import subprocess
import shutil
import hashlib

spec = importlib.util.spec_from_file_location("split_controller", pathlib.Path(__file__).with_name("split_controller.py"))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)

PREFIX = "#!/bin/bash\nPK=/packet\nRUN=example\n"
PRE = "# Pre-claim (the guarded section)\nclaim_checked\n# ---- 0. local checks\nverify_all\n"
WORK = "# ---- 1. contact both hosts\ncontact_one\n: > \"$LOUT/phases.txt\"\n# T1-E1 outage observation (dead)\nobswait() { :; }\nphase() {\n  run_phase\n}\n# ---- 5. admission\nrun_work\n"
FINAL = "# ---- 9. closeout: proof-gated\nclose_m02\nclose_m01\nend_or_quarantine\n"

class SplitController(unittest.TestCase):
 def test_extracts_exact_guards_and_finalization(self):
  launch, pre, final = m.split_launch(PREFIX+PRE+WORK+FINAL)
  self.assertEqual(pre, PRE)
  self.assertEqual(final, FINAL)
  self.assertIn('. "$PK/preflight.sh"', launch)
  self.assertIn('. "$PK/finalize.sh"', launch)
  self.assertNotIn("obswait", launch)
  self.assertIn("run_phase", launch)
  self.assertEqual(launch.count("contact_one"), 1)
 def test_refuses_changed_or_ambiguous_source(self):
  original=PREFIX+PRE+WORK+FINAL
  for text in [original.replace("# Pre-claim (", "# Changed ("), original+FINAL, original.replace("phase() {", "changed() {")]:
   with self.subTest(text=text):
    with self.assertRaises(ValueError): m.split_launch(text)
 def test_refuses_output_inside_source(self):
  self.assertFalse(m.separate_paths(pathlib.Path('/packet'), pathlib.Path('/packet/out')))
  self.assertFalse(m.separate_paths(pathlib.Path('/packet'), pathlib.Path('/packet')))
  self.assertTrue(m.separate_paths(pathlib.Path('/packet'), pathlib.Path('/work/output')))

 def test_refuses_both_phase_anchor_forms(self):
  text=PREFIX+PRE+WORK.replace('phase() {\n', 'phase() {  # 0 rc 0\n')+'\nphase() {\n}\n'+FINAL
  with self.assertRaises(ValueError):m.split_launch(text)

 def test_refuses_unknown_command_appended_to_legacy_line(self):
  line='  case "$n" in first|hop-*) t1phase "$n" ;; esac'
  text=PREFIX+PRE+WORK.replace('  run_phase',line+'; unexpected_command\n  run_phase')+FINAL
  with self.assertRaises(ValueError):m.split_launch(text)

 def test_compatibility_launcher_transcript_and_refusal(self):
  bash=os.environ.get('BASH_FOR_TESTS') or shutil.which('bash')
  if not bash: self.skipTest('bash unavailable; transcript not qualified')
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d)
   defs="set -e\nPK='"+root.as_posix()+"'\nLOUT='"+root.as_posix()+"'\n"
   for name in ['claim_checked','verify_all','contact_one','run_phase','run_work','close_m02','close_m01','end_or_quarantine']:
    defs+=name+'() { echo '+name+'; };\n'
   text=defs+PRE+WORK+FINAL
   launch,pre,final=m.split_launch(text)
   for name,body in [('original.sh',text),('launch.sh',launch),('preflight.sh',pre),('finalize.sh',final)]:
    (root/name).write_text(body)
   for refuse in [False,True]:
    if refuse:
     for name in ['original.sh','launch.sh']:
      p=root/name;p.write_text(p.read_text().replace('claim_checked() { echo claim_checked;', 'claim_checked() { echo claim_checked; exit 1;'))
    results=[subprocess.run([bash,str(root/name)],text=True,capture_output=True,timeout=5) for name in ['original.sh','launch.sh']]
    self.assertEqual((results[0].returncode,results[0].stdout),(results[1].returncode,results[1].stdout))
    if refuse:self.assertNotIn('contact_one',results[1].stdout)
    else:
     self.assertEqual(results[1].returncode,0,results[1].stderr)
     self.assertLess(results[1].stdout.index('close_m02'),results[1].stdout.index('close_m01'))

 def test_materialize_keeps_reference_and_host_bytes(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d); source=root/'source';source.mkdir()
   original=(PREFIX+PRE+WORK+FINAL).encode()
   (source/'launch.sh').write_bytes(original)
   (source/'host.sh').write_bytes(b'host safety bytes')
   pin=hashlib.sha256(original).hexdigest()
   out=root/'out'
   with self.assertRaises(ValueError):m.materialize(source,out,'0'*64)
   self.assertFalse(out.exists())
   result=m.materialize(source,out,pin)
   self.assertEqual((source/'launch.sh').read_bytes(),original)
   self.assertEqual((out/'host.sh').read_bytes(),b'host safety bytes')
   self.assertEqual(result['status'],'UNSEALED_REVIEW_ONLY')
   with self.assertRaises(ValueError):m.materialize(source,out,pin)

if __name__=='__main__': unittest.main()
