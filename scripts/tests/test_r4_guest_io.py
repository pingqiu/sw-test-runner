import base64, copy, hashlib, importlib.util, io, json, os
from pathlib import Path
import subprocess, sys, tempfile, unittest, signal, threading, time, selectors
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('guest_io',Path(__file__).resolve().parents[1]/'r4/guest-io.py')
guest=importlib.util.module_from_spec(spec);spec.loader.exec_module(guest)


class GuestLedger(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name);self.path=self.root/'ledger'
        self.intent=b'intent op1 during-loss 0 4096 '+b'a'*64+b'\n'
        self.ack=self.intent.replace(b'intent ',b'ack ',1)

    def own(self,p,seconds=8):
        def stop():
            if p.poll() is None:
                try:os.killpg(p.pid,signal.SIGKILL)
                except ProcessLookupError:pass
            p.wait(timeout=3)
            for stream in (p.stdin,p.stdout,p.stderr):
                if stream:stream.close()
        self.addCleanup(stop)
        timer=threading.Timer(seconds,lambda: os.killpg(p.pid,signal.SIGKILL) if p.poll() is None else None)
        timer.daemon=True;timer.start();self.addCleanup(timer.cancel)
        return p

    @unittest.skipUnless(sys.platform=='linux','Bounded process-group protocol control')
    def test_stalled_protocol_is_killed_and_reaped(self):
        p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)'],stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
        self.own(p,seconds=.2)
        self.assertEqual(p.stdout.readline(),b'')
        self.assertNotEqual(p.wait(timeout=2),0)
        self.assertFalse(Path('/proc/'+str(p.pid)).exists())

    def test_record_is_synced_before_receipt_and_reopens(self):
        ledger=guest.DurableLedger(self.path);reply=io.BytesIO()
        synced=[]
        original=guest.os.fsync
        class OrderedReply(io.BytesIO):
            def write(inner,value):
                self.assertEqual(len(synced),len(inner.getvalue().splitlines())+1,'receipt preceded successful fsync')
                return super().write(value)
        reply=OrderedReply()
        def sync(fd):original(fd);synced.append(True)
        with patch.object(guest.os,'fsync',side_effect=sync):
            ledger.accept(self.intent,reply);ledger.accept(self.ack,reply)
        self.assertEqual(self.path.read_bytes(),self.intent+self.ack)
        self.assertEqual(reply.getvalue(),b''.join(b'R4_STORED '+hashlib.sha256(x).hexdigest().encode()+b'\n' for x in (self.intent,self.ack)))
        self.assertEqual(guest.DurableLedger(self.path).acks,{'op1'})

    def test_failed_fsync_never_confirms_record(self):
        reply=io.BytesIO()
        with patch.object(guest.os,'fsync',side_effect=OSError('disk unavailable')):
            with self.assertRaises(OSError):guest.DurableLedger(self.path).accept(self.intent,reply)
        self.assertEqual(reply.getvalue(),b'')
        # Separate ACK-only injection: intent was already synced successfully.
        self.path.unlink();ledger=guest.DurableLedger(self.path);ledger.accept(self.intent,io.BytesIO())
        reply=io.BytesIO()
        with patch.object(guest.os,'fsync',side_effect=OSError('ACK sync unavailable')):
            with self.assertRaises(OSError):ledger.accept(self.ack,reply)
        self.assertEqual(reply.getvalue(),b'')

    def test_forged_ack_partial_record_and_wrong_image_refuse(self):
        for record in (self.ack,self.intent[:-1],b'intent op1 before 1 4096 '+b'a'*64+b'\n'):
            with self.subTest(record=record):
                with self.assertRaises(ValueError):guest.DurableLedger(self.path).accept(record,io.BytesIO())
        ledger=guest.DurableLedger(self.path);ledger.accept(self.intent,io.BytesIO())
        with self.assertRaises(ValueError):ledger.accept(self.ack.replace(b'a'*64,b'b'*64),io.BytesIO())
        self.assertEqual(self.path.read_bytes(),self.intent)

    def test_new_guest_requires_old_guest_ended_in_the_fault_window(self):
        good=dict(remote_exit=255,ended=20,ended_unix_ns=200)
        guest.validate_guest_exit(good,dict(at=10,key='node-2/owned-vm',dispatch_unix_ns=100),False)
        for status,active in ((good,True),(dict(remote_exit=255,ended=9),False),
                              (dict(remote_exit=0,ended=20),False),(dict(remote_exit=-9,ended=20),False),(dict(error='invalid ledger',remote_exit=255,ended=20),False)):
            with self.subTest(status=status,active=active),self.assertRaises(ValueError):
                guest.validate_guest_exit(status,dict(at=10,key='node-2/owned-vm',dispatch_unix_ns=100),active)

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
        p=subprocess.Popen(['bash',str(helper),'stream',str(dev),str(remote_ledger),'during-loss','3'],env=env,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
        self.own(p)
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

    @unittest.skipUnless(sys.platform=='linux','Real shell with instrumented device writer')
    def test_failed_partial_device_write_never_emits_ack(self):
        helper=Path(__file__).resolve().parents[2]/'scenarios/v3block/r4.sh'
        dev=self.root/'device';dev.write_bytes(bytes(32*4096))
        dd=self.root/'bad-dd'
        dd.write_text("#!/usr/bin/env python3\nimport os,sys\np=next(a[3:] for a in sys.argv if a.startswith('of='))\nwith open(p,'r+b') as f:f.write(b'x'*512)\nsys.exit(17)\n");dd.chmod(0o700)
        env=dict(os.environ,R4_ALLOW_FILE='1',R4_LEDGER_RELAY='1',R4_DD=str(dd))
        p=subprocess.Popen(['bash',str(helper),'write',str(dev),str(self.root/'local'),'before','1'],env=env,
            stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True,bufsize=0)
        self.own(p)
        frame=p.stdout.readline();self.assertTrue(frame.startswith(b'R4_LEDGER intent '))
        guest.DurableLedger(self.path).accept(frame[10:],p.stdin)
        out,err=p.communicate(timeout=5)
        self.assertNotEqual(p.returncode,0,err);self.assertNotIn(b'R4_LEDGER ack ',out)
        self.assertEqual(dev.read_bytes()[:512],b'x'*512,'failing device operation was not exercised')
        self.assertFalse(guest.DurableLedger(self.path).acks)

    @unittest.skipUnless(sys.platform=='linux','Real shell with withheld ACK receipt')
    def test_withheld_ack_receipt_blocks_the_following_write(self):
        helper=Path(__file__).resolve().parents[2]/'scenarios/v3block/r4.sh'
        dev=self.root/'device';dev.write_bytes(bytes(32*4096));calls=self.root/'dd-calls'
        dd=self.root/'count-dd'
        dd.write_text('#!/bin/sh\ndd "$@" || exit $?\necho completed >> "$DD_CALLS"\n');dd.chmod(0o700)
        env=dict(os.environ,R4_ALLOW_FILE='1',R4_LEDGER_RELAY='1',R4_DD=str(dd),DD_CALLS=str(calls),R4_DD_OFLAG='sync')
        p=subprocess.Popen(['bash',str(helper),'write',str(dev),str(self.root/'local'),'before','2'],env=env,
            stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True,bufsize=0)
        self.own(p);ledger=guest.DurableLedger(self.path)
        frame=p.stdout.readline();self.assertTrue(frame.startswith(b'R4_LEDGER intent '));ledger.accept(frame[10:],p.stdin)
        frame=p.stdout.readline();self.assertTrue(frame.startswith(b'R4_LEDGER ack '))
        self.assertEqual(calls.read_text().splitlines(),['completed'],'ACK was emitted before the device completed')
        ledger.accept(frame[10:],io.BytesIO()) # sync ACK, deliberately withhold its guest receipt
        with selectors.DefaultSelector() as selector:
            selector.register(p.stdout,selectors.EVENT_READ)
            self.assertEqual(selector.select(.3),[],'guest advanced without its previous ACK receipt')
        self.assertEqual(calls.read_text().splitlines(),['completed'])
        out,err=p.communicate(b'R4_STORED wrong\n',timeout=5)
        self.assertNotEqual(p.returncode,0,err);self.assertNotIn(b'R4_LEDGER intent ',out)

    @unittest.skipUnless(sys.platform=='linux','Full CLI control with inert SSH and regular files')
    def test_cli_loses_old_guest_and_reads_every_off_host_ack_on_new_guest(self):
        helper=Path(guest.__file__);r4=helper.parents[1]/'../scenarios/v3block/r4.sh'
        r4=r4.resolve();tools=self.root/'tools';tools.mkdir()
        dev=self.root/'device';dev.write_bytes(bytes(64*4096))
        hosts=self.root/'hosts';hosts.mkdir()
        for name in ('node-1','node-2','node-3'):
            w=hosts/name;w.mkdir()
            (w/'r4.sh').write_text('#!/bin/sh\ncase "$1" in connect|dev) echo "$FAKE_DEVICE";; *) exec bash "$REAL_R4" "$@";; esac\n')
            (w/'auto-p2.py').write_bytes(helper.with_name('auto-p2.py').read_bytes())
        ssh=tools/'ssh'
        ssh.write_text(r'''#!/usr/bin/env python3
import json,os,pathlib,shlex,subprocess,sys
args=sys.argv[1:]
assert 'StrictHostKeyChecking=yes' in args and any(a.startswith('UserKnownHostsFile=') for a in args)
peer=next(a for a in args if a.startswith('swtest@'))
node={'192.0.2.1':'node-1','192.0.2.2':'node-2','192.0.2.3':'node-3'}[peer.split('@')[1]]
root=pathlib.Path(os.environ['FAKE_HOSTS']);w=root/node
command=args[-1].replace('/var/lib/r4/test-rf3',str(w))
parts=shlex.split(command);assert parts[:3]==['sudo','-n','bash']
p=subprocess.Popen(parts[2:],start_new_session=True)
(w/'pid').write_text(str(p.pid))
with (root/'ssh.jsonl').open('a') as f:f.write(json.dumps(dict(node=node,command=command))+'\n')
rc=p.wait();sys.exit(128-rc if rc<0 else rc)
''');ssh.chmod(0o755)
        cfg=self.root/'observer/config.json'
        env=dict(os.environ,PATH=str(tools)+os.pathsep+os.environ['PATH'],FAKE_HOSTS=str(hosts),
                 FAKE_DEVICE=str(dev),REAL_R4=str(r4),R4_ALLOW_FILE='1',R4_DD_OFLAG='sync',R4_DD_IFLAG='fullblock')
        fault_ns=None
        def cli(command,*args,token=None,success=True):
            e=dict(env)
            if token:e['PROMOTION']=token
            if command=='stream-ended':args=(*args,'--fault-dispatch-unix-ns',str(fault_ns),'--fault-key','node-2/local-kill')
            result=subprocess.run([sys.executable,str(helper),'--config',str(cfg),command,*args],env=e,capture_output=True,timeout=20)
            if not success:
                self.assertNotEqual(result.returncode,0,result.stdout+result.stderr);return result
            self.assertEqual(result.returncode,0,result.stdout+result.stderr+b"\n"+b"\n".join(p.read_bytes()[-3000:] for p in cfg.parent.glob("*.log")))
            return result.stdout
        args=['--tag','test-rf3','--rf','3','--user','swtest','--key','unused','--data-port','19601','--nvme-port','4420','--writes','4']
        for i in (1,2,3):args+=['--node',f'node-{i}',f'192.0.2.{i}',f'10.84.1.{10+i}','ssh-ed25519 TEST']
        cli('init',*args)
        value=dict(authority={'body':dict(VolumeID='v1',ReplicaID='r2',Epoch=2,DataAddr='10.84.1.11:19601',
                   Primary=dict(Kind=2,FencedEpoch=2,Basis=1,DurableLSN=1))},mode='automatic',operator_calls=0)
        token=base64.b64encode(json.dumps(value).encode()).decode()
        def blocked_successors():
            before=(hosts/'ssh.jsonl').read_bytes()
            for command in ('verify','after-rejoin'):cli(command,token=token,success=False)
            self.assertEqual((hosts/'ssh.jsonl').read_bytes(),before,'unproven old guest dispatched a successor')
        try:
            cli('before');blocked_successors() # no ended proof
            cli('stream-start');blocked_successors() # real old relay remains active
            # Even a syntactically valid stale ended record cannot override
            # the actual, still-running old relay (exercise the active guard).
            now=time.monotonic();wall=time.time_ns()
            guest.publish(cfg.parent/'stream-ended.json',dict(remote_exit=255,ended=now,ended_unix_ns=wall))
            guest.publish(cfg.parent/'fault-dispatch.json',dict(at=now-1,key='node-2/local-kill',dispatch_unix_ns=wall-1))
            blocked_successors()
            (cfg.parent/'stream-ended.json').unlink();(cfg.parent/'fault-dispatch.json').unlink()
            pid=int((hosts/'node-2/pid').read_text());fault_ns=time.time_ns();os.killpg(pid,9)
            dispatched=fault_ns
            # Real process ends after readiness but BEFORE a delayed cloud dispatch.
            end=time.monotonic()+5
            while not (cfg.parent/'stream-ended.json').exists():
                self.assertLess(time.monotonic(),end);time.sleep(.02)
            fault_ns=time.time_ns()
            cli('stream-ended',success=False);blocked_successors()
            fault_ns=dispatched
            cli('stream-ended')
            (hosts/'node-2/guest-ledger').unlink() # the authoritative ACK ledger stays off-host
            value=dict(authority={'body':dict(VolumeID='v1',ReplicaID='r2',Epoch=2,DataAddr='10.84.1.11:19601',
                       Primary=dict(Kind=2,FencedEpoch=2,Basis=1,DurableLSN=1))},mode='automatic',operator_calls=0)
            token=base64.b64encode(json.dumps(value).encode()).decode()
            self.assertIn(b'"P2": "PASS"',cli('verify',token=token))
            self.assertIn(b'mismatches=0',cli('after-rejoin',token=token))
            rows=(cfg.parent/'ledger').read_text().splitlines()
            self.assertEqual(sum(line.startswith('ack ') and ' before ' in line for line in rows),4)
            self.assertEqual(sum(line.startswith('ack ') and ' after-rejoin ' in line for line in rows),4)
            self.assertGreaterEqual(sum(line.startswith('ack ') and ' during-loss ' in line for line in rows),1)
            calls=[json.loads(line) for line in (hosts/'ssh.jsonl').read_text().splitlines()]
            self.assertEqual([call['node'] for call in calls[:2]],['node-2','node-2'])
            self.assertTrue(all(call['node']=='node-1' for call in calls[2:]))
        finally:
            cli('cleanup')
            for p in hosts.glob('*/pid'):
                pid=int(p.read_text())
                try:
                    cmd=Path('/proc/'+str(pid)+'/cmdline').read_bytes()
                    if str(hosts).encode() in cmd:os.killpg(pid,9)
                except (FileNotFoundError,ProcessLookupError):pass

if __name__=='__main__':unittest.main()
