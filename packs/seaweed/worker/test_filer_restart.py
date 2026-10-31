"""Local-only CLI checks; ELF exec replaces product/hardware, never a lab run."""
import datetime,hashlib,json,os,pathlib,shutil,socket,subprocess,sys,tempfile,time,unittest
from unittest.mock import patch
import lab_profile as profile
from test_lab_profile import LaunchContract

@unittest.skipUnless(sys.platform=='linux','Linux private socket/process seam')
class FilerReplacement(unittest.TestCase):
 @unittest.skipUnless(shutil.which('cc'), 'ELF product fixture')
 def test_actual_deploy_kill_replacement_with_worker_umask(self):
  for writable in (False,True):
   with self.subTest(group_writable=writable),tempfile.TemporaryDirectory() as tmp:
    base=pathlib.Path(tmp);root=base/'run';root.mkdir(mode=0o700)
    release=base/'release';(release/'bin').mkdir(parents=True);binary=release/'bin'/'weed'
    source=b'''#include <unistd.h>
#include <fcntl.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <string.h>
int main(){int f=open("filer/ldb2/CURRENT",O_WRONLY|O_CREAT,0600);
if(f<0)return 2;write(f,"METADATA",8);close(f);
int s=socket(AF_UNIX,SOCK_STREAM,0);struct sockaddr_un a={.sun_family=AF_UNIX};
strcpy(a.sun_path,"filer.sock");if(bind(s,(void*)&a,sizeof(a)))return 3;
for(;;)pause();}
'''
    subprocess.run(['cc','-x','c','-','-o',str(binary)],input=source,capture_output=True,timeout=10,check=True)
    binary.chmod(0o755)
    c=dict(LaunchContract().config(),root=str(root),release=str(release),roles=['filer'],host='m02',
     boot_id=pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
     deadline=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=25)).isoformat(),products={'weed':hashlib.sha256(binary.read_bytes()).hexdigest()})
    args=[sys.executable,'-I','-B',profile.__file__,'--config',json.dumps(c),'--action','deploy','--role','filer']
    def wait_for(child,marker):
     until=time.monotonic()+5
     while time.monotonic()<until and child.poll() is None:
      if marker.exists() and (root/'filer.sock').exists():return
      time.sleep(.02)
     self.fail('product not started; exit='+str(child.poll()))
    with (base/'stdout').open('wb') as stdout,(base/'stderr').open('wb') as stderr:
     old=subprocess.Popen(args,stdout=stdout,stderr=stderr,umask=0o002)
     try:wait_for(old,root/'filer-generation.json')
     finally:old.kill();old.wait(timeout=5)
     self.assertEqual(old.returncode,-9)
     binding=json.loads((root/'filer-generation.json').read_text())['metadata_directory']
     if writable:(root/'filer.toml').chmod(0o660)
     new=subprocess.Popen(args+['--replacement-filer'],stdout=stdout,stderr=stderr,umask=0o002)
     try:
      if writable:
       self.assertEqual(new.wait(timeout=5),1)
       self.assertFalse((root/'filer-replacement-generation.json').exists())
      else:
       wait_for(new,root/'filer-replacement-generation.json')
       self.assertEqual((root/'filer.toml').stat().st_mode&0o777,0o600)
       self.assertEqual(json.loads((root/'filer-replacement-generation.json').read_text())['metadata_directory'],binding)
     finally:
      if new.poll() is None:new.kill()
      new.wait(timeout=5)
    if writable:self.assertIn('replacement filer state not private',(base/'stderr').read_text())

 def test_actual_cli_preserves_metadata_and_refuses_foreign_or_live_state(self):
  for mode in ('good','no-socket','recreated-nonempty','unbound-marker','wrong-device','live-old','foreign-boot','bad-config','config-symlink','empty-metadata','metadata-symlink','regular-socket-path','socket-symlink','active-socket','foreign-socket'):
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
    m=meta.lstat();old['metadata_directory']=dict(device=m.st_dev,inode=m.st_ino)
    if mode=='unbound-marker':old.pop('metadata_directory')
    if mode=='wrong-device':old['metadata_directory']['device']+=1
    old_bytes=json.dumps(old).encode();(root/'filer-generation.json').write_bytes(old_bytes)
    sock=root/'filer.sock';listener=socket.socket(socket.AF_UNIX)
    if mode!='no-socket':listener.bind(str(sock))
    if mode!='active-socket':listener.close()
    if mode=='foreign-socket':os.chown(sock,os.getuid()+1,os.getgid())
    if mode=='bad-config':(root/'filer.toml').write_bytes(b'OTHER')
    if mode=='config-symlink':(root/'filer.toml').unlink();(root/'filer.toml').symlink_to(sentinel)
    if mode=='recreated-nonempty':
     meta.rename(root/'original-metadata');meta.mkdir(mode=0o700);sentinel.write_bytes(b'RETAINED-METADATA')
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
 def test_metadata_recheck_precedes_socket_unlink_and_exec(self):
  for boundary in ('before-unlink','before-exec'):
   with self.subTest(boundary=boundary),tempfile.TemporaryDirectory() as tmp:
    root=pathlib.Path(tmp)/'run';root.mkdir(mode=0o700);(root/'filer').mkdir(mode=0o700)
    meta=root/'filer'/'ldb2';meta.mkdir(mode=0o700);(meta/'CURRENT').write_bytes(b'SEED')
    release=pathlib.Path(tmp)/'release';(release/'bin').mkdir(parents=True)
    binary=release/'bin'/'weed';shutil.copyfile('/bin/true',binary);binary.chmod(0o755)
    boot=pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()
    original=dict(pid=99999999,starttime=1,boot_id=boot,metadata_directory=profile.filer_metadata(root))
    (root/'filer-generation.json').write_text(json.dumps(original));(root/'filer.toml').write_text(profile.filer_config(root))
    sock=root/'filer.sock'
    with socket.socket(socket.AF_UNIX) as listener:listener.bind(str(sock))
    c=dict(LaunchContract().config(),root=str(root),release=str(release),roles=['filer'],host='m02',boot_id=boot,
     deadline=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=20)).isoformat(),products={'weed':hashlib.sha256(binary.read_bytes()).hexdigest()})
    def recreate(*args):
     meta.rename(root/'saved');meta.mkdir(mode=0o700);(meta/'CURRENT').write_bytes(b'SEED')
    original_check=profile.filer_metadata;calls=[]
    def check(*args):
     calls.append(1)
     if boundary=='before-unlink' and len(calls)==2:recreate()
     return original_check(*args)
    with patch.object(profile,'filer_metadata',side_effect=check),patch.object(profile.os,'chdir',side_effect=recreate if boundary=='before-exec' else lambda *a:None),patch.object(profile.os,'execve') as execute:
     with self.assertRaisesRegex(ValueError,'metadata directory changed'):profile.launch(c,'filer',True)
     execute.assert_not_called()
    if boundary=='before-unlink':self.assertTrue(sock.exists(),'socket removed before requalification')

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
