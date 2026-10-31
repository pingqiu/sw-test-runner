"""Explicit local test: real module mock outputs -> raw export -> accepted Go CLI.
No import-time effects. Mock provider only; no account auth/API or real apply.
Synthetic observations below qualify serialization/Join, never real host facts.
"""
import argparse, copy, gzip, hashlib, json, os, pathlib, subprocess, tempfile


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--terraform', required=True)
    p.add_argument('--inventory-cli', required=True)
    p.add_argument('--module', type=pathlib.Path, required=True)
    p.add_argument('--evidence', type=pathlib.Path, required=True)
    a = p.parse_args()
    env = {k:v for k,v in os.environ.items() if not k.startswith(('ARM_', 'AZURE_', 'TF_CLI_ARGS', 'TF_LOG', 'TF_VAR_'))}
    env.pop('TF_DATA_DIR', None)
    env.update(CHECKPOINT_DISABLE='1', TF_IN_AUTOMATION='1', ARM_USE_CLI='false', ARM_USE_MSI='false')
    a.evidence.mkdir(parents=True, exist_ok=True)
    tf, cli = str(pathlib.Path(a.terraform).resolve()), str(pathlib.Path(a.inventory_cli).resolve())
    module = a.module.resolve()
    # OS-native filter spelling matters (Windows uses backslashes). Refuse a
    # zero-test success; only this committed mock-provider test file is selected.
    testfile = str(pathlib.Path('tests')/'inventory.tftest.hcl')
    source = (module/testfile).read_text(encoding='utf-8-sig')
    if 'mock_provider "azurerm"' not in source or '\nprovider "' in source:
        raise AssertionError('mock-only test file required')
    result = subprocess.run([tf, '-chdir='+str(module), 'test', '-filter='+testfile, '-json', '-verbose'], env=env, capture_output=True, timeout=120)
    (a.evidence/'terraform-test.jsonl.gz').write_bytes(gzip.compress(result.stdout,mtime=0))
    (a.evidence/'terraform-test.stderr').write_bytes(result.stderr)
    rows = [json.loads(row) for row in result.stdout.splitlines() if row]
    summary = [row['test_summary'] for row in rows if row.get('type') == 'test_summary']
    if result.returncode or len(summary)!=1 or summary[0]['status']!='pass' or summary[0]['passed']!=18:
        raise AssertionError('expected all 18 mock-only runs, never zero-test success')
    states = {row['@testrun']:row['test_state']['outputs']['inventory_json']['value'] for row in rows if row.get('type')=='test_state'}
    if set(states)!={'inventory_output','client_only'}:
        raise AssertionError('both actual module outputs required')
    checks = 0
    with tempfile.TemporaryDirectory(prefix='azure-output-', dir=a.evidence) as folder:
        root = pathlib.Path(folder)
        def invoke(args, expected, code):
            nonlocal checks
            r = subprocess.run([cli,'--json']+args, capture_output=True, timeout=10, env=env)
            body = json.loads(r.stdout)
            if r.returncode!=expected or body['code']!=code or body['execution_authorized'] is not False:
                raise AssertionError(str((args,r.returncode,body)))
            checks += 1
        for name, value in states.items():
            raw = value.encode()
            # Real Terraform output -raw over the ACTUAL module-generated value,
            # in resource-free local state (no provider/backend/configuration).
            state = dict(version=4, terraform_version='1.17.1', serial=1, lineage='11111111-1111-1111-1111-111111111111',
                         outputs={'inventory_json':dict(value=value,type='string',sensitive=False)}, resources=[])
            (root/'terraform.tfstate').write_text(json.dumps(state))
            r = subprocess.run([tf,'-chdir='+str(root),'output','-raw','inventory_json'],capture_output=True,check=True,timeout=10,env=env)
            if r.stdout!=raw: raise AssertionError('raw export changed module bytes')
            checks += 1
            req, obs = root/'requested.json', root/'resolved.json'
            req.write_bytes(raw)
            (a.evidence/(name+'-requested.json')).write_bytes(raw)
            invoke(['validate',str(req)],0,'OK')
            requested = json.loads(raw)
            resolved = copy.deepcopy(requested)
            resolved['state']='resolved'
            resolved['observation']=dict(requested_sha256=hashlib.sha256(raw).hexdigest(),hosts=[])
            for host in resolved['hosts']:
                host['management']['host_key']='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB'
                host['data'].update(nic='eth0',mtu=1500)
                for disk in host['disks']: disk['device_id']='/dev/disk/by-id/mock-'+host['id']
                resolved['observation']['hosts'].append(dict(host_id=host['id'],boot_id='11111111-1111-1111-1111-111111111111',captured_at='2030-01-01T00:00:00Z',evidence_sha256='c'*64))
            obs.write_text(json.dumps(resolved))
            invoke(['join',str(req),str(obs)],0,'OK')
            for field in ('work','hard','cleanup_reserve_ms','service','missing_services'):
                changed=copy.deepcopy(resolved)
                if field=='service':
                    changed['hosts'][0]['services'].append(dict(name='master_http',port=9999)) if not changed['hosts'][0]['services'] else changed['hosts'][0]['services'][0].update(port=9999)
                elif field=='missing_services': del changed['hosts'][0]['services']
                elif field=='cleanup_reserve_ms': changed['deadlines'][field]+=1
                else: changed['deadlines'][field]='2030-01-01T'+('00:59:00Z' if field=='work' else '02:01:00Z')
                obs.write_text(json.dumps(changed))
                invoke(['join',str(req),str(obs)],2,'INVENTORY_JOIN_REFUSED')
            quoted=subprocess.run([tf,'-chdir='+str(root),'output','-json','inventory_json'],capture_output=True,check=True,timeout=10,env=env)
            req.write_bytes(quoted.stdout)
            invoke(['validate',str(req)],2,'INVENTORY_INVALID')
    print(json.dumps(dict(status='PASS',code='OK',remedy='data tests grant no deployment authority',terraform_mock_runs=18,cli_export_join_checks=checks,account_api=False,execution_authorized=False)))


if __name__=='__main__':
    try: main()
    except Exception as error:
        print(json.dumps(dict(status='FAIL',code='AZURE_INVENTORY_CONTRACT_FAILED',remedy='inspect retained local test output; do not deploy',error=str(error))))
        raise SystemExit(1)
