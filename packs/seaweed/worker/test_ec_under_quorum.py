"""Frozen real /proc/file capture, with literal first-encode interval controls."""
import hashlib, importlib.util, json, os, pathlib, subprocess, sys, tempfile, unittest
from unittest import mock

@unittest.skipUnless(sys.platform=='linux','requires owned /proc holder fixtures')
class UnderQuorum(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.root=pathlib.Path(self.tmp.name);os.chmod(self.root,0o700)
  spec=importlib.util.spec_from_file_location('uq_capture',pathlib.Path(__file__).with_name('ec_capture.py'));self.cap=importlib.util.module_from_spec(spec);spec.loader.exec_module(self.cap)
  self.logs=mock.patch.object(subprocess,'check_output',return_value='ec_volume.go:1593 correcting\n');self.log_read=self.logs.start();self.addCleanup(self.logs.stop)
  self.block=2*1024*1024;self.procs=[];self.holders={}
  self.manifest={'run_id':'fixture','objects':[{'key':'ec-'+str(i-1),'fid':'1,%xaabbccdd'%i,'length':64} for i in (1,2)]}
  for i in range(14):
   d=self.root/('h'+str(i));d.mkdir(mode=0o700);self.holders[d.name]=str(d)
   with (d/('bucket_1.ec%02d'%i)).open('wb') as f:f.truncate(self.block)
   if i==0:
    with (d/'bucket_1.ec00').open('r+b') as f:f.write(bytes([3,0,0,0,0,0,0,0]))
   self.procs.append(subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)','--dir='+str(d)]))
  self.files([16,self.block+16],self.block)
 def tearDown(self):
  for p in self.procs:p.terminate()
  for p in self.procs:p.wait(timeout=3)
  self.tmp.cleanup()
 def files(self,offsets,block):
  rows=b''
  for i,offset in enumerate(offsets,1):
   unit=offset//8
   rows+=i.to_bytes(8,'big')+(unit&0xffffffff).to_bytes(4,'big')+bytes([unit>>32])+(64).to_bytes(4,'big',signed=True)
  for d in self.root.iterdir():
   (d/'bucket_1.ecx').write_bytes(rows)
   (d/'bucket_1.vif').write_text(json.dumps({'version':3,'datFileSize':20*1024*1024,'ecShardConfig':{'dataShards':10,'parityShards':4,'encodeTsNs':'12345','blockSize':block}}))
 def capture(self):
  return self.cap.capture({'root':str(self.root),'holders':self.holders,'run_id':'fixture','volume_exe':sys.executable,'volume_sha256':hashlib.sha256(pathlib.Path(sys.executable).read_bytes()).hexdigest(),'under_quorum':True},self.manifest)
 def test_literal_intervals_and_boundary(self):
  row=self.capture();self.assertEqual(row['layout'],[3,self.block,20*1024*1024]);self.assertEqual(row['intervals'],{'ec-0':[16,96],'ec-1':[self.block+16,96]})
  self.files([self.block-16,2*self.block+16],self.block)
  self.assertEqual(self.capture()['intervals']['ec-0'],[self.block-16,96])
 def test_offset_high_byte_cannot_be_reinterpreted_as_low(self):
  self.files([16,(1<<35)+16],self.block)
  with self.assertRaises(ValueError):self.capture()
 def test_disagreement_retains_original_metadata(self):
  path=self.root/'h0/bucket_1.vif';data=json.loads(path.read_bytes());data['datFileSize']+=8;path.write_text(json.dumps(data))
  with self.assertRaises(ValueError) as caught:self.capture()
  evidence=json.loads(str(caught.exception).split(': ',1)[1]);vifs=[f for f in evidence['files'] if 'raw_vif' in f]
  self.assertEqual(len(vifs),14);self.assertEqual(len(evidence['layouts']),14)
  for f in vifs:self.assertEqual(hashlib.sha256(f['raw_vif'].encode()).hexdigest(),f['sha256'])
  self.assertEqual(sum(f['name'].endswith('.ecx') for f in evidence['files']),14)
 def test_canonical_superblock_and_reported_split(self):
  for i,d in enumerate(self.root.iterdir()):
   p=d/'bucket_1.vif';v=json.loads(p.read_bytes());v['version']=3+i%2;p.write_text(json.dumps(v))
  with (self.root/'h0/bucket_1.ec00').open('r+b') as f:f.write(bytes([4,0,0,0,0,0,0,0]))
  row=self.capture();self.assertEqual(row['layout'],[4,self.block,20*1024*1024]);self.assertEqual(row['vif_versions'],{'3':7,'4':7});self.assertEqual(row['superblock'],'0400000000000000');self.assertEqual(row['intervals']['ec-0'],[16,104]);self.assertEqual(row['version_corrections'],['ec_volume.go:1593 correcting']);self.log_read.assert_called_once()
 def test_short_superblock_refuses(self):
  (self.root/'h0/bucket_1.ec00').write_bytes(bytes([3,0,0]))
  with self.assertRaises(ValueError):self.capture()
 def test_correction_log_failure_refuses(self):
  self.log_read.side_effect=subprocess.CalledProcessError(23,['journalctl'])
  with self.assertRaises(subprocess.CalledProcessError):self.capture()
 def test_unknown_layout_refuses(self):
  self.files([16,self.block+16],0)
  with self.assertRaises(ValueError):self.capture()

if __name__=='__main__':unittest.main()
