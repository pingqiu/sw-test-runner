#!/usr/bin/env python3
"""Transport approved cloud inventory to the committed R4 scenario; no separate judge."""
import hashlib, json, os, pathlib, re, subprocess, sys, time


def require(condition, message):
    if not condition:
        raise ValueError(message)


def completed_imds404_only(status):
    """Exit 2 is allowed only for the captured recoverable IMDS 404 warning."""
    if not isinstance(status, dict) or status.get('status') != 'done' or status.get('errors') != []:
        return False
    if not isinstance(status.get('recoverable_errors'), dict):
        return False
    warning = re.compile(
        r"Polling IMDS failed attempt [1-9][0-9]* with exception: UrlError\('404 Client Error: "
        r"Not Found for url: http://169\.254\.169\.254/metadata/reprovisiondata\?api-version=2019-06-01'\)")
    found = False

    def check(value):
        nonlocal found
        if isinstance(value, dict):
            for key, item in value.items():
                if key == 'errors' and item != []:
                    return False
                if key == 'recoverable_errors':
                    if not isinstance(item, dict):
                        return False
                    for entries in item.values():
                        if not isinstance(entries, list):
                            return False
                        for entry in entries:
                            if not isinstance(entry, str) or not warning.fullmatch(entry):
                                return False
                            found = True
                elif not check(item):
                    return False
        elif isinstance(value, list):
            return all(check(item) for item in value)
        return True

    return check(status) and found


def main(root):
    if not __debug__:
        raise RuntimeError("R4 launcher refuses Python optimization")
    root = pathlib.Path(root).resolve()
    manifest = json.loads((root/'manifest.json').read_text())
    require(isinstance(manifest, dict) and {'pins.json', 'swblock'} <= manifest.keys(),
            "manifest must include pins.json and swblock")
    for name, sha in manifest.items():
        require(hashlib.sha256((root/name).read_bytes()).hexdigest() == sha,
                f"manifest digest mismatch: {name}")
    pins = json.loads((root/'pins.json').read_text())
    for name in ('product', 'runner'):
        require(subprocess.check_output(['git','-C',str(root/name),'rev-parse','HEAD'],text=True).strip() == pins[name],
                f"source commit mismatch: {name}")
    ctx = json.loads(pathlib.Path(os.environ['SEAWEED_CLOUD_PACK_CONTEXT']).read_text())
    inv = json.loads(pathlib.Path(ctx['inventory']).read_text())
    require(ctx['schema'] == 'seaweed.cloud-pack-context/v1', "invalid cloud pack context schema")
    require(inv['target'] == 'azure', "inventory target must be azure")
    hosts = {h['id']: h for h in inv['hosts']}
    require(set(hosts) == {'node-1', 'node-2', 'node-3'}, "inventory must name node-1..node-3")
    require(set(ctx['host_keys']) == set(hosts), "host keys must cover exactly the inventory nodes")
    out = pathlib.Path(ctx['output'])
    env = dict(os.environ, GOTOOLCHAIN='local', CGO_ENABLED='0')
    args = []
    users = {h['management']['user'] for h in hosts.values()}
    require(len(users) == 1, "inventory nodes must use the same SSH user")
    user = next(iter(users))
    require(re.fullmatch('[a-z_][a-z0-9_-]*', user), "invalid inventory SSH user")
    for i in range(1, 4):
        h = hosts[f'node-{i}']; m = h['management']
        require(h['resource_id'] in inv['managed_resource_ids'], "host is not a managed resource")
        require(m['user'] == user and m.get('port', 22) == 22, "invalid SSH user or port")
        pin = ctx['host_keys'][h['id']].strip()
        require(pin.startswith('ssh-ed25519 ') and '\n' not in pin, "invalid ed25519 host key")
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
            if r.returncode == 2:
                detail = subprocess.run(ssh+['sudo cloud-init status --long --format json'],
                                        capture_output=True, timeout=30)
                (out/f'cloud-init-{i}-long.log').write_bytes(detail.stdout+detail.stderr)
                (out/f'cloud-init-{i}-long.exit').write_text(str(detail.returncode)+'\n')
                try:
                    status = json.loads(detail.stdout)
                except (ValueError, UnicodeError):
                    status = None
                if detail.returncode in (0, 2) and completed_imds404_only(status):
                    break
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
