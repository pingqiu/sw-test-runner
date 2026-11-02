"""CPU-only replacement launch/readiness controls over the real profile program."""
import datetime, hashlib, json, os, pathlib, shutil, subprocess, sys, tempfile, unittest
from unittest.mock import patch
import lab_profile as profile
from test_lab_profile import LaunchContract

class ReplacementControls(unittest.TestCase):
    @unittest.skipUnless(sys.platform=='linux' and shutil.which('cc'),'Linux ELF fixture')
    def test_cli_replacement_preserves_data_and_refuses_live_or_foreign_old(self):
        for mode in ('good','live-old','foreign-boot'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temporary:
                base=pathlib.Path(temporary);root=base/'run';release=base/'release'
                root.mkdir(mode=0o700);(root/'volume').mkdir(mode=0o700)
                sentinel=root/'volume'/'1.dat';sentinel.write_bytes(b'PERSISTED-SEED')
                (release/'bin').mkdir(parents=True);binary=release/'bin'/'weed-volume'
                built=subprocess.run(['cc','-x','c','-','-o',str(binary)],input=b'int main(){return 0;}',capture_output=True,timeout=10)
                self.assertEqual(built.returncode,0,built.stderr);binary.chmod(0o755)
                boot=pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()
                old=profile.process(os.getpid()) if mode=='live-old' else dict(pid=99999999,starttime=1,boot_id=boot)
                if mode=='foreign-boot':old['boot_id']='foreign'
                original=json.dumps(old).encode();(root/'volume-generation.json').write_bytes(original)
                c=dict(LaunchContract().config(),root=str(root),release=str(release),roles=['volume'],host='m02',boot_id=boot,
                    deadline=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=20)).isoformat(),
                    device='CPU-NO-RDMA',gid_index=3,products={'weed-volume':hashlib.sha256(binary.read_bytes()).hexdigest()})
                args=[sys.executable,'-I','-B',profile.__file__,'--config',json.dumps(c),'--action','deploy','--role','volume','--replacement-volume']
                child=subprocess.run(args,capture_output=True,timeout=12)
                self.assertEqual(child.returncode==0,mode=='good',child.stderr)
                self.assertEqual(sentinel.read_bytes(),b'PERSISTED-SEED')
                self.assertEqual((root/'volume-generation.json').read_bytes(),original)
                replacement=root/'volume-replacement-generation.json'
                self.assertEqual(replacement.exists(),mode=='good')
                if mode=='good':self.assertEqual(json.loads(replacement.read_text())['pid'],child.pid if hasattr(child,'pid') else json.loads(child.stdout)['pid'])
    def test_client_ack_is_probe_entry_not_post_readiness(self):
        c=dict(deadline=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=100)).isoformat(),
               server_address='127.0.0.1',server_services=dict(master_http=1,volume_http=2,filer_http=3))
        with patch.object(profile.time,'monotonic',side_effect=[100.,101.,102.,103.]),patch.object(profile,'services',return_value={}),patch.object(profile,'http_json',return_value={'IsLeader':True}),patch.object(profile,'topology_contains',return_value=True):
            self.assertEqual(profile.ready(c)['client_mono'],100.)
    def test_services_uses_replacement_exact_generation_ports_and_logs(self):
        for mode in ('good','wrong-generation','missing-port','missing-cm','missing-dc'):
            with self.subTest(mode=mode),tempfile.TemporaryDirectory() as temporary:
                root=pathlib.Path(temporary);root.chmod(0o700)
                current=dict(pid=2,starttime=20,boot_id='b',executable_sha256='sha',state='S')
                new=dict(current,role='volume',logs=[])
                (root/'volume-generation.json').write_text(json.dumps(dict(new,pid=1,starttime=10)))
                (root/'volume-replacement-generation.json').write_text(json.dumps(new))
                c=dict(root=str(root),roles=['volume'],products={'weed-volume':'sha'},services=dict(volume_http=1,volume_grpc=2,rdma_control=3,dc_control=4,rdma=5),server_address='10.0.2.3')
                ports={1,2,3,4};markers=['RDMA DC push-read writer active','RDMA listener bound on 10.0.2.3:5']
                if mode=='missing-port':ports.remove(4)
                if mode=='missing-cm':markers.pop()
                if mode=='missing-dc':markers.pop(0)
                if mode=='wrong-generation':current=dict(current,pid=1,starttime=10)
                with patch.object(profile,'process',return_value=current),patch.object(profile,'owned_listeners',return_value=ports),patch.object(profile,'log_evidence',return_value=[dict(markers=markers)]):
                    if mode=='good':
                        result=profile.services(c)
                        self.assertEqual(result['volume']['scope_identity']['PID'],2)
                        self.assertEqual(result['volume']['scope_identity']['StartTime'],20)
                    else:
                        with self.assertRaises(ValueError):profile.services(c)

if __name__=='__main__':unittest.main()
