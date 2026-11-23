"""Render direct exec calls to the pack's Python checkers; no verdict logic.

Checker inputs are on the named POSIX node; receipt_path is runner-local.
The caller supplies boundaries
already produced by the pack. This module neither reads evidence nor counts
reps. Host-side ledger metrics remain inside the pack's existing phase checks.
"""
import pathlib
import re
import shlex


def command(c, name, script, *args):
    if not re.fullmatch(r'[a-zA-Z0-9_-]+', name):
        raise ValueError('command receipt name must be a single safe component')
    argv = [c['python'], '-I', str(pathlib.PurePosixPath(c['packet']) / script),
            *map(str, args)]
    return {'action': 'exec', 'node': c['node'],
            'cmd': 'exec ' + shlex.join(argv),
            'receipt_path': str(pathlib.Path(c['runner_receipt_dir']) / (name + '.json'))}


def grant(c, grant_path, range_path, request):
    return command(c, 'grant', 'lsncheck.py', 'grant', grant_path, range_path,
                   '--request', request)


def image(c, name, ledger, image_path, lsn, blocks, seed, volume, raced, bs=4096):
    return command(c, name, 'lsncheck.py', 'image', ledger, image_path, lsn,
                   '--blocks', blocks, '--seed', seed, '--volume', volume,
                   '--bs', bs, '--raced', raced)


def stopbound(c, ledger, journal):
    return command(c, 'stopbound', 'lsncheck.py', 'stopbound', ledger, journal)


def summary(c, bundle, state, ready, m01_status, m02_status, run_id):
    return command(c, 'summary', 'summarize-go2.py', bundle, state, ready,
                   m01_status, m02_status, run_id)


def gate(c):
    return command(c, 'gate', 'build/gate.py', 'check',
                   str(pathlib.PurePosixPath(c['packet']) / 'GATE.json'))


def evidence_hashes(c, paths):
    """Hash node-local evidence only; runner receipts need a local hash pass."""
    if not paths:
        raise ValueError('explicit evidence files required')
    return {'action': 'exec', 'node': c['node'],
            'cmd': 'exec ' + shlex.join(['sha256sum', '--', *paths]),
            'receipt_path': str(pathlib.Path(c['runner_receipt_dir']) / 'evidence-hashes.json')}
