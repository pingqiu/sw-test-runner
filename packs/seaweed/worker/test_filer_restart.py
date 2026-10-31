"""Local-only CLI checks; ELF exec replaces product/hardware, never a lab run."""
import datetime,hashlib,json,os,pathlib,shutil,socket,subprocess,sys,tempfile,unittest
from unittest.mock import patch
import lab_profile as profile
from test_lab_profile import LaunchContract

@unittest.skipUnless(sys.platform=='linux','Linux private socket/process seam')
class FilerReplacement(unittest.TestCase):
 def test_actual_cli_preserves_metadata_and_refuses_foreign_or_live_state(self):
  for mode in ('good','no-socket','live-old','foreign-boot','bad-config','config-symlink','empty-metadata','metadata-symlink','regular-socket-path','socket-symlink','active-socket','foreign-socket'):
   if mode=='foreign-socket' and os.geteuid()!=0:continue
   with self.subTest(mode=mode),tempfile.TemporaryDirectory() as tmp:
    base=pathlib.Path(tmp);root=base/'run';release=base/'release'
    root.mkdir(mode=0o700);(root/'filer').mkdir(mode=0o700);meta=root/'filer'/'ldb2';meta.mkdir(mode=0o700)
    sentinel=meta/'CURRENT';sentinel.write_bytes(b'RETAINED-METADATA')
    original=profile.filer_config(root).encode();(root/'filer.toml').write_bytes(original)
    (release/'bin').mkdir(parents=True);binary=release/'bin'/'weed';shutil.copyfile('/bin/true',binary);binary.chmod(0o755)
    boot=pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()
    old=profile.process(os.getpid()) if mode=='live-old' else dict(pid=99999999,starttime=1,boot_id=boot)
    if mode=='foreign-boot':old['boot_id']='foreign'
    old_bytes=json.dumps(old).encode();(root/'filer-generation.json').write_bytes(old_bytes)
    sock=root/'filer.sock';listener=socket.socket(socket.AF_UNIX)
    if mode!='no-socket':listener.bind(str(sock))
    if mode!='active-socket':listener.close()
    if mode=='foreign-socket':os.chown(sock,os.getuid()+1,os.getgid())
    if mode=='bad-config':(root/'filer.toml').write_bytes(b'OTHER')
    if mode=='config-symlink':(root/'filer.toml').unlink();(root/'filer.toml').symlink_to(sentinel)
    if mode=='empty-metadata':sentinel.unlink()
    if mode=='metadata-symlink':meta.rename(root/'saved-metadata');meta.symlink_to(root/'saved-metadata',target_is_directory=True)
    if mode in ('regular-socket-path','socket-symlink'):
     sock.unlink()
     if mode=='regular-socket-path':sock.write_bytes(b'FOREIGN')
     else:sock.symlink_to(sentinel)
    c=dict(LaunchContract().config(),root=str(root),release=str(release),roles=['filer'],host='m02',boot_id=boot,
     deadline=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=20)).isoformat(),products={'weed':hashlib.sha256(binary.read_bytes()).hexdigest()})
    try:
     args=[sys.executable,'-I','-B',profile.__file__,'--config',json.dumps(c),'--action','deploy','--role','filer','--replacement-filer']
     r=subprocess.run(args,capture_output=True,timeout=12)
     good=mode in ('good','no-socket')
     self.assertEqual(r.returncode==0,good,r.stderr)
     self.assertEqual((root/'filer-generation.json').read_bytes(),old_bytes)
     self.assertEqual((root/'filer-replacement-generation.json').exists(),good)
     if mode!='empty-metadata':self.assertEqual(sentinel.read_bytes(),b'RETAINED-METADATA')
     if good:
      self.assertEqual((root/'filer.toml').read_bytes(),original);self.assertFalse(os.path.lexists(sock))
      self.assertIn('-localSocket='+str(sock),json.loads(r.stdout)['argv'])
     elif mode!='no-socket':self.assertTrue(os.path.lexists(sock),'refusal removed socket')
    finally:listener.close()
 def test_ready_binds_filer_replacement(self):
  for mode in ('good','old','wrong-product','missing-port'):
   with self.subTest(mode=mode),tempfile.TemporaryDirectory() as tmp:
    root=pathlib.Path(tmp);root.chmod(0o700)
    new=dict(pid=2,starttime=20,boot_id='b',executable_sha256='sha',role='filer')
    (root/'filer-generation.json').write_text(json.dumps(dict(new,pid=1,starttime=10)))
    (root/'filer-replacement-generation.json').write_text(json.dumps(new))
    current=dict(new,state='S')
    if mode=='old':current.update(pid=1,starttime=10)
    if mode=='wrong-product':current['executable_sha256']='other'
    c=dict(root=str(root),roles=['filer'],products={'weed':'sha'},services=dict(filer_http=1,filer_grpc=2))
    with patch.object(profile,'process',return_value=current),patch.object(profile,'owned_listeners',return_value={1} if mode=='missing-port' else {1,2}):
     if mode=='good':self.assertEqual(profile.services(c)['filer']['scope_identity']['PID'],2)
     else:
      with self.assertRaises(ValueError):profile.services(c)

if __name__=='__main__':unittest.main()
