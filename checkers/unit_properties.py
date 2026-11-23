"""Read one unit property set; failed or incomplete producers remain UNKNOWN."""
import subprocess

PROPS = ('LoadState', 'ActiveState', 'SubState', 'Result', 'ExecMainStatus', 'InvocationID')


def show_unit_properties(unit, timeout, systemctl="systemctl"):
    cmd = [systemctl, 'show', '--no-pager'] + sum((['-p', p] for p in PROPS), []) + ['--', unit]
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise ValueError('systemctl show timed out')
    if r.returncode != 0:
        raise ValueError('systemctl show exit %d: %s' % (r.returncode, (r.stderr or r.stdout).strip()[:120]))
    d = {}
    for line in r.stdout.splitlines():
        k, sep, v = line.partition('=')
        if sep:
            d[k] = v
    need = [p for p in PROPS if p != 'ExecMainStatus' or unit.endswith('.service')]   # scopes have no main process
    d.setdefault('ExecMainStatus', '-')
    if any(p not in d for p in need) or not d['LoadState'] or not d['ActiveState']:
        raise ValueError('systemctl show output not parsable: %r' % r.stdout[:120])
    return d
