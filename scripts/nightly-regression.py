#!/usr/bin/env python3
"""Credential-free tp01 nightly; workstation stages inputs and publishes results."""
import datetime
import hashlib
import json
from pathlib import Path
import socket
import sys

CELLS = ['ec-go', 'ec-rust', 'ec-2a', 'master-writes', 'volume-restart',
         'c1', 'c2', 'c4', 'c5', 'ec-reconstruction-go', 'ec-reconstruction-rust']
CASES = dict(zip(CELLS[4:9], ['volume-restart', 'C1-single-kill', 'C2-rolling', 'C4-meta-restart', 'C5-two-kill']))


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + '\n')


def prepare(base, template, cell, sha):
    inv = json.loads(template.read_bytes())
    run = 'reg-' + sha[:12] + '-' + cell
    inv.update(run_id=run, target='onprem', transport='http', managed_resource_ids=[])
    inv['artifact_manifest_sha256'] = hashlib.sha256((base / 'artifacts/weed').read_bytes()).hexdigest()
    h = inv['hosts'][0]
    h['management'].update(address='127.0.0.1', port=22, user='testdev', host_key='')
    h['data'].update(address=socket.gethostbyname(socket.gethostname()), nic='eth0', mtu=1500)
    h.update(run_root='/home/testdev/sw-runs/' + run, disks=[dict(id='overlay', device_id='', mount='/', format_allowed=False)])
    now = datetime.datetime.now(datetime.timezone.utc)
    canon = lambda minutes: (now + datetime.timedelta(minutes=minutes)).strftime('%Y-%m-%dT%H:%M:%SZ')
    inv['deadlines'] = dict(work=canon(25), hard=canon(27), cleanup_reserve_ms=60000)
    cfg = dict(ssh_key=str(base / 'key'), host_key=(base / 'host.pub').read_text().strip(), files={})
    names = ['weed'] if cell.startswith('ec-') else ['weed', 'weed-volume', 'native.tar']
    if cell.startswith('ec-'):
        cfg.update(ec_stage1=True)
        if cell.endswith('rust'):
            cfg.update(ec_volume_impl='rust')
            names.append('weed-volume')
        if cell == 'ec-2a':
            cfg.update(ec_under_quorum=True)
        if cell.startswith('ec-reconstruction-'):
            cfg.update(ec_reconstruction=True)
    elif cell == 'master-writes':
        cfg.update(master_writes=True)
    else:
        cfg.update(replication='001', cluster_chaos_cases=[CASES[cell]])
    for name in names:
        path = base / 'artifacts' / name
        cfg['files'][name] = dict(path=str(path), sha256=hashlib.sha256(path.read_bytes()).hexdigest())
    write(base / 'inventory.json', inv)
    write(base / 'config.json', cfg)


if __name__ == '__main__':
    if sys.argv[1] == 'prepare':
        prepare(Path(sys.argv[2]), Path(sys.argv[3]), sys.argv[4], sys.argv[5])
