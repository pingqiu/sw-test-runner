"""Read-only local resource inventories for the shared absence decision.

All run bindings remain data in the caller's run.env. The collector performs
no cleanup, SSH, ownership claim or mutation. Only the authenticated loader
binds the deciding function; packet adapters supply location and CLI arguments.
"""
import fnmatch
import os
import subprocess


def main(packet_file, arguments, decide):
    __file__ = packet_file
    argv = [packet_file, *arguments]
    def _runenv(need):
        """Read run.env bindings as data, never execute them.

        RUN_ID must fully match RUN_ID_RE; required keys must be present.
        Invalid data or failed reads raise so the observer returns UNKNOWN.
        """
        import re
        v = {}
        with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), 'run.env'), encoding='utf-8') as f:
            for line in f:
                line = line.rstrip('\n')
                if not line.strip() or line.lstrip().startswith('#'):
                    continue
                k, sep, val = line.partition('=')
                if not sep or not k.isidentifier() or k in v:
                    raise ValueError('run.env: bad or repeated line %r' % line[:80])
                v[k] = val
        if not re.fullmatch(v.get('RUN_ID_RE') or '(?!)', v.get('RUN_ID', '')):
            raise ValueError('run.env: RUN_ID %r does not match RUN_ID_RE %r' % (v.get('RUN_ID'), v.get('RUN_ID_RE')))
        miss = [k for k in need if k not in v]
        if miss:
            raise ValueError('run.env: missing %s' % ','.join(miss))
        return v


    PK = os.path.dirname(os.path.abspath(__file__))
    C = os.path.dirname(PK)
    try:
        E = _runenv(('HOSTS', 'NQN_PREFIX', 'UNIT_GLOB', 'OWNED_ROOTS',
                     'EXTRA_DEVICES', 'CHECK_LOOP', 'CHECK_FIRECRACKER', 'TEST_SEAM_PREFIX'))
        if E['CHECK_LOOP'] not in ('0', '1') or E['CHECK_FIRECRACKER'] not in ('0', '1'):
            raise ValueError('CHECK_LOOP / CHECK_FIRECRACKER must be 0 or 1')
    except Exception as e:
        print('ABSENCE-UNKNOWN:\nrun.env %r' % e)
        return 3
    RUN = E['RUN_ID']   # read from a file: no command line of this process names an owned root
    HOSTS = E['HOSTS'].split()
    if len(argv) not in (2, 4) or argv[1] not in HOSTS or (len(argv) == 4 and argv[2] != '--self-unit'):
        print('use: absence.py %s [--self-unit NAME]' % '|'.join(HOSTS))
        return 2
    HOST = argv[1]
    SELF = argv[3] if len(argv) == 4 else None


    def _ports(spec):
        out = set()
        for t in spec.split():
            a, _, b = t.partition('-')
            out |= set(range(int(a), int(b or a) + 1))
        return out


    try:
        PORTS = _ports(E.get('PORTS_' + HOST, E.get('PORTS', '')))
    except ValueError as e:
        print('ABSENCE-UNKNOWN:\nrun.env PORTS_%s %r' % (HOST, e))
        return 3
    RS = []
    for template in E['OWNED_ROOTS'].split():
        relative = template.replace('{run}', RUN).replace('{host}', HOST).strip('/')
        RS.append(os.path.join(os.path.dirname(C), relative))
    if not RS:
        print('ABSENCE-UNKNOWN:\nrun.env OWNED_ROOTS is empty')
        return 3
    NQNP = E['NQN_PREFIX']
    UNITS = E['UNIT_GLOB'].replace('{run}', RUN).replace('{host}', HOST)
    COMMS = {'firecracker'} if E['CHECK_FIRECRACKER'] == '1' else set()   # CHECK_FIRECRACKER
    CHECK_LOOP = E['CHECK_LOOP'] == '1'


    def under(path):
        return any(path == r or path.startswith(r + '/') for r in RS)

    DEVGLOBS = E['EXTRA_DEVICES'].split()
    ROOT = os.environ.get(E['TEST_SEAM_PREFIX'] + 'ABS_ROOT', '')
    SYSTEMCTL = os.environ.get(E['TEST_SEAM_PREFIX'] + 'ABS_SYSTEMCTL', 'systemctl')
    left, unknown = [], []


    def rd(p):
        with open(ROOT + p, 'rb') as f:
            return f.read().decode(errors='replace')


    def present(p):
        """Observe presence; only ENOENT/ENOTDIR prove absence. Other errors raise."""
        try:
            os.lstat(p)
            return True
        except (FileNotFoundError, NotADirectoryError):
            return False


    def ls(p):
        if not present(ROOT + p):
            return None
        return sorted(os.listdir(ROOT + p))


    # UNIT
    try:
        command = [SYSTEMCTL, 'list-units', '--all', '--plain', '--no-legend', UNITS]
        r = subprocess.run(command, capture_output=True, text=True, timeout=30)
        if r.returncode != 0:
            unknown.append('UNIT systemctl exit %d: %s' % (r.returncode, (r.stdout + r.stderr).strip()[:200]))
        else:
            for l in r.stdout.splitlines():
                if l.strip() and l.split()[0] not in (SELF, (SELF or '') + '.service'):
                    left.append('UNIT ' + l.split()[0])
    except Exception as e:
        unknown.append('UNIT systemctl %r' % e)
    # PORT
    for f in ('/proc/net/tcp', '/proc/net/tcp6'):
        try:
            lines = rd(f).splitlines()
            if not lines or 'local_address' not in lines[0]:
                raise ValueError('no header')
            for l in lines[1:]:
                p = l.split()
                port = int(p[1].rsplit(':', 1)[1], 16)
                if p[3] == '0A' and port in PORTS:
                    left.append('PORT %d (%s)' % (port, f))
        except Exception as e:
            unknown.append('PORT %s %r' % (f, e))
    # MOUNT
    try:
        mps = [l.split()[4] for l in rd('/proc/self/mountinfo').splitlines() if l.strip()]
        if '/' not in mps:
            raise ValueError('no / in mountinfo')
        left += ['MOUNT ' + m for m in mps if under(m)]
    except Exception as e:
        unknown.append('MOUNT %r' % e)
    # NVME
    for cls in ('/sys/class/nvme', '/sys/class/nvme-subsystem'):
        try:
            ents = ls(cls)
        except Exception as e:
            unknown.append('NVME list %s %r' % (cls, e))
            continue
        for d in ents or []:
            a = '%s/%s/subsysnqn' % (cls, d)
            try:
                if not present(ROOT + a):
                    continue
                if NQNP and rd(a).strip().startswith(NQNP):
                    left.append('NVME %s/%s %s' % (cls, d, rd(a).strip()))
            except Exception as e:
                unknown.append('NVME %s %r' % (a, e))
    # LOOP: a loop device whose backing file is at or under an owned root
    try:
        for b in (ls('/sys/block') or []) if CHECK_LOOP else []:
            if not b.startswith('loop'):
                continue
            a = '/sys/block/%s/loop/backing_file' % b
            try:
                if not present(ROOT + a):
                    continue   # not bound to a file
                bf = rd(a).strip()
                if under(bf):
                    left.append('LOOP /dev/%s backing %s' % (b, bf))
            except Exception as e:
                unknown.append('LOOP %s %r' % (a, e))
    except Exception as e:
        unknown.append('LOOP list %r' % e)
    # DEV: every EXTRA_DEVICES entry. A plain path: lstat it (present = leftover; ENOENT/ENOTDIR = absent; any other error,
    # e.g. EACCES, EIO, ELOOP = UNKNOWN). A glob (only in the last component): list its directory (an error = UNKNOWN),
    # then lstat every matching name the same way. glob.glob is not used: it hides errors as "no match".
    for g in DEVGLOBS:
        try:
            dn, bn = os.path.split(g)
            if any(c in dn for c in '*?['):
                raise ValueError('a glob only in the last component')
            if any(c in bn for c in '*?['):
                names = [n for n in sorted(os.listdir(ROOT + dn)) if fnmatch.fnmatchcase(n, bn)]
            else:
                names = [bn]
            for n in names:
                if present(ROOT + os.path.join(dn, n)):
                    left.append('DEV %s' % os.path.join(dn, n))
        except Exception as e:
            unknown.append('DEV %s %r' % (g, e))
    # FC, PROC (excluding this process and its ancestors: the cleanup unit runs the check from inside its own unit)
    skip = set()
    try:
        p = os.getpid()
        while p > 1 and len(skip) < 64:
            skip.add(str(p))
            st = rd('/proc/%d/stat' % p)
            p = int(st.rsplit(')', 1)[1].split()[1])
    except Exception as e:
        unknown.append('PROC ancestry %r' % e)
    try:
        for p in [p for p in os.listdir(ROOT + '/proc') if p.isdigit()]:
            if p in skip:
                continue
            try:
                comm = rd('/proc/%s/comm' % p).strip()
                cmd = rd('/proc/%s/cmdline' % p).replace('\0', ' ').strip()
            except (FileNotFoundError, ProcessLookupError):
                continue
            except Exception as e:
                unknown.append('PROC %s %r' % (p, e))
                continue
            if comm in COMMS:
                left.append('FC %s %s %s' % (p, comm, cmd[:200]))
            elif any(r in cmd for r in RS):
                left.append('PROC %s %s' % (p, cmd[:200]))
    except Exception as e:
        unknown.append('PROC list %r' % e)
    # ROOT
    for r in RS:
        try:
            if present(r):
                left.append('ROOT %s exists' % r)
        except Exception as e:
            unknown.append('ROOT lstat %s %r' % (r, e))

    decision = decide(left, unknown)
    if decision['verdict'] == 'UNKNOWN':
        print('\n'.join(['ABSENCE-UNKNOWN:'] + decision['details']))
        return 3
    if decision['verdict'] == 'FAIL':
        print('\n'.join(['LEFTOVER:'] + decision['details']))
        return 1
    if decision['verdict'] != 'PASS':
        raise ValueError('unsupported resource-absence verdict')
    print('ABSENCE-CLEAN host=%s units=0 ports=0 mounts=0 nvme=0 loop=0 dev=0 comms=0 procs=0 root=absent' % HOST)
    return 0
