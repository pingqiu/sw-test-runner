#!/usr/bin/env python3
"""Start the pinned R4 argv locally before invoking the cloud controller."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import socket
import subprocess
import tempfile
import time

import yaml


def require(ok, message):
    if not ok:
        raise ValueError(message)


def command(action, values, work):
    """Extract the actual unit argv, not a separately maintained smoke command."""
    text = re.sub(r"{{\s*(\w+)\s*}}", lambda m: values[m[1]], action['cmd'])
    words = shlex.split(text)
    require('unit' in words, 'missing scenario unit command')
    role = words[words.index('unit')+2]
    start = words.index('--', words.index('unit'))+1
    end = words.index('&&', start)
    argv = [w.replace('$W', str(work)).replace('$T', 'local-start-smoke')
            .replace('$R', 'r3' if values['rf'] == '3' else 'r2') for w in words[start:end]]
    require(not any('$' in w or '{{' in w for w in argv), 'unresolved command value')
    return role, argv


def port(address):
    with socket.socket() as s:
        s.bind((address, 0))
        return str(s.getsockname()[1])


def set_arg(argv, flag, value):
    require(argv.count(flag) == 1, 'expected exactly one '+flag)
    argv[argv.index(flag)+1] = value


def stop(processes):
    # Only direct children recorded by this invocation; never name-based kills.
    for p in reversed(processes):
        if p.poll() is None:
            p.terminate()
    for p in reversed(processes):
        try:
            p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait(timeout=5)


def watch(processes, logs, phases, seconds, addresses=None):
    end = time.monotonic()+seconds
    while time.monotonic() < end:
        for p, log in zip(processes, logs):
            require(p.poll() is None, 'startup exited: '+str(log))
        time.sleep(.1)
    for i, (log, phase) in enumerate(zip(logs, phases)):
        if addresses:
            host, p = addresses[i].rsplit(':', 1)
            with socket.create_connection((host, int(p)), timeout=1):
                pass
            continue
        records = []
        for line in log.read_text(errors='replace').splitlines():
            try:
                records.append(json.loads(line))
            except ValueError:
                pass
        require(any(r.get('phase') == phase for r in records),
                'missing '+phase+' in '+str(log))


def smoke(root, seconds=10):
    require(seconds >= 10, 'start observation must be at least 10 seconds')
    pins = json.loads((root/'pins.json').read_text())
    for name in ('product', 'runner'):
        require(re.fullmatch('[0-9a-f]{40}', pins[name]), 'full pin required')
        head = subprocess.check_output(['git', '-C', str(root/name), 'rev-parse', 'HEAD'], text=True).strip()
        require(head == pins[name], 'source pin mismatch: '+name)
    scenario_path = 'scenarios/v3block/r4-node-loss.yaml'
    pinned = subprocess.check_output(['git', '-C', str(root/'runner'), 'show', pins['runner']+':'+scenario_path])
    require((root/'runner'/scenario_path).read_bytes() == pinned, 'scenario differs from runner pin')
    scenario = yaml.safe_load(pinned)
    actions = next(p['actions'] for p in scenario['phases'] if p['name'] == 'start')
    out = Path(tempfile.mkdtemp(prefix='start-preflight-', dir=root))
    out.chmod(0o700)  # Contains temporary certificate keys; never publish wholesale.
    src = out/'src'
    src.mkdir()
    with (out/'src.tar').open('wb') as f:
        subprocess.run(['git', '-C', str(root/'product'), 'archive', pins['product']], stdout=f, check=True)
    subprocess.run(['tar', '-xf', str(out/'src.tar'), '-C', str(src)], check=True)
    binaries = out/'bin'
    binaries.mkdir()
    env = dict(os.environ, CGO_ENABLED='0', GOTOOLCHAIN='local')
    for binary in ('blockmaster', 'blockvolume'):
        subprocess.run(['go', 'build', '-trimpath', '-buildvcs=false', '-o', str(binaries/binary), './cmd/'+binary], cwd=src, env=env, check=True)
    # External-NVMe opt-in rejects loopback. Preserve that exact flag and use
    # an existing controller interface only for its temporary NVMe listener.
    bind = subprocess.check_output(['hostname', '-I'], text=True).split()[0]
    require(not bind.startswith('127.') and ':' not in bind, 'controller IPv4 interface required')
    receipts = dict(pins=pins, seconds=seconds, binary_sha256={b: hashlib.sha256((binaries/b).read_bytes()).hexdigest() for b in ('blockmaster','blockvolume')}, rounds=[])
    for rf in ('3', '2'):
        base = out/('rf'+rf)
        base.mkdir()
        tls = base/'tls'
        subprocess.run(['bash', str(src/'scripts/k8s-alpha-certs.sh'), 'standalone', str(tls), 's1','s2','s3'], env=dict(env, SW_BLOCK_TLS_APPLY='0'), stdout=subprocess.DEVNULL, check=True)
        values = dict(scenario['env'], rf=rf, run_id='local-start-smoke', n1_data='127.0.0.1', n2_data='127.0.0.2', n3_data='127.0.0.3', master_port=port('127.0.0.1'), data_port=port('127.0.0.1'), ctrl_port=port('127.0.0.1'), status_port=port('127.0.0.1'), nvme_port=port(bind))
        # Use the scenario's own spec writer, with controller-local endpoints.
        specwork = base/'spec-root/local-start-smoke'
        specwork.mkdir(parents=True)
        subprocess.run(['bash', str(root/'runner/scenarios/v3block/r4.sh'), 'spec', 'local-start-smoke', rf, values['n1_data'],values['n2_data'],values['n3_data'],values['data_port'],values['ctrl_port']], env=dict(env,R4_ROOT=str(base/'spec-root')), stdout=subprocess.DEVNULL, check=True)
        processes, logs, phases, rendered, handles = [], [], [], [], []
        try:
            for action in actions:
                if ' unit $T ' not in action.get('cmd',''):
                    continue
                if rf == '2' and action['node'] == 'node-1' and '/bin/blockvolume ' in action['cmd']:
                    continue
                work = base/action['node']
                work.mkdir(exist_ok=True)
                (work/'master').mkdir(exist_ok=True)
                role, argv = command(action, values, work)
                argv[0] = str(binaries/('blockmaster' if role == 'master' else 'blockvolume'))
                for i, arg in enumerate(argv):
                    if '/tls/' in arg:
                        argv[i] = str(tls/Path(arg).name)
                if role == 'master':
                    set_arg(argv,'--cluster-spec',str(specwork/'cluster-spec.yaml'))
                    set_arg(argv,'--listen','127.0.0.1:'+values['master_port'])
                else:
                    set_arg(argv,'--master','127.0.0.1:'+values['master_port'])
                    set_arg(argv,'--status-addr','127.0.0.1:'+port('127.0.0.1'))
                    set_arg(argv,'--nvme-listen',bind+':'+port(bind))
                log = base/(action['node']+'-'+role+'.log')
                f = log.open('wb'); handles.append(f)
                p = subprocess.Popen(argv, stdout=f, stderr=subprocess.STDOUT, env=env)
                processes.append(p)
                logs.append(log); phases.append('listening' if role == 'master' else 'status-listening')
                rendered.append(argv)
                # The product enforces one process per volume per host. Smoke
                # replicas sequentially without bypassing that production lock.
                if role == 'master':
                    watch([p], [log], ['listening'], seconds, ['127.0.0.1:'+values['master_port']])
                else:
                    watch([processes[0], p], [logs[0], log], ['listening', 'status-listening'], seconds,
                          ['127.0.0.1:'+values['master_port'], argv[argv.index('--status-addr')+1]])
                    stop([p])
            receipts['rounds'].append(dict(rf=rf, argv=rendered, phases=phases, result='PASS'))
        finally:
            stop(processes)
            for f in handles:
                f.close()
            (out/'receipt.json').write_text(json.dumps(receipts, indent=2)+'\n')
    print('LOCAL START PREFLIGHT PASS '+str(out/'receipt.json'))
    return out


def main(args=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('assets', type=Path)
    parser.add_argument('--launch-token', help='after local smoke passes, invoke cloud run with this authorization')
    args = parser.parse_args(args)
    require(__debug__, 'R4 start preflight refuses Python optimization')
    root = args.assets.resolve()
    smoke(root)
    if args.launch_token:
        require(re.fullmatch('[0-9a-f]{24}', args.launch_token), 'invalid launch token')
        return subprocess.call([str(root/'seaweed-cloud'),'run','--config',str(root/'config.json'),'--pack','exec','--launch-token',args.launch_token,'--json'])
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
