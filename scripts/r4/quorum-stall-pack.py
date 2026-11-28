#!/usr/bin/env python3
"""Pinned v1.0.1 RF3/RF2 rows; use the existing inventory and cleanup controller."""
import hashlib,importlib.util,json,subprocess,sys
from pathlib import Path

def main(root):
    if not __debug__:raise ValueError('gate refuses optimized Python')
    root=Path(root).resolve();manifest=json.loads((root/'manifest.json').read_text())
    required=('pins.json','swblock','cloud-pack.py','quorum-stall.py','quorum-stall-policy.json')
    for name in required:
        if name not in manifest or hashlib.sha256((root/name).read_bytes()).hexdigest()!=manifest[name]:
            raise ValueError('missing or changed manifest input: '+name)
    policy=json.loads((root/'quorum-stall-policy.json').read_text())
    if set(policy)!={'write_budget_ms','resume_budget_ms','budget_ref'}:raise ValueError('exact owner policy required')
    subprocess.run([sys.executable,str(root/'quorum-stall.py'),'policy','--rf','3',
        '--write-budget-ms',str(policy['write_budget_ms']),'--resume-budget-ms',str(policy['resume_budget_ms']),
        '--budget-ref',policy['budget_ref']],check=True)
    spec=importlib.util.spec_from_file_location('r4_cloud_pack',root/'cloud-pack.py')
    pack=importlib.util.module_from_spec(spec);spec.loader.exec_module(pack)
    return pack.main(root,scenario='scenarios/v3block/quorum-stall.yaml',extra_env=policy)

if __name__=='__main__':
    try:sys.exit(main(sys.argv[1]))
    except (ValueError,KeyError,FileNotFoundError,subprocess.CalledProcessError) as error:
        print(str(error),file=sys.stderr);sys.exit(1)
