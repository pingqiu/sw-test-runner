"""Test-only stand-in for soak_native_worker.child (no native connector).
GETs answer the exact payload digest from the command's seed; while the file
`fake-fail` exists in the run root, a completing GET fails with a timeout."""
import hashlib
import json
import os
import time
from pathlib import Path

from soak_native_worker import payload


def audit_child(root):
    """r26: the fresh audit client is started after the load logged body_end; every load worker before it."""
    try:
        return '"body_end"' in (root / 'op-events.jsonl').read_text()
    except OSError:
        return False


def stop(pipe, root, own, abandoned, poisoned):
    """r26 F5 ownership: the stop-time variants the reap must survive, each from a flag file in the run root:
    fake-stop-ignore (never answers, never exits), fake-final-delay (its content: seconds before the final row),
    fake-final-hang (final row, then a destructor that never returns). Otherwise: the final row, then exit."""
    if not own:
        return
    if (root / 'fake-stop-ignore').exists():
        while True:
            time.sleep(1)
    if (root / 'fake-final-delay').exists():
        time.sleep(float((root / 'fake-final-delay').read_text() or 6))
    pipe.send(dict(event='final', pid=os.getpid(), poisoned=poisoned, abandoned_outstanding=list(abandoned),
                   unsettled_buffers=len(abandoned) + int(poisoned), stats={}, mono=time.monotonic()))
    if (root / 'fake-final-hang').exists():
        while True:
            time.sleep(1)


def child(pipe, cfg):
    root = Path(cfg['root'])
    if 'late_completions' in cfg:  # r12: the worker flag, recorded for the load test that it reaches F3 only
        (root / ('fake-late-flag-%d' % os.getpid())).write_text(json.dumps(cfg['late_completions']))
    own = cfg.get('ownership') is True  # r26 F5
    if own:
        (root / ('fake-own-flag-%d' % os.getpid())).write_text('true')
        if audit_child(root) and (root / 'fake-audit-hang').exists():  # r26: an audit client that is never ready
            while True:
                time.sleep(1)
    pipe.send(dict(event='ready', pid=os.getpid(), stats={}, backing='fake'))
    abandoned = []  # r12 F3: ops this process abandoned at a (fake) deadline, as soak_native_worker.finish keeps them
    poisoned = False
    while True:
        cmd = pipe.recv()
        if cmd['action'] == 'stop':
            return stop(pipe, root, own, abandoned, poisoned)
        if cmd['action'] == 'stats':
            pipe.send(dict(event='stats', stats={}))
            continue
        if own and (poisoned or abandoned):  # the reuse barrier: the parent must never send here
            pipe.send(dict(event='refused', operation_id=cmd['operation_id'], reason='poisoned' if poisoned else 'blocked'))
            continue
        if own and cmd['action'] == 'set' and (root / 'fake-submit-raise').exists():  # r26: no fid, no submitted row
            poisoned = True
            pipe.send(dict(event='error', operation_id=cmd['operation_id'], action='set', phase='submit',
                           error="RuntimeError('submit failed after enqueue')", epoch=time.time()))
            if (root / 'fake-orphan').exists():  # the hidden op completes anyway: evidence only on a poisoned connector
                pipe.send(dict(event='orphan', fid=99, ok=False, error='late hidden op', results=None, mono=time.monotonic()))
            continue
        submitted = time.monotonic()
        pipe.send(dict(event='submitted', operation_id=cmd['operation_id'], action=cmd['action'],
                       pid=os.getpid(), monotonic=submitted))
        if own and cmd['action'] == 'get' and audit_child(root) and (root / 'fake-audit-get-hang').exists():
            while True:  # r26: an audit GET that never answers (and a child that never reads its stop)
                time.sleep(1)
        if cmd['action'] == 'set' and (root / 'fake-deadline-set').exists():  # the real child's deadline error event
            time.sleep(0.05)
            if cfg.get('late_completions') is True:
                abandoned.append(cmd['operation_id'])
            pipe.send(dict(event='error', operation_id=cmd['operation_id'], action='set',
                           error="TimeoutError('native completion deadline')", epoch=time.time(),
                           **(dict(phase='wait') if own else {})))
            if own and not (root / 'fake-keep-abandoned').exists():
                # r26: the blocked worker idle-drains (and, as the real one, refuses any op sent to it meanwhile): the
                # abandoned future completes late, then the worker unblocks
                end = time.monotonic() + 0.2
                while time.monotonic() < end:
                    if pipe.poll(0.01):
                        c = pipe.recv()
                        pipe.send(dict(event='refused', operation_id=c.get('operation_id'), reason='blocked'))
                while abandoned:
                    pipe.send(dict(event='late', operation_id=abandoned.pop(0), fid=7, ok=False, error='late failure',
                                   results=None, mono=time.monotonic()))
                pipe.send(dict(event='unblocked', pid=os.getpid(), mono=time.monotonic()))
            continue
        if cmd['action'] == 'set' and (root / 'fake-late-bogus').exists():  # a late row for an op that never timed out
            pipe.send(dict(event='late', operation_id=cmd['operation_id'], fid=8, ok=True, error='', results=None,
                           mono=time.monotonic()))
        while abandoned and not own:  # its native completion arrives during this process's next op: ok=true, too late
            pipe.send(dict(event='late', operation_id=abandoned.pop(0), fid=7, ok=True, error='', results=None,
                           mono=time.monotonic()))
        time.sleep(cfg.get('fake_delay_' + cmd['action'], 0.01))  # long GETs: always some pending at a pause
        while (root / 'fake-frozen').exists():  # stage 3 F1: a SIGSTOPped volume answers nothing until it is killed
            time.sleep(0.01)
        row = dict(operation_id=cmd['operation_id'], action=cmd['action'], key=cmd['key'], submitted=submitted,
                   completed=time.monotonic(), ok=True, error=None, results=None)
        if own:
            row.update(pid=os.getpid(), failed_delta=0)  # r26: the strict per-op connector_failed_ops delta
        if cmd['action'] == 'set' and (root / 'fake-fail-set').exists():  # stage 3: a failed PUT; the file holds its error
            row.update(ok=False, error=(root / 'fake-fail-set').read_text() or 'connection reset', results=[False])
            if own:
                row['failed_delta'] = 1
        elif cmd['action'] == 'set':
            row['fids'] = ['3,0a1b2c']
        elif (root / 'fake-refused').exists():  # a K8 budget-refusal miss: the worker's counter delta names it
            row.update(results=[False], error='', cause='budget_refusal')
        elif (root / 'fake-unavailable').exists():  # #1187: a miss the connector counted volume-unavailable (the worker's flag)
            row.update(results=[False], error='', unavailable=True)
        elif (root / 'fake-notfound').exists():  # r9's miss-candidate shape: ok, the key not returned, no error text
            row.update(results=[False], error='')
        elif (root / 'fake-status1').exists():  # a native NOT_FOUND-shaped failure; the file holds its error text
            row.update(ok=False, error=(root / 'fake-status1').read_text(), results=[False], untouched=True)
        elif (root / 'fake-fail-empty').exists():  # a failed GET with an EMPTY error (the 11-02 r2 stop)
            row.update(ok=False, error='', results=[False], untouched=False)
        elif (root / 'fake-fail').exists():  # its content, if any, is the error text
            row.update(ok=False, error=(root / 'fake-fail').read_text() or 'operation timed out')
            if own:  # r26: a settled failed GET under pressure: counted +1, its buffer untouched
                row.update(failed_delta=1, untouched=True, results=[False])
        else:
            row.update(length=cmd['length'], sha256=hashlib.sha256(payload(cmd['seed'], cmd['length'])).hexdigest(),
                       untouched=False)
        while (root / 'fake-hold-complete').exists():  # stage 3 F3: a completion recorded now, delivered late
            time.sleep(0.01)
        pipe.send(dict(event='complete', row=row))
