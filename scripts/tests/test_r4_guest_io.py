import base64, copy, hashlib, importlib.util, io, json, os
from pathlib import Path
import subprocess, sys, tempfile, unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('guest_io',Path(__file__).resolve().parents[1]/'r4/guest-io.py')
guest=importlib.util.module_from_spec(spec);spec.loader.exec_module(guest)


class GuestLedger(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name);self.path=self.root/'ledger'
        self.intent=b'intent op1 during-loss 0 4096 '+b'a'*64+b'\n'
        self.ack=self.intent.replace(b'intent ',b'ack ',1)

    def test_record_is_synced_before_receipt_and_reopens(self):
        ledger=guest.DurableLedger(self.path);reply=io.BytesIO()
        ledger.accept(self.intent,reply);ledger.accept(self.ack,reply)
        self.assertEqual(self.path.read_bytes(),self.intent+self.ack)
        self.assertEqual(reply.getvalue(),b''.join(b'R4_STORED '+hashlib.sha256(x).hexdigest().encode()+b'\n' for x in (self.intent,self.ack)))
        self.assertEqual(guest.DurableLedger(self.path).acks,{'op1'})

    def test_failed_fsync_never_confirms_record(self):
        reply=io.BytesIO()
        with patch.object(guest.os,'fsync',side_effect=OSError('disk unavailable')):
            with self.assertRaises(OSError):guest.DurableLedger(self.path).accept(self.intent,reply)
        self.assertEqual(reply.getvalue(),b'')

    def test_forged_ack_partial_record_and_wrong_image_refuse(self):
        for record in (self.ack,self.intent[:-1],b'intent op1 before 1 4096 '+b'a'*64+b'\n'):
            with self.subTest(record=record):
                with self.assertRaises(ValueError):guest.DurableLedger(self.path).accept(record,io.BytesIO())
        ledger=guest.DurableLedger(self.path);ledger.accept(self.intent,io.BytesIO())
        with self.assertRaises(ValueError):ledger.accept(self.ack.replace(b'a'*64,b'b'*64),io.BytesIO())
        self.assertEqual(self.path.read_bytes(),self.intent)

    def test_new_guest_requires_old_guest_ended_in_the_fault_window(self):
        good=dict(remote_exit=255,ended=20)
        guest.validate_guest_exit(good,dict(at=10),False)
        for status,active in ((good,True),(dict(remote_exit=255,ended=9),False),
                              (dict(remote_exit=0,ended=20),False),(dict(remote_exit=-9,ended=20),False),(dict(error='invalid ledger',remote_exit=255,ended=20),False)):
            with self.subTest(status=status,active=active),self.assertRaises(ValueError):
                guest.validate_guest_exit(status,dict(at=10),active)

    def test_successor_is_selected_only_from_the_pinned_replication_line(self):
        cfg=dict(rf='3',data_port='19601',hosts={'node-1':dict(data='10.84.1.11'),'node-3':dict(data='10.84.1.13')})
        value=dict(authority={'body':dict(VolumeID='v1',ReplicaID='r2',Epoch=2,DataAddr='10.84.1.11:19601',
                    Primary=dict(Kind=2,FencedEpoch=2,Basis=1,DurableLSN=1))},mode='automatic',operator_calls=0)
        def token(v):return base64.b64encode(json.dumps(v).encode()).decode()
        self.assertEqual(guest.selected_host(cfg,token(value)),'node-1')
        value['authority']['body'].update(ReplicaID='r3',DataAddr='10.84.1.13:19601')
        self.assertEqual(guest.selected_host(cfg,token(value)),'node-3')
        cfg['rf']='2';value['authority']['body']['ReplicaID']='r2'
        self.assertEqual(guest.selected_host(cfg,token(value)),'node-3')
        for changes in (dict(DataAddr='10.84.1.99:19601'),dict(ReplicaID='r1'),dict(Epoch=1)):
            bad=copy.deepcopy(value);bad['authority']['body'].update(changes)
            with self.assertRaises(ValueError):guest.selected_host(cfg,token(bad))

    def test_lost_receipt_does_not_erase_an_already_synced_ack(self):
        ledger=guest.DurableLedger(self.path);ledger.accept(self.intent,io.BytesIO())
        class LostReply:
            def write(self,value):raise BrokenPipeError('host powered off')
        with self.assertRaises(BrokenPipeError):ledger.accept(self.ack,LostReply())
        self.assertEqual(guest.DurableLedger(self.path).acks,{'op1'})

    def test_remote_guest_script_is_always_loopback(self):
        for streaming in (False,True):
            script=guest.writer_script(dict(tag='owned-rf3',nqn='our-nqn',nvme_port='4420'),'before',1,streaming)
            self.assertIn('our-nqn 127.0.0.1 4420 30',script)
            self.assertNotIn('allow-external',script)
            self.assertIn('R4_LEDGER_RELAY=1',script)

    @unittest.skipUnless(sys.platform=='linux','Linux shell controls; regular files only')
    def test_real_guest_waits_for_durable_intent_and_ack_receipts(self):
        helper=Path(__file__).resolve().parents[2]/'scenarios/v3block/r4.sh'
        dev=self.root/'dev';dev.write_bytes(bytes(32*4096))
        remote_ledger=self.root/'guest-ledger'
        env=dict(os.environ,R4_ALLOW_FILE='1',R4_LEDGER_RELAY='1',R4_DD_OFLAG='sync',R4_DD_IFLAG='fullblock')
        p=subprocess.Popen(['bash',str(helper),'stream',str(dev),str(remote_ledger),'during-loss','3'],env=env,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        ledger=guest.DurableLedger(self.path)
        for _ in range(6):
            frame=p.stdout.readline();self.assertTrue(frame.startswith(b'R4_LEDGER '));ledger.accept(frame[10:],p.stdin)
        out,err=p.communicate(timeout=10);self.assertEqual(p.returncode,0,err);self.assertEqual(out,b'')
        self.assertEqual(len(ledger.acks),3)
        result=subprocess.run(['bash',str(helper),'verify',str(dev),str(self.path),'during-loss'],env=env,capture_output=True,timeout=10)
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertIn(b'mismatches=0',result.stdout)

    @unittest.skipUnless(sys.platform=='linux','Linux shell controls; regular files only')
    def test_missing_or_wrong_off_host_receipt_prevents_device_write(self):
        helper=Path(__file__).resolve().parents[2]/'scenarios/v3block/r4.sh'
        dev=self.root/'dev';dev.write_bytes(bytes(32*4096))
        env=dict(os.environ,R4_ALLOW_FILE='1',R4_LEDGER_RELAY='1',R4_DD_OFLAG='sync')
        for reply in (b'',b'R4_STORED wrong\n'):
            result=subprocess.run(['bash',str(helper),'write',str(dev),str(self.root/'remote'),'before','1'],input=reply,env=env,capture_output=True,timeout=10)
            self.assertNotEqual(result.returncode,0)
            self.assertIn(b'off-host ledger receipt',result.stderr)
            self.assertEqual(dev.read_bytes(),bytes(32*4096))


if __name__=='__main__':unittest.main()