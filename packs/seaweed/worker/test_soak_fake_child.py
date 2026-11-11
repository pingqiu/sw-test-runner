"""Test-only stand-in for soak_native_worker.child (no native connector).
GETs answer the exact payload digest from the command's seed; while the file
`fake-fail` exists in the run root, a completing GET fails with a timeout."""
import hashlib
import json
import os
import time
from pathlib import Path

from soak_native_worker import payload


def child(pipe, cfg):
    root = Path(cfg['root'])
    if 'late_completions' in cfg:  # r12: the worker flag, recorded for the load test that it reaches F3 only
        (root / ('fake-late-flag-%d' % os.getpid())).write_text(json.dumps(cfg['late_completions']))
    pipe.send(dict(event='ready', pid=os.getpid(), stats={}, backing='fake'))
    abandoned = []  # r12 F3: ops this process abandoned at a (fake) deadline, as soak_native_worker.finish keeps them
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
        if cmd['action'] == 'set' and (root / 'fake-deadline-set').exists():  # the real child's deadline error event
            time.sleep(0.05)
            if cfg.get('late_completions') is True:
                abandoned.append(cmd['operation_id'])
            pipe.send(dict(event='error', operation_id=cmd['operation_id'], action='set',
                           error="TimeoutError('native completion deadline')", epoch=time.time()))
            continue
        if cmd['action'] == 'set' and (root / 'fake-late-bogus').exists():  # a late row for an op that never timed out
            pipe.send(dict(event='late', operation_id=cmd['operation_id'], fid=8, ok=True, error='', results=None,
                           mono=time.monotonic()))
        while abandoned:  # its native completion arrives during this process's next op: ok=true, too late
            pipe.send(dict(event='late', operation_id=abandoned.pop(0), fid=7, ok=True, error='', results=None,
                           mono=time.monotonic()))
        time.sleep(cfg.get('fake_delay_' + cmd['action'], 0.01))  # long GETs: always some pending at a pause
        while (root / 'fake-frozen').exists():  # stage 3 F1: a SIGSTOPped volume answers nothing until it is killed
            time.sleep(0.01)
        row = dict(operation_id=cmd['operation_id'], action=cmd['action'], key=cmd['key'], submitted=submitted,
                   completed=time.monotonic(), ok=True, error=None, results=None)
        if cmd['action'] == 'set' and (root / 'fake-fail-set').exists():  # stage 3: a failed PUT; the file holds its error
            row.update(ok=False, error=(root / 'fake-fail-set').read_text() or 'connection reset', results=[False])
        elif cmd['action'] == 'set':
            row['fids'] = ['3,0a1b2c']
        elif (root / 'fake-refused').exists():  # a K8 budget-refusal miss: the worker's counter delta names it
            row.update(results=[False], error='', cause='budget_refusal')
        elif (root / 'fake-notfound').exists():  # r9's miss-candidate shape: ok, the key not returned, no error text
            row.update(results=[False], error='')
        elif (root / 'fake-status1').exists():  # a native NOT_FOUND-shaped failure; the file holds its error text
            row.update(ok=False, error=(root / 'fake-status1').read_text(), results=[False], untouched=True)
        elif (root / 'fake-fail-empty').exists():  # a failed GET with an EMPTY error (the 11-02 r2 stop)
            row.update(ok=False, error='', results=[False], untouched=False)
        elif (root / 'fake-fail').exists():  # its content, if any, is the error text
            row.update(ok=False, error=(root / 'fake-fail').read_text() or 'operation timed out')
        else:
            row.update(length=cmd['length'], sha256=hashlib.sha256(payload(cmd['seed'], cmd['length'])).hexdigest(),
                       untouched=False)
        while (root / 'fake-hold-complete').exists():  # stage 3 F3: a completion recorded now, delivered late
            time.sleep(0.01)
        pipe.send(dict(event='complete', row=row))
