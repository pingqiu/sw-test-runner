import importlib.util
import json
import pathlib
import tempfile
import unittest

spec=importlib.util.spec_from_file_location('judge_bindings',pathlib.Path(__file__).with_name('judge_bindings.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class JudgeBindings(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
  self.root=pathlib.Path(self.tmp.name)
  self.config={'root':str(self.root),'victim':'m01','boot_id':'boot-a','kind':'KB','rehearsal':True,'run_id':'go2-pilot-r01','release_credit':False,'blocks':1000,'request':'move-a'}
  self.range={'row':'d6-lost-range','verdict':'PASS','counted':True,'values':{'L':'42'}}
  self.loss={'host':'m01','boot_id':'boot-a','clock_domain':'host','unit':'run-src.service','invocation':'inv-a','kill_issued_ns':1000000000,'confirmed_dead_ns':1100000000}
 def build(self):
  (self.root/'lostrange.receipt.json').write_text(json.dumps(self.range))
  (self.root/'loss.receipt.json').write_text(json.dumps(self.loss))
  return m.initial_actions(self.config)+m.followup_actions(self.config)
 def test_binds_accepted_actions_and_host_issue_time(self):
  actions=self.build()
  self.assertEqual([a['action'] for a in actions],['hostloss_row1','hostloss_lostrange','hostloss_final','hostloss_image'])
  final=actions[2];image=actions[3]
  self.assertEqual(final['kill_ns'],'1000000000')
  self.assertNotIn('fence_sec',final)
  self.assertEqual(image['L'],'42')
  self.assertEqual(final['row1_receipt'],str(self.root/'row1.receipt.json'))
  self.assertTrue(all(a['allow_not_counted']=='true' for a in actions))
 def test_partition_binds_only_fence_second_and_stopbound(self):
  self.config['kind']='PO'
  self.loss.pop('kill_issued_ns');self.loss.pop('confirmed_dead_ns');self.loss['fence_sec']=123
  actions=self.build();final=actions[2]
  self.assertEqual(final['fence_sec'],'123');self.assertNotIn('kill_ns',final)
  self.assertEqual(actions[-1]['action'],'hostloss_stopbound')
 def test_counted_template_cannot_allow_not_counted(self):
  self.config['rehearsal']=False
  self.assertTrue(all('allow_not_counted' not in a for a in self.build()))
 def test_rejects_wrong_clock_identity_legacy_or_dead_stamp(self):
  for key,value in [('clock_domain','controller'),('host','m02'),('boot_id','other'),('kill_issued_ns',0),('confirmed_dead_ns',1)]:
   old=self.loss[key];self.loss[key]=value
   with self.subTest(key=key),self.assertRaises(ValueError):self.build()
   self.loss[key]=old
  self.loss={'unit':'run-src.service','invocation':'inv-a','t_ns':1100000000}
  with self.assertRaises(ValueError):self.build()
 def test_requires_exactly_one_loss_clock_and_confirmed_range(self):
  self.loss['fence_sec']=123
  with self.assertRaises(ValueError):self.build()
  del self.loss['fence_sec'];self.range['verdict']='NOT_COUNTED';self.range['counted']=False
  with self.assertRaises(ValueError):self.build()

 def test_exception_refuses_counted_id_or_release_credit(self):
  for key,value in [('run_id','go2-pilot-d01'),('release_credit',True)]:
   old=self.config[key];self.config[key]=value
   with self.assertRaises(ValueError):self.build()
   self.config[key]=old
 def test_snapshot_uses_its_own_cut_lsn(self):
  (self.root/'s1.snapshot.json').write_text(json.dumps({'cutLSN':17}))
  a=m.snapshot_image_action(self.config,'s1')
  self.assertEqual(a['L'],'17')
  self.assertEqual(a['image'],str(self.root/'s1.image.bin'))
  self.assertEqual(a['row'],'restore-s1')

if __name__=='__main__':unittest.main()
