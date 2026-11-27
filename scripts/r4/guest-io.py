#!/usr/bin/env python3
"""R4 guest execution stays on the holder; observed ACKs are synced off-host."""
import argparse, base64, hashlib, importlib.util, ipaddress, json, os
from pathlib import Path
import re, shlex, signal, subprocess, sys, time


def require(value, message):
    if not value:
        raise ValueError(message)


class DurableLedger:
    def __init__(self, path):
        self.path = Path(path)
        self.intents = {}
        self.acks = set()
        if self.path.exists():
            for line in self.path.read_bytes().splitlines(keepends=True):
                self.validate(line)
                self.remember(line)

    def validate(self, line):
        require(line.endswith(b'\n') and len(line) < 512, 'incomplete ledger record')
        fields = line.decode('ascii').split()
        require(len(fields) == 6, 'invalid ledger fields')
        kind, op, round_, offset, length, sha = fields
        require(kind in ('intent', 'ack') and re.fullmatch(r'[A-Za-z0-9_-]+', op)
                and round_ in ('before', 'during-loss', 'after-rejoin'), 'invalid ledger identity')
        require(offset.isdigit() and int(offset) % 4096 == 0 and length == '4096'
                and re.fullmatch('[0-9a-f]{64}', sha), 'invalid ledger block')
        value = tuple(fields[2:])
        require(op not in self.acks, 'duplicate acknowledged operation')
        if kind == 'intent':
            require(op not in self.intents, 'duplicate intent')
        else:
            require(self.intents.get(op) == value, 'ACK has no matching intent')
        return fields

    def remember(self, line):
        kind, op, *value = line.decode().split()
        if kind == 'intent': self.intents[op] = tuple(value)
        else: self.acks.add(op)

    def accept(self, line, reply):
        self.validate(line)
        with self.path.open('ab', buffering=0) as f:
            require(f.write(line) == len(line), 'short ledger write')
            os.fsync(f.fileno())
        self.remember(line)
        reply.write(b'R4_STORED ' + hashlib.sha256(line).hexdigest().encode() + b'\n')
        reply.flush()


def promotion(text):
    spec = importlib.util.spec_from_file_location('r4_auto_p2', Path(__file__).with_name('auto-p2.py'))
    mod = importlib.util.module_from_spec(spec); spec.loader.exec_module(mod)
    return mod.decode(text)['authority']['body']


def selected_host(cfg, text):
    line = promotion(text)
    mapping = {'r2': 'node-3'} if cfg['rf'] == '2' else {'r2': 'node-1', 'r3': 'node-3'}
    require(line['ReplicaID'] in mapping, 'successor is not a configured survivor')
    host = mapping[line['ReplicaID']]
    require(line['DataAddr'] == cfg['hosts'][host]['data'] + ':' + cfg['data_port'], 'foreign successor endpoint')
    return host


def ssh(cfg, host, script):
    h = cfg['hosts'][host]
    return ['ssh', '-i', cfg['key'], '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes',
            '-o', 'StrictHostKeyChecking=yes', '-o', 'UserKnownHostsFile='+h['known_hosts'],
            '-o', 'ConnectTimeout=10', '-o', 'ServerAliveInterval=5', '-o', 'ServerAliveCountMax=3',
            cfg['user']+'@'+h['management'], 'sudo -n bash -c '+shlex.quote(script)]


def remote(cfg, host, script, **kwargs):
    return subprocess.run(ssh(cfg, host, script), check=True, timeout=300, **kwargs)


def writer_script(cfg, round_, count, stream=False):
    w = '/var/lib/r4/'+cfg['tag']; q = shlex.quote
    command = 'stream' if stream else 'write'
    return ('set -eu; W='+q(w)+'; DEV=$(bash "$W/r4.sh" connect '+q(cfg['nqn'])+' 127.0.0.1 '+cfg['nvme_port']+' 30); '
            'R4_LEDGER_RELAY=1 bash "$W/r4.sh" '+command+' "$DEV" "$W/guest-ledger" '+round_+' '+str(count))


def relay(cfg, host, round_, count, stream=False):
    root = Path(cfg['root']); ledger = DurableLedger(root/'ledger')
    log = root/(round_+'.log')
    with log.open('ab', buffering=0) as err:
        p = subprocess.Popen(ssh(cfg, host, writer_script(cfg, round_, count, stream)),
                             stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=err)
        try:
            for line in iter(p.stdout.readline, b''):
                if line.startswith(b'R4_LEDGER '):
                    record = line[len(b'R4_LEDGER '):]
                    try:
                        ledger.accept(record, p.stdin)
                    except BrokenPipeError:
                        # Record is already synced; the dying host may miss its
                        # receipt. Still verify that ACK after the power loss.
                        break
                    if stream and record.startswith(b'ack '):
                        (root/'stream-ready.json').write_text(json.dumps({'acks':len(ledger.acks)})+'\n')
                else:
                    err.write(line)
            return p.wait(timeout=30)
        finally:
            if p.poll() is None:
                p.terminate()
                try: p.wait(timeout=5)
                except subprocess.TimeoutExpired: p.kill(); p.wait()


def validate_guest_exit(status, fault, still_active):
    require(not status.get('error') and status.get('remote_exit') in (255,137)
            and status.get('ended',-1)>=fault['at'], 'guest did not terminate in the power-off window')
    require(not still_active, 'old guest relay still active')


def process_identity(pid):
    try:
        fields=Path('/proc/'+str(pid)+'/stat').read_text().rsplit(')',1)[1].split()
        return None if fields[0]=='Z' else fields[19]
    except FileNotFoundError:
        return None


def active(root):
    info = json.loads((root/'stream-pid.json').read_text())
    return info if process_identity(info['pid']) == info['start'] else None


def copy_ledger(cfg, host):
    data = (Path(cfg['root'])/'ledger').read_bytes()
    require(data, 'empty off-host ledger')
    digest = hashlib.sha256(data).hexdigest(); w = '/var/lib/r4/'+cfg['tag']
    script = 'set -eu; cat > '+shlex.quote(w+'/ledger-offhost')+'; echo '+shlex.quote(digest+'  '+w+'/ledger-offhost')+' | sha256sum -c -'
    remote(cfg, host, script, input=data)
    return w


def verify(cfg, host, text, final=False):
    w = copy_ledger(cfg, host); q = shlex.quote
    script = ('set -eu; W='+q(w)+'; DEV=$(bash "$W/r4.sh" connect '+q(cfg['nqn'])+' 127.0.0.1 '+cfg['nvme_port']+' 30); ')
    if final:
        script += 'bash "$W/r4.sh" verify "$DEV" "$W/ledger-offhost" before during-loss after-rejoin'
    else:
        script += 'python3 "$W/auto-p2.py" verify '+q(text)+' "$W/r4.sh" "$DEV" "$W/ledger-offhost"; bash "$W/r4.sh" drop-control "$DEV" "$W/ledger-offhost"'
    result = remote(cfg, host, script, capture_output=True)
    (Path(cfg['root'])/('verify-final.log' if final else 'verify-failover.log')).write_bytes(result.stdout+result.stderr)
    print(result.stdout.decode(), end='')


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--config', type=Path, required=True)
    sub = p.add_subparsers(dest='command', required=True)
    init = sub.add_parser('init'); init.add_argument('--tag', required=True);init.add_argument('--rf',choices=('2','3'),required=True)
    init.add_argument('--user',required=True);init.add_argument('--key',required=True);init.add_argument('--node',nargs=4,action='append',required=True)
    init.add_argument('--data-port',required=True);init.add_argument('--nvme-port',required=True);init.add_argument('--writes',type=int,required=True)
    for cmd in ('before','stream-start','stream-ended','_stream','verify','no-ack','after-rejoin','cleanup'):sub.add_parser(cmd)
    a = p.parse_args(); root = a.config.parent
    if a.command == 'init':
        require(not root.exists(), 'guest controller directory is not fresh')
        require(re.fullmatch('[A-Za-z0-9_-]+',a.tag) and re.fullmatch('[a-z_][a-z0-9_-]*',a.user),'invalid guest identity')
        require(0<a.writes<=10000 and a.data_port.isdigit() and a.nvme_port.isdigit(),'invalid guest geometry/ports')
        require({n[0] for n in a.node}=={'node-1','node-2','node-3'} and len(a.node)==3,'exactly three pinned hosts required')
        root.mkdir(mode=0o700)
        hosts={}
        for name,management,data,pin in a.node:
            ipaddress.IPv4Address(management);ipaddress.IPv4Address(data)
            require(pin.startswith('ssh-ed25519 ') and '\n' not in pin,'invalid SSH host pin')
            known=root/(name+'-known_hosts');known.write_text(management+' '+pin+'\n')
            hosts[name]=dict(management=management,data=data,known_hosts=str(known))
        cfg=dict(root=str(root),tag=a.tag,rf=a.rf,nqn='nqn.2026-11.io.seaweedfs:r4-'+a.tag,
                 user=a.user,key=a.key,hosts=hosts,data_port=a.data_port,nvme_port=a.nvme_port,writes=a.writes)
        a.config.write_text(json.dumps(cfg,indent=2)+'\n');(root/'ledger').touch()
        return
    cfg=json.loads(a.config.read_text())
    if a.command == 'before':
        require(relay(cfg,'node-2','before',cfg['writes'])==0,'initial guest writes failed')
    elif a.command == '_stream':
        status={'started':time.monotonic()}
        try: status['remote_exit']=relay(cfg,'node-2','during-loss',0,True)
        except Exception as error: status['error']=str(error)
        status['ended']=time.monotonic();(root/'stream-ended.json').write_text(json.dumps(status)+'\n')
    elif a.command == 'stream-start':
        require(not (root/'stream-pid.json').exists(),'stream already started')
        with (root/'stream-controller.log').open('wb') as log:
            proc=subprocess.Popen([sys.executable,str(Path(__file__).resolve()),'--config',str(a.config),'_stream'],start_new_session=True,stdout=log,stderr=log)
        identity=process_identity(proc.pid);require(identity is not None,'stream process vanished')
        (root/'stream-pid.json').write_text(json.dumps(dict(pid=proc.pid,start=identity))+'\n')
        end=time.monotonic()+60
        while not (root/'stream-ready.json').exists():
            require(active(root) and time.monotonic()<end,'guest did not ACK before power-off');time.sleep(.1)
        require(active(root),'guest ended before power-off')
        (root/'fault-ready.json').write_text(json.dumps(dict(at=time.monotonic()))+'\n')
    elif a.command == 'stream-ended':
        fault=json.loads((root/'fault-ready.json').read_text());end=time.monotonic()+60
        while not (root/'stream-ended.json').exists():
            require(time.monotonic()<end,'guest SSH did not end after host power-off');time.sleep(.2)
        status=json.loads((root/'stream-ended.json').read_text())
        while active(root) and time.monotonic()<end:time.sleep(.05)
        validate_guest_exit(status,fault,bool(active(root)))
        print('guest and holder lost together; off-host ledger retained')
    elif a.command == 'cleanup':
        if (root/'stream-pid.json').exists() and active(root):
            info=active(root);os.killpg(info['pid'],signal.SIGTERM);end=time.monotonic()+5
            while active(root) and time.monotonic()<end:time.sleep(.1)
            if active(root):
                os.killpg(info['pid'],signal.SIGKILL);end=time.monotonic()+5
                while active(root) and time.monotonic()<end:time.sleep(.1)
        require(not (root/'stream-pid.json').exists() or not active(root),'guest relay still active')
    else:
        require((root/'stream-ended.json').exists() and not active(root),'old guest is not proven ended')
        text=os.environ['PROMOTION'];host=selected_host(cfg,text)
        if a.command=='verify':verify(cfg,host,text)
        elif a.command=='after-rejoin':
            require(relay(cfg,host,'after-rejoin',cfg['writes'])==0,'new guest writes failed');verify(cfg,host,text,True)
        elif a.command=='no-ack' and cfg['rf']=='2':
            w='/var/lib/r4/'+cfg['tag'];script='set -eu; W='+shlex.quote(w)+'; DEV=$(bash "$W/r4.sh" dev '+shlex.quote(cfg['nqn'])+' 127.0.0.1 10); bash "$W/r4.sh" no-ack "$DEV" "$W/no-ack-ledger" 30'
            result=remote(cfg,host,script,capture_output=True);(root/'no-ack.log').write_bytes(result.stdout+result.stderr)


if __name__=='__main__':
    main()