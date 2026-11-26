#!/usr/bin/env python3
"""Transport approved cloud inventory to the committed R4 scenario; no separate judge."""
import hashlib, json, os, pathlib, re, subprocess, sys, time

def main(root):
    root = pathlib.Path(root).resolve()
    pins = json.loads((root/'pins.json').read_text())
    for name, sha in json.loads((root/'manifest.json').read_text()).items():
        assert hashlib.sha256((root/name).read_bytes()).hexdigest() == sha, name
    for name in ('product', 'runner'):
        assert subprocess.check_output(['git','-C',str(root/name),'rev-parse','HEAD'],text=True).strip() == pins[name]
    ctx = json.loads(pathlib.Path(os.environ['SEAWEED_CLOUD_PACK_CONTEXT']).read_text())
    inv = json.loads(pathlib.Path(ctx['inventory']).read_text())
    assert ctx['schema'] == 'seaweed.cloud-pack-context/v1'
    assert inv['target'] == 'azure'
    hosts = {h['id']: h for h in inv['hosts']}
    assert set(hosts) == {'node-1', 'node-2', 'node-3'}
    assert set(ctx['host_keys']) == set(hosts)
    out = pathlib.Path(ctx['output'])
    env = dict(os.environ, GOTOOLCHAIN='local', CGO_ENABLED='0')
    args = []
    users = {h['management']['user'] for h in hosts.values()}
    assert len(users) == 1
    user = next(iter(users))
    assert re.fullmatch('[a-z_][a-z0-9_-]*', user)
    for i in range(1, 4):
        h = hosts[f'node-{i}']; m = h['management']
        assert h['resource_id'] in inv['managed_resource_ids']
        assert m['user'] == user and m.get('port', 22) == 22
        pin = ctx['host_keys'][h['id']].strip()
        assert pin.startswith('ssh-ed25519 ') and '\n' not in pin
        known = out/f'known_hosts-{i}'
        known.write_text(m['address']+' '+pin+'\n'); known.chmod(0o600)
        ssh = ['ssh', '-i', ctx['ssh_key'], '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes',
               '-o', 'StrictHostKeyChecking=yes', '-o', 'UserKnownHostsFile='+str(known),
               '-o', 'ConnectTimeout=10', m['user']+'@'+m['address']]
        end = time.monotonic()+300
        while True:
            r = subprocess.run(ssh+['sudo timeout 240 cloud-init status --wait'], capture_output=True, timeout=255)
            (out/f'cloud-init-{i}.log').write_bytes(r.stdout+r.stderr)
            if r.returncode == 0: break
            if time.monotonic() >= end: raise RuntimeError(f'{h["id"]} cloud-init readiness failed')
            time.sleep(5)
        for k,v in {f'n{i}_ip':m['address'], f'n{i}_data':h['data']['address'], f'n{i}_host_key':pin}.items():
            args += ['-env', k+'='+v]
    args += ['-env', 'ssh_user='+user, '-env', 'ssh_key='+ctx['ssh_key'],
             '-env', 'product_src='+str(root/'product'), '-env', 'runner_src='+str(root/'runner'),
             '-env', 'product_head='+pins['product'], '-env', 'runner_head='+pins['runner']]
    for rf in (3,2):
        cmd = [str(root/'swblock'), 'run', '--allow-mutating', '-results-dir', str(out/f'rf{rf}'),
               *args, '-env', f'rf={rf}', str(root/'runner/scenarios/v3block/r4-node-loss.yaml')]
        with (out/f'rf{rf}.log').open('wb') as log:
            r = subprocess.run(cmd, env=env, stdout=log, stderr=subprocess.STDOUT)
        (out/f'rf{rf}.exit').write_text(str(r.returncode)+'\n')
        if r.returncode: return r.returncode
    return 0

if __name__ == "__main__":
    sys.exit(main(sys.argv[1]))
