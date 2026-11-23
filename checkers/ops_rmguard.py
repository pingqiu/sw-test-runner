"""Authenticated run-directory removal; loading this module performs no actions."""

def run(packet_file, arguments):
    __file__ = packet_file
    argv = [packet_file, *arguments]
    """rmguard.py R DEVINO: the ONLY path that deletes a run's root (python3.12 -I, stdlib only, root). Reusable as is: the
    run's bindings (RUN_ID, RUN_ID_RE, OWNED_ROOTS, TEST_SEAM_PREFIX) are DATA in run.env next to this file.
    r3 (r2 review P1-2): the recorded object is ANCHORED by file descriptors from the identity check through the
    whole deletion; no absolute pathname is resolved again after the check.
      walk      starting at an fd for "/", every component of dirname(R) is opened with O_DIRECTORY|O_NOFOLLOW relative to
                its parent's fd (a symlink anywhere fails the open); each fd is fstat'ed: a directory owned by root or by
                the owner of <home>, not group/world-writable unless sticky. The fd of R's parent is kept.
      identity  R's last component is opened with O_DIRECTORY|O_NOFOLLOW relative to that kept parent fd; fstat(fd) must
                equal DEVINO (recorded when this run created R). R must also be canonical and end in /<one of OWNED_ROOTS> ({run} expanded).
      mounts    /proc/self/mountinfo is read in full and parsed (every line has ' - ' and >= 5 fields, '/' present, octal
                unescaped); no mount point at or under R. During the removal every directory must also be on R's st_dev.
      delete    contents are removed relative to the anchored fd only (scandir(fd), fstatat/openat with O_NOFOLLOW,
                unlinkat, rmdir at the parent fd), each child directory re-verified (fstat == the fstatat it was chosen
                by, same st_dev). Last, R's entry in the kept parent fd is fstatat'ed again: only if it still names
                DEVINO is it rmdir'ed (rmdir removes only an EMPTY directory). If R was renamed away and something else
                put at the path, that impostor is never touched: the result is INCOMPLETE (our directory was emptied at
                whatever name it now has).
    Exit 0 "RMGUARD DELETED"; 2 "RMGUARD REFUSED ..." (nothing deleted); 3 "RMGUARD UNKNOWN ..." (an inventory could not
    be read or parsed: nothing deleted); 4 "RMGUARD INCOMPLETE ..." (deletion started and did not finish, or R moved).
    Test seams (unset in operation; sudo strips them in teardown): <TEST_SEAM_PREFIX>ABS_ROOT prefixes /proc;
    <TEST_SEAM_PREFIX>RMGUARD_PAUSE=F
    makes it write F.ready after the identity check and wait (<= 60 s) for F.go, so a control can swap R in between."""
    import os, stat, time


    def _runenv(need):
        """run.env next to this file: the run's bindings as DATA (KEY=VALUE lines, '#' comments; never executed). RUN_ID must
        fully match RUN_ID_RE; every key in need must be present. Raises ValueError/OSError otherwise (callers fail
        closed). This keeps the helper byte-identical for any packet: no run, seat, host, port or prefix literal here."""
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


    try:
        E = _runenv(('OWNED_ROOTS', 'TEST_SEAM_PREFIX'))
    except Exception as _e:
        E, ERR = None, _e
    ROOT = os.environ.get(E['TEST_SEAM_PREFIX'] + 'ABS_ROOT', '') if E else ''
    PAUSE = os.environ.get(E['TEST_SEAM_PREFIX'] + 'RMGUARD_PAUSE', '') if E else ''
    DF = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | getattr(os, 'O_CLOEXEC', 0)


    def out(msg, rc):
        print(msg, flush=True)
        raise SystemExit(rc)


    def unescape(p):
        o, i = [], 0
        while i < len(p):
            if p[i] == '\\' and i + 3 < len(p) and all(c in '01234567' for c in p[i + 1:i + 4]):
                o.append(chr(int(p[i + 1:i + 4], 8)))
                i += 4
            else:
                o.append(p[i])
                i += 1
        return ''.join(o)


    class Stop(Exception):
        def __init__(self, rc, msg):
            self.rc, self.msg = rc, msg


    def wipe(dfd, rdev, where, errs):
        """Remove every entry of the directory open at dfd, relative to dfd only."""
        try:
            names = [e.name for e in os.scandir(dfd)]
        except OSError as e:
            errs.append('scandir %s: %r' % (where, e))
            return
        for n in names:
            try:
                st = os.stat(n, dir_fd=dfd, follow_symlinks=False)
            except FileNotFoundError:
                continue
            except OSError as e:
                errs.append('fstatat %s/%s: %r' % (where, n, e))
                continue
            if stat.S_ISDIR(st.st_mode):
                if st.st_dev != rdev:
                    raise Stop(4, 'RMGUARD INCOMPLETE: %s/%s is on another device (a mount inside R appeared): stopped' % (where, n))
                try:
                    cfd = os.open(n, DF, dir_fd=dfd)
                except OSError as e:
                    errs.append('openat %s/%s: %r' % (where, n, e))
                    continue
                try:
                    c = os.fstat(cfd)
                    if (c.st_dev, c.st_ino) != (st.st_dev, st.st_ino):
                        errs.append('%s/%s changed between fstatat and openat: skipped' % (where, n))
                        continue
                    wipe(cfd, rdev, where + '/' + n, errs)
                finally:
                    os.close(cfd)
                try:
                    os.rmdir(n, dir_fd=dfd)
                except OSError as e:
                    errs.append('rmdir %s/%s: %r' % (where, n, e))
            else:
                try:
                    os.unlink(n, dir_fd=dfd)
                except FileNotFoundError:
                    pass
                except OSError as e:
                    errs.append('unlink %s/%s: %r' % (where, n, e))


    def main():
        if len(argv) != 3:
            out('use: rmguard.py R DEVINO', 2)
        R, DEVINO = argv[1], argv[2]
        if E is None:
            out('RMGUARD UNKNOWN: run.env %r: nothing deleted' % ERR, 3)
        tails = ['/' + r.replace('{run}', E['RUN_ID']).strip('/') for r in E['OWNED_ROOTS'].split() if '{host}' not in r]
        if not os.path.isabs(R) or os.path.realpath(R) != R or not any(R.endswith(t) for t in tails) or ':' not in DEVINO:
            out('RMGUARD REFUSED identity: R %r is not canonical ...%s or no recorded dev:ino (%r)' % (R, '|'.join(tails), DEVINO), 2)
        comps = [c for c in os.path.dirname(R).split('/') if c]
        name = os.path.basename(R)
        fds = []
        try:
            # walk: fds from "/" down to <home>/sw-runs, never following a symlink
            try:
                fds.append(os.open('/', DF))
                for c in comps:
                    fds.append(os.open(c, DF, dir_fd=fds[-1]))
            except OSError as e:
                raise Stop(2, 'RMGUARD REFUSED ancestry: opening %s with O_NOFOLLOW: %r' % ('/' + '/'.join(comps[:len(fds)]), e))
            sts = [os.fstat(f) for f in fds]
            huid = sts[-2].st_uid  # <home>
            for i, s in enumerate(sts):
                p = '/' + '/'.join(comps[:i])
                if not stat.S_ISDIR(s.st_mode):
                    raise Stop(2, 'RMGUARD REFUSED ancestry: %s is not a directory' % p)
                if s.st_uid not in (0, huid):
                    raise Stop(2, 'RMGUARD REFUSED ancestry: %s owned by uid %d' % (p, s.st_uid))
                if (s.st_mode & 0o022) and not (s.st_mode & stat.S_ISVTX):
                    raise Stop(2, 'RMGUARD REFUSED ancestry: %s is group/world-writable (%o)' % (p, s.st_mode & 0o777))
            pfd = fds[-1]
            # identity: R relative to the kept parent fd
            try:
                rfd = os.open(name, DF, dir_fd=pfd)
            except FileNotFoundError:
                raise Stop(0, 'RMGUARD DELETED (already absent)')
            except OSError as e:
                raise Stop(2, 'RMGUARD REFUSED identity: opening %s with O_NOFOLLOW: %r' % (R, e))
            fds.append(rfd)
            rs = os.fstat(rfd)
            if '%d:%d' % (rs.st_dev, rs.st_ino) != DEVINO:
                raise Stop(2, 'RMGUARD REFUSED identity: %s is dev:ino %d:%d, recorded %s' % (R, rs.st_dev, rs.st_ino, DEVINO))
            # mounts: full parse
            try:
                with open(ROOT + '/proc/self/mountinfo', 'rb') as f:
                    data = f.read().decode('utf-8', 'surrogateescape')
            except Exception as e:
                raise Stop(3, 'RMGUARD UNKNOWN: mountinfo unreadable: %r' % e)
            mps = []
            for n, l in enumerate(data.splitlines(), 1):
                if not l.strip():
                    continue
                pre = l.split(' - ', 1)[0].split()
                if ' - ' not in l or len(pre) < 5:
                    raise Stop(3, 'RMGUARD UNKNOWN: mountinfo line %d does not parse: %r' % (n, l[:120]))
                mps.append(unescape(pre[4]))
            if '/' not in mps:
                raise Stop(3, 'RMGUARD UNKNOWN: mountinfo has no root mount (%d entries)' % len(mps))
            under = [m for m in mps if m == R or m.startswith(R + '/')]
            if under:
                raise Stop(2, 'RMGUARD REFUSED mounts under R: %s' % under[:5])
            if PAUSE:
                open(PAUSE + '.ready', 'w').close()
                t0 = time.time()
                while not os.path.exists(PAUSE + '.go') and time.time() - t0 < 60:
                    time.sleep(0.1)
            # delete, anchored at rfd
            errs = []
            wipe(rfd, rs.st_dev, R, errs)
            if errs:
                raise Stop(4, 'RMGUARD INCOMPLETE: %d errors: %s' % (len(errs), '; '.join(errs[:3])))
            try:
                now = os.stat(name, dir_fd=pfd, follow_symlinks=False)
            except FileNotFoundError:
                raise Stop(4, 'RMGUARD INCOMPLETE: %s no longer names our directory (moved away; emptied at its new name)' % R)
            if (now.st_dev, now.st_ino) != (rs.st_dev, rs.st_ino):
                raise Stop(4, 'RMGUARD INCOMPLETE: %s now names dev:ino %d:%d, not ours (%s): left untouched; ours was emptied at its new name' % (R, now.st_dev, now.st_ino, DEVINO))
            try:
                os.rmdir(name, dir_fd=pfd)
            except OSError as e:
                raise Stop(4, 'RMGUARD INCOMPLETE: rmdir %s: %r' % (R, e))
            raise Stop(0, 'RMGUARD DELETED %s (dev:ino %s, fd-anchored, %d mounts checked)' % (R, DEVINO, len(mps)))
        except Stop as s:
            for f in reversed(fds):
                try:
                    os.close(f)
                except OSError:
                    pass
            out(s.msg, s.rc)


    main()
