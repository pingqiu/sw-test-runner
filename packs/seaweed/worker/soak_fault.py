"""Stage 3 host-side fault injectors and the resource sampler (M02, one token-scoped row at a time).

  --action apply     arm the host-side dead-man restore (systemd-run --on-active), then inject; print the witness
  --action restore   undo, prove the restore, cancel the dead-man; idempotent, and the dead-man runs this same action
  --action sample    the resource sampler unit: QP/MR by pid, /proc/pid/fd, limits, port, statfs, product metrics
  --action mark-load | mark-quiescence   the controller's phase markers for the sampler

Every witness row carries this host's CLOCK_MONOTONIC and the target's pid/starttime; the assessor joins them.
A view that cannot be read is recorded as null, never as 0. Nothing here retries a fault."""
import argparse, json, os, re, subprocess, sys, time, types
from pathlib import Path

if 'soak_stats' not in sys.modules:  # -I drops the script dir: load the exact protected sibling
    _source = Path(__file__).resolve().with_name('soak_stats.py')
    _module = sys.modules['soak_stats'] = types.ModuleType('soak_stats')
    _module.__file__ = str(_source)
    exec(compile(_source.read_bytes(), str(_source), 'exec'), _module.__dict__)
from soak_stats import fd_view, identity, limits, port_view, proc_memory, rdma_view, scrape, statfs_free

KINDS = ('control', 'link_flap', 'conn_kill', 'mr_pressure', 'disk_full', 'fd_exhaust')
DEADMAN = ('link_flap', 'mr_pressure', 'disk_full', 'fd_exhaust')  # kinds that leave state behind


class Refused(Exception):
    """A preflight or identity refusal: nothing was changed."""


def run(argv, check=True, timeout=30):
    done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    if check and done.returncode != 0:
        raise RuntimeError('%s exited %d: %s' % (argv[0], done.returncode, done.stderr.strip()[:200]))
    return done


def target(c):
    """The volume's live identity: the first of the row's units with a live MainPID."""
    for unit in c['units']:
        out = run(['systemctl', 'show', '-p', 'MainPID', '--value', unit], check=False).stdout.strip()
        if out.isdigit() and int(out) > 0:
            ident = identity(int(out))
            if ident:
                return dict(pid=ident['pid'], starttime=ident['starttime'])
    return None


def deadman_unit(c):
    return 'sw-fault-%s-%s' % (c['run_id'], c['episode'])  # outside the run's unit glob: cleanup never cancels it


def witness_path(c):
    return Path(c['root']) / ('fault-witness-%s.jsonl' % c['episode'])


def record(c, row):
    with witness_path(c).open('a') as f:
        f.write(json.dumps(row, sort_keys=True) + '\n')


def armed(c):
    """The arm row apply wrote before any change: the identity and the values a restore must return to."""
    try:
        rows = [json.loads(line) for line in witness_path(c).read_text().splitlines()]
    except FileNotFoundError:
        return None
    return next((r for r in rows if r.get('action') == 'arm'), None)


def sys_root(c):
    return c.get('sys_root', '/sys')  # tests point this at a fake tree; the runner never sets it


def conns(c, pid):
    """The volume's established TCP connections on the RDMA setup ports, as 'local>peer', or None."""
    try:
        out = run(['ss', '-Htnp', 'state', 'established'], check=False)
    except (OSError, subprocess.TimeoutExpired):
        return None
    if out.returncode != 0:
        return None
    rows = []
    for line in out.stdout.splitlines():
        f = line.split()
        if len(f) >= 4 and 'pid=%d,' % pid in line and f[2].rsplit(':', 1)[-1] in {str(p) for p in c['setup_ports']}:
            rows.append(f[2] + '>' + f[3])
    return sorted(rows)


def snapshot(c, ident):
    """The kind's host-event view of the target."""
    row = dict(mono=time.monotonic(), epoch=time.time(), identity=ident)
    if ident is None:
        return row
    pid, kind = ident['pid'], c['kind']
    fv = fd_view(pid)
    row.update(limits=limits(pid), fds=fv[0] if fv else None)
    if kind in ('link_flap', 'control'):
        row['port'] = port_view(sys_root(c), c['rdma_device'], c['rdma_port']) if c.get('rdma_device') else None
    if kind in ('conn_kill', 'control'):
        row['conns'] = conns(c, pid) if c.get('setup_ports') else None
    if kind in ('disk_full', 'control'):
        row['free'] = statfs_free(c['mount']) if c.get('mount') else None
    return row


def addresses(dev):
    out = run(['ip', '-o', 'addr', 'show', 'dev', dev]).stdout
    return {m.split('/')[0] for m in re.findall(r'inet6? (\S+)', out)}


def link_preflight(c):
    """F3 touches the data NIC only: it must be the row's RDMA device, carry no SSH session, no management
    address and no default route."""
    dev = c['netdev']
    if not re.fullmatch(r'[A-Za-z0-9_.:-]{1,15}', dev):
        raise Refused('netdev name')
    ib = Path(sys_root(c)) / 'class' / 'net' / dev / 'device' / 'infiniband'
    if not (ib / c['rdma_device']).exists():
        raise Refused('netdev is not the row RDMA device')
    mine = addresses(dev)
    if mine & set(c['management_addrs']):
        raise Refused('netdev carries a management address')
    if re.search(r'\bdev %s\b' % re.escape(dev), run(['ip', 'route', 'show', 'default']).stdout):
        raise Refused('netdev carries the default route')
    ssh = run(['ss', '-Htn', 'state', 'established', '( sport = :22 )']).stdout
    if any(f.split()[2].rsplit(':', 1)[0].strip('[]') in mine for f in ssh.splitlines() if len(f.split()) >= 4):
        raise Refused('netdev carries the SSH route')


def mount_preflight(c):
    """F6: the ballast lives in this run's own loop mount, inside the run root, and nowhere else."""
    mount, root = Path(c['mount']).resolve(), Path(c['root']).resolve()
    if mount != root and root not in mount.parents:
        raise Refused('mount outside the run root')
    for line in Path(c.get('mountinfo', '/proc/self/mountinfo')).read_text().splitlines():
        f = line.split()
        if len(f) > 4 and f[4] == str(mount) and '-' in f:
            source = f[f.index('-') + 2]
            if re.fullmatch(r'/dev/loop[0-9]+', source):
                return
            raise Refused('mount is not a loop device')
    raise Refused('mount not found')


def ballast(c):
    return Path(c['mount']) / ('.sw-fault-ballast-%s' % c['episode'])


def apply(c):
    kind = c['kind']
    ident = target(c)
    if ident is None:
        raise Refused('no live target')
    before = snapshot(c, ident)
    plan = {}
    if kind == 'link_flap':
        link_preflight(c)
        if (before.get('port') or [''])[0].split(':')[-1].strip() != 'ACTIVE':
            raise Refused('port not ACTIVE before the flap')
    elif kind == 'conn_kill':
        if not before.get('conns'):
            raise Refused('no setup connection to kill')
    elif kind == 'mr_pressure':
        mem = proc_memory('/proc/%d/status' % ident['pid']) or {}
        use = max(mem.get('vm_pin_kB', 0), mem.get('vm_lck_kB', 0)) * 1024
        if not (before.get('limits') and 0 < c['memlock_bytes'] < use):
            raise Refused('memlock target not below current use (%d bytes)' % use)
        plan = dict(memlock=c['memlock_bytes'], use_bytes=use)
    elif kind == 'fd_exhaust':
        if before.get('fds') is None or not before.get('limits'):
            raise Refused('fd view unreadable')
        plan = dict(nofile=before['fds'] + c['nofile_extra'])
        soft = before['limits']['nofile'][0]
        if soft != -1 and plan['nofile'] >= soft:
            raise Refused('nofile target not below the current soft limit')
    elif kind == 'disk_full':
        mount_preflight(c)
        free = before.get('free')
        if free is None:
            raise Refused('statfs unreadable')
        plan = dict(ballast_bytes=free - c['free_below_bytes'] // 2)
        if not 0 < plan['ballast_bytes'] <= c['max_ballast_bytes']:
            raise Refused('ballast outside its bound')
    record(c, dict(action='arm', kind=kind, identity=ident, before=before, plan=plan))
    deadman = None
    if kind in DEADMAN:  # armed BEFORE the change, so a dead controller still gets its restore
        deadman = dict(unit=deadman_unit(c), on_active_s=c['deadman_s'])
        run(['systemd-run', '--unit=' + deadman['unit'], '--on-active=%ds' % c['deadman_s'],
             '--timer-property=AccuracySec=1s', c['python'], '-I', c['script'], '--config', json.dumps(c),
             '--action', 'restore'])
    pid = str(ident['pid'])
    if kind == 'link_flap':
        run(['ip', 'link', 'set', 'dev', c['netdev'], 'down'])
        wait_port(c, lambda s: 'ACTIVE' not in s, 10)
    elif kind == 'conn_kill':
        ports = ' or '.join('sport = :%d' % p for p in c['setup_ports'])
        run(['ss', '-K', '-tn', 'state', 'established', '( %s )' % ports])
    elif kind == 'mr_pressure':
        run(['prlimit', '--pid', pid, '--memlock=%d:' % plan['memlock']])
    elif kind == 'fd_exhaust':
        run(['prlimit', '--pid', pid, '--nofile=%d:' % plan['nofile']])
    elif kind == 'disk_full':
        fd = os.open(ballast(c), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            os.posix_fallocate(fd, 0, plan['ballast_bytes'])
        finally:
            os.close(fd)
    after = snapshot(c, target(c))
    row = dict(action='apply', kind=kind, identity=ident, before=before, after=after, plan=plan, deadman=deadman)
    record(c, row)
    return row


def wait_port(c, ok, seconds):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        view = port_view(sys_root(c), c['rdma_device'], c['rdma_port'])
        if view and ok(view[0]):
            return
        time.sleep(0.2)
    raise RuntimeError('port state did not change within %d s' % seconds)


def restore(c):
    """Undo to the arm row's values, prove it, then cancel the dead-man. Restored is True only when proven."""
    kind, arm = c['kind'], armed(c)
    ident = target(c)
    proof, restored = {}, arm is None  # nothing was armed, so nothing was changed
    if arm is not None:
        same = ident == arm['identity']
        if kind == 'link_flap':
            run(['ip', 'link', 'set', 'dev', c['netdev'], 'up'])
            wait_port(c, lambda s: 'ACTIVE' in s, 30)
            restored = True
        elif kind in ('mr_pressure', 'fd_exhaust') and same:
            key = 'memlock' if kind == 'mr_pressure' else 'nofile'
            soft = arm['before']['limits'][key][0]
            run(['prlimit', '--pid', str(ident['pid']), '--%s=%s:' % (key, 'unlimited' if soft == -1 else soft)])
            restored = (limits(ident['pid']) or {}).get(key) == arm['before']['limits'][key]
        elif kind == 'disk_full':
            try:
                ballast(c).unlink()
            except FileNotFoundError:
                pass
            restored = not ballast(c).exists() and (statfs_free(c['mount']) or 0) >= c['free_below_bytes']
        elif kind in ('conn_kill', 'control'):
            restored = True  # nothing persists
        proof['same_target'] = same
    after = snapshot(c, ident)
    if kind in DEADMAN:
        unit = deadman_unit(c)
        run(['systemctl', 'stop', unit + '.timer'], check=False)
        state = run(['systemctl', 'is-active', unit + '.timer'], check=False).stdout.strip()
        proof['deadman_timer'] = state
        restored = restored and state != 'active'
    row = dict(action='restore', kind=kind, identity=ident, after=after, restored=restored, proof=proof)
    record(c, row)
    return row


def sample(c):
    """One row per sample_s until quiescence_s after the quiescence marker, or max_s."""
    root = Path(c['root'])
    start, quiet = time.monotonic(), None
    with (root / 'resource-samples.jsonl').open('x', buffering=1) as out:
        for seq in range(10 ** 6):
            now = time.monotonic()
            if now - start > c['max_s']:
                break
            phase = 'quiescence' if (root / 'resources-phase-quiescence').exists() else \
                'load' if (root / 'resources-phase-load').exists() else 'pre_load'
            if phase == 'quiescence' and quiet is None:
                quiet = now
            ident = target(c)
            row = dict(seq=seq, mono=now, epoch=time.time(), phase=phase, identity=ident,
                       watch={label: watched(unit) for label, unit in sorted(c.get('watch', {}).items())})
            if ident:
                pid = ident['pid']
                fv = fd_view(pid)
                mem = proc_memory('/proc/%d/status' % pid)
                row.update(fds=fv[0] if fv else None, dat_sizes=fv[1] if fv else None, limits=limits(pid),
                           rdma=rdma_view(pid), vm_pin_kB=(mem or {}).get('vm_pin_kB'))
            if c.get('rdma_device'):
                row['port'] = port_view(sys_root(c), c['rdma_device'], c['rdma_port'])
            if c.get('mount'):
                row['free'] = statfs_free(c['mount'])
            if c.get('metrics_url'):
                row['metrics'] = scrape(c['metrics_url'], c.get('metrics', {}))
            out.write(json.dumps(row, sort_keys=True) + '\n')
            if quiet is not None and now - quiet >= c['quiescence_s']:
                break
            time.sleep(max(0, start + (seq + 1) * c['sample_s'] - time.monotonic()))


def watched(unit):
    out = run(['systemctl', 'show', '-p', 'MainPID', '--value', unit], check=False).stdout.strip()
    ident = identity(int(out)) if out.isdigit() and int(out) > 0 else None
    return ident and dict(pid=ident['pid'], starttime=ident['starttime'])


def mark(c, phase):
    fd = os.open(Path(c['root']) / ('resources-phase-' + phase), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    os.close(fd)
    return dict(action='mark', phase=phase, mono=time.monotonic())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', required=True)
    parser.add_argument('--action', required=True, choices=('apply', 'restore', 'sample', 'mark-load', 'mark-quiescence'))
    args = parser.parse_args()
    c = json.loads(args.config)
    if c.get('kind') not in KINDS:
        raise SystemExit('soak_fault: unknown kind')
    if args.action == 'sample':
        return sample(c)
    try:
        row = dict(apply=apply, restore=restore).get(args.action, lambda c: mark(c, args.action[5:]))(c)
    except Refused as error:
        row = dict(action=args.action, kind=c['kind'], refused=str(error))
        print(json.dumps(row, sort_keys=True), flush=True)
        raise SystemExit(3)
    print(json.dumps(row, sort_keys=True), flush=True)
    if args.action == 'restore' and not row['restored']:
        raise SystemExit(4)  # CLEANUP_UNKNOWN: the controller stops the stage


if __name__ == '__main__':
    main()
