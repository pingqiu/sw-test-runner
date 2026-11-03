"""Disposable local S0b authority; never reads a lab GO, key, lease or install."""
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import shutil
import socket
import sys
import time
import uuid
from datetime import datetime, timezone

source, base, target, context = map(Path, sys.argv[1:5])
assert os.getuid() == 0 and base.parent == target.parent == Path('/opt')
ctx = json.loads(context.read_bytes())
weed = Path(os.environ['EC_LOCAL_WEED']).resolve(strict=True)
assert hashlib.sha256(weed.read_bytes()).hexdigest() == 'e15a1951b6e7d2b159bbeb5740f8bce5370d1d9ee3eaaaf4d9f2e38e1c2926f4', 'expected real weed 13230 binary'
python = str(Path(shutil.which('python3')).resolve(strict=True))
digest = lambda p: hashlib.sha256(Path(p).read_bytes()).hexdigest()
advertise = os.environ['EC_LOCAL_ADVERTISE']
ip = ipaddress.ip_address(advertise)
assert ip.version == 4 and (ip.is_private or ip in ipaddress.ip_network('100.64.0.0/10')) and not (ip.is_loopback or ip.is_link_local or ip.is_unspecified), 'local non-loopback private IPv4 required'
with socket.socket() as probe:
    probe.bind((advertise, 0))  # Refuse a remote address; no interface changes.
run, lease = 'local-ec-' + uuid.uuid4().hex[:12], uuid.uuid4().hex
controller, record, data, service, locks = [base / p for p in ('controller/release', 'records', 'target-output', 'controller/service', 'locks')]
for p in (controller, record, data, service, locks):
    p.mkdir(parents=True, exist_ok=False)
    p.chmod(0o755)
files = {'bin/sw-test-runner': Path(os.environ['EC_LOCAL_RUNNER']), 'bin/weed': weed,
         'scripts/testops-ci-worker.sh': source / 'scripts/testops-ci-worker.sh',
         'config/master.toml': source / 'packs/seaweed/local-ec/master.toml'}
for name in ('ec_service.py', 'ec_setup.py', 'ec_capture.py', 'ec_chaos.py', 'chaos_load.py'):
    files['worker/' + name] = source / 'packs/seaweed/worker' / name
for root in (controller, target):
    rows = []
    for name, origin in files.items():
        p = root / name
        p.parent.mkdir(parents=True, exist_ok=True)
        p.parent.chmod(0o755)
        shutil.copyfile(origin, p)
        mode = 0o644 if name.startswith('config/') else 0o755
        p.chmod(mode)
        role = 'runner' if name == 'bin/sw-test-runner' else 'product' if name == 'bin/weed' else 'config' if name.startswith('config/') else 'adapter'
        rows.append(dict(path=name, sha256=digest(p), size=p.stat().st_size, role=role, mode=format(mode, '04o')))
    p = root / 'config/build-info.json'
    p.parent.mkdir(mode=0o755, exist_ok=True)
    p.parent.chmod(0o755)
    p.write_text(json.dumps(dict(schema='ec-stage1-local/v1', product_source='13230ea1509d9a8f80dead4680ab1fb334aadb19', files=rows)))
    p.chmod(0o644)
for p in (base/'controller', service/'queue', service/'state/running', service/'state/done', service/'state/failed', service/'state/status', service/'logs', service/'results', locks/'lab.lock.ledger'):
    p.mkdir(parents=True, exist_ok=True)
    p.chmod(0o755 if p == base/'controller' else 0o700)
    if p != base/'controller': os.chown(p, 1001, 1001)
(service/'state').chmod(0o755)
data.chmod(0o700)
os.chown(data, 1001, 1001)
(locks/'lab.quarantine').mkdir(mode=0o755)
(locks/'lab.quarantine').chmod(0o755)
lock = locks/'lab.lock'
lock.touch(mode=0o600)
os.chown(lock, 1001, 1001)
template = source/'packs/seaweed/local-ec'
text = (template/'policy.json').read_text()
for a, b in {'@RELEASE@': str(target), '@OUTPUT@': str(data), '@RUN@': run, '@PYTHON@': python, '@PYTHON_SHA@': digest(python), '@ADVERTISE@': advertise}.items(): text = text.replace(a, b)
policy = json.loads(text)
host = dict(node='m02', host=ctx['host'], user='local', host_key=ctx['host_key'], root=str(data))
dep = dict(NodeID='m02', Host=ctx['host'], User='local', HostKey=ctx['host_key'], InstallRoot=str(target), ExpectedBootID=Path('/proc/sys/kernel/random/boot_id').read_text().strip(), WorkloadUID=1001, RequireNative=False, RequireInterpreter=True)
dep['Files'] = [dict(Path=str(target/name), SHA256=digest(target/name), Size=(target/name).stat().st_size, Roles=['runner' if name == 'bin/sw-test-runner' else 'product' if name == 'bin/weed' else 'config' if name.startswith('config/') else 'adapter']) for name in files]
dep['Files'] += [dict(Path=str(target/'config/build-info.json'), SHA256=digest(target/'config/build-info.json'), Size=(target/'config/build-info.json').stat().st_size, Roles=['config']), dict(Path=python, SHA256=digest(python), Size=Path(python).stat().st_size, Roles=['interpreter'])]
policy['deployments'] = [dep]
policy['scripts'] = [dict(path=str(controller/name), sha256=digest(controller/name)) for name in files if name.startswith('worker/')]
def pin(name, body):
    p = record/name
    p.write_bytes(body)
    p.chmod(0o644)
    return dict(path=str(p), sha256=digest(p))
def js(name, value): return pin(name, (json.dumps(value, indent=2)+'\n').encode())
pol = js('run-policy.json', policy)
inv = js('admission-inventory.json', dict(version='s0b-inventory-v1', hosts=[host]))
art = js('artifacts.json', dict(version='s0b-artifacts-v1', deployments=[dep]))
scenario = pin('scenario.yaml', (template/'scenario.yaml').read_bytes())
stamp = lambda delta: datetime.fromtimestamp(time.time()+delta, timezone.utc).isoformat().replace('+00:00','Z')
binding = dict(request_id=run, run_id=run, workload='s3', transport='http', inventory=inv, scenario=scenario, policy=pol, artifacts=art, hosts=[host], lease_id=lease, lease_owner='local', lease_resource='m01,m02', output_dir=str(data), work_deadline=stamp(480), hard_deadline=stamp(541), cleanup_reserve_ms=60000)
request = (json.dumps(dict(version='s0b-request-v1', binding=binding), indent=2)+'\n').encode()
p = service/'queue'/(run+'.env')
p.write_bytes(request)
os.chown(p, 1001, 1001)
p.chmod(0o600)
go = js('go.json', dict(version='s0b-go-v1', source_id=uuid.uuid4().hex[:24], request_sha256=hashlib.sha256(request).hexdigest(), binding=binding, cleanup=[], allow_mutating=True))
js('bootstrap.json', dict(version='s0b-bootstrap-v1', worker_lease=dict(lock_file=str(lock)), approval=go, policy=pol, running_dir=str(service/'state/running'), ledger_dir=str(locks/'lab.lock.ledger'), cancel_file=str(record/'cancel.json'), key_files=dict(m02=ctx['key_file']), results_dir=str(service/'results')))
st = data.stat()
p = data/'.ec-stage1-owned.json'
p.write_text(json.dumps(dict(run=run, lease=lease, device=st.st_dev, inode=st.st_ino)))
p.chmod(0o444)
env = dict(TESTOPS_QUEUE_DIR=str(service/'queue'), TESTOPS_STATE_DIR=str(service/'state'), TESTOPS_STATUS_DIR=str(service/'state/status'), TESTOPS_LOG_DIR=str(service/'logs'), TESTOPS_LOCK_FILE=str(lock), TESTOPS_APPROVED_ADAPTER=str(controller/'bin/sw-test-runner'), TESTOPS_APPROVED_ADAPTER_SHA256=digest(controller/'bin/sw-test-runner'), TESTOPS_APPROVED_POLICY=str(record/'bootstrap.json'), TESTOPS_NOTIFY_CMD=':', PYTHONDONTWRITEBYTECODE='1', PATH='/usr/bin:/bin')
(base/'fixture.json').write_text(json.dumps(dict(run=run, lease=lease, env=env, worker=str(controller/'scripts/testops-ci-worker.sh'), service=str(service), data=str(data), record=str(record))))
