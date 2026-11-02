"""Test-only stand-in for soak_native_worker.child (no native connector).
GETs answer the exact payload digest from the command's seed; while the file
`fake-fail` exists in the run root, a completing GET fails with a timeout."""
import hashlib
import os
import time
from pathlib import Path

from soak_native_worker import payload


def child(pipe, cfg):
    root = Path(cfg['root'])
    pipe.send(dict(event='ready', pid=os.getpid(), stats={}, backing='fake'))
    while True:
        cmd = pipe.recv()
        if cmd['action'] == 'stop':
            return
        if cmd['action'] == 'stats':
            pipe.send(dict(event='stats', stats={}))
            continue
        submitted = time.monotonic()
        pipe.send(dict(event='submitted', operation_id=cmd['operation_id'], action=cmd['action'],
                       pid=os.getpid(), monotonic=submitted))
        time.sleep(cfg.get('fake_delay_' + cmd['action'], 0.01))  # long GETs: always some pending at a pause
        row = dict(operation_id=cmd['operation_id'], action=cmd['action'], key=cmd['key'], submitted=submitted,
                   completed=time.monotonic(), ok=True, error=None, results=None)
        if cmd['action'] == 'set':
            row['fids'] = ['3,0a1b2c']
        elif (root / 'fake-fail').exists():  # its content, if any, is the error text
            row.update(ok=False, error=(root / 'fake-fail').read_text() or 'operation timed out')
        else:
            row.update(length=cmd['length'], sha256=hashlib.sha256(payload(cmd['seed'], cmd['length'])).hexdigest(),
                       untouched=False)
        pipe.send(dict(event='complete', row=row))
