"""M02 resource sampler for the KV soak (step 3a). A finite owned task: it
samples the identity-bound master/filer/volume generations (original and
replacement) every 30 s for a fixed horizon, writes one bounded series file
in the private run root, takes a final sample and exits 0."""
import argparse
import hashlib
import json
import os
import stat
import time
from pathlib import Path

from soak_stats import process_sample

INTERVAL_S = 30
MAX_ROWS = 200  # 3600 s / 30 s + final, with margin


def generations(root):
    """Every recorded service generation: (role, file, pid, starttime)."""
    out = []
    for role in ('master', 'filer', 'volume'):
        for name in (role + '-generation.json', role + '-replacement-generation.json'):
            try:
                fd = os.open(root / name, os.O_RDONLY | os.O_NOFOLLOW)
            except FileNotFoundError:
                continue
            with os.fdopen(fd) as stream:
                g = json.load(stream)
            out.append((role, name, int(g['pid']), int(g['starttime'])))
    return out


def sample(root):
    return [dict(role=role, generation=name, **process_sample(pid, start))
            for role, name, pid, start in generations(root)]


def run(root, horizon_s):
    info = root.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise ValueError('sampler root must be a private directory')
    if not 0 < horizon_s <= (MAX_ROWS - 2) * INTERVAL_S:
        raise ValueError('sampler horizon out of range')
    path = root / 'soak-series-m02.jsonl'
    start, rows = time.monotonic(), 0
    with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), 'w') as out:
        tick = 0
        while tick * INTERVAL_S <= horizon_s:
            time.sleep(max(0, start + tick * INTERVAL_S - time.monotonic()))
            out.write(json.dumps(dict(mono=time.monotonic(), tick=tick, processes=sample(root))) + '\n')
            rows, tick = rows + 1, tick + 1
        out.write(json.dumps(dict(mono=time.monotonic(), tick=tick, final=True, processes=sample(root))) + '\n')
        rows += 1
    data = path.read_bytes()
    return dict(status='SAMPLED', file=path.name, rows=rows, bytes=len(data),
                sha256=hashlib.sha256(data).hexdigest(), start_mono=start, end_mono=time.monotonic())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', required=True)
    config = json.loads(parser.parse_args().config)
    if set(config) != {'root', 'horizon_s'}:
        raise ValueError('sampler config keys must be exactly root,horizon_s')
    print(json.dumps(run(Path(config['root']), config['horizon_s']), sort_keys=True), flush=True)


if __name__ == '__main__':
    main()
