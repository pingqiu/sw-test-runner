"""Owned Linux process/file fixtures for frozen capture; no EC product claim."""
import hashlib, importlib.util, json, os, pathlib, subprocess, sys, tempfile, unittest

@unittest.skipUnless(sys.platform=='linux','requires real /proc holder identities')
class Capture(unittest.TestCase):
 def test_actual_owned_holders_and_original_refusal(self):
  spec=importlib.util.spec_from_file_location('cap',pathlib.Path(__file__).with_name('ec_capture.py'));cap=importlib.util.module_from_spec(spec);spec.loader.exec_module(cap)
  with tempfile.TemporaryDirectory() as tmp:
   root=pathlib.Path(tmp);os.chmod(root,0o700);holders={};procs=[]
   try:
    for i in range(14):
     d=root/('h%d'%i);d.mkdir(mode=0o700);holders[d.name]=str(d)
     (d/('bucket_1.ec%02d'%i)).write_bytes(b'reference shard bytes')
     (d/'bucket_1.ecx').write_bytes(b'index')
     (d/'bucket_1.vif').write_text(json.dumps({'ecShardConfig':{'dataShards':10,'parityShards':4,'encodeTsNs':'12345'}}))
     procs.append(subprocess.Popen([sys.executable,'-c','import time;time.sleep(20)','--dir='+str(d)]))
    (root/'h0/bucket_1.ecx').write_bytes(b'index')
    (root/'h0/bucket_1.vif').write_text(json.dumps({'ecShardConfig':{'dataShards':10,'parityShards':4,'encodeTsNs':'12345'}}))
    c={'root':tmp,'holders':holders,'run_id':'cpu','volume_exe':sys.executable,'volume_sha256':hashlib.sha256(pathlib.Path(sys.executable).read_bytes()).hexdigest()}
    m={'run_id':'cpu','objects':[{'fid':'1,abc'}]}
    row=cap.capture(c,m);self.assertTrue(row['original_absent']);self.assertEqual(len(row['holders']),14);self.assertEqual(len(row['shards']),14)
    (root/'h0/bucket_1.dat').write_bytes(b'intact original')
    with self.assertRaisesRegex(ValueError,'original'):cap.capture(c,m)
   finally:
    for p in procs:p.terminate()
    for p in procs:p.wait(timeout=3)
if __name__=='__main__':unittest.main()
