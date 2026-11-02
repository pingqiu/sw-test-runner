"""M01/M02 profile programs; the runner alone owns admission and processes."""
import hashlib
import ipaddress
import json
import math
import re
import os
import stat
import argparse
import datetime
import socket
import subprocess
import sys
import types
import http.client
import io
from urllib.parse import urlencode, urlsplit


def stream_digest(stream):
    digest = hashlib.sha256()
    for chunk in iter(lambda: stream.read(1024 * 1024), b''):
        digest.update(chunk)
    return digest.hexdigest()
from pathlib import Path, PurePosixPath
import time


def qualify_hardware(config, raw):
    def require(condition, reason):
        if not condition:
            raise ValueError(reason)
    require(raw['host'] == config['host'] and raw['boot_id'] == config['boot_id'],
            'host/boot changed')
    start, end = raw['capture_start'], raw['capture_end']
    require(all(type(t) in (int, float) and math.isfinite(t) for t in (start, end))
            and 0 <= end - start <= 45, 'hardware capture bracket')
    require(config['port'] == 1, 'profile supports port 1 only')
    device = raw['rdma_devices'][config['device']]
    firmware = re.findall(r'^\s*fw_ver:\s*(\S+)\s*$',
                          raw['devices_raw'][config['device']], re.MULTILINE)
    require(device['pci'] == config['pci'] and
            device['physical_pci'] == config['physical_pci'] and
            firmware == [config['firmware']], 'PCI/PF/firmware identity changed')
    gid = device['chosen']
    require(device['state'] == '4: ACTIVE' and
            device['active_mtu'] == config['active_mtu'] and
            device['max_tx_rate_mbps'] == 40000, 'RDMA port state/MTU/cap')
    require(gid['index'] == config['gid_index'] and gid['type'] == 'RoCE v2'
            and gid['netdev'] == config['nic'] and
            str(ipaddress.ip_address(gid['gid']).ipv4_mapped) == config['address'],
            'selected GID does not bind address/NIC')
    links = [r for r in raw['links'] if r['ifname'] == config['nic']]
    require(len(links) == 1 and links[0]['mtu'] == config['mtu'] and
            {'UP', 'LOWER_UP'} <= set(links[0]['flags']), 'NIC state/MTU')
    # An active mlx5 port/GID proves no DCI/DCT operation. Preserve that gap.
    return dict(status='OBSERVED', dc_capability='NOT_PROVEN', observation=raw)


def observe(host):
    start = time.time()
    boot = Path('/proc/sys/kernel/random/boot_id').read_text().strip()
    # Load the exact protected sibling, never a package selected from sys.path.
    source = Path(__file__).resolve().with_name('vf_preflight.py')
    module = types.ModuleType('_profile_vf')
    exec(compile(source.read_bytes(), str(source), 'exec'), module.__dict__)
    raw = module.observe(host)
    raw.update(boot_id=boot, capture_start=start, capture_end=time.time(),
               kernel=os.uname().release)
    if boot != Path('/proc/sys/kernel/random/boot_id').read_text().strip():
        raise ValueError('boot changed during capture')
    return raw


def qualify_process(expected, current):
    for field in ('pid', 'starttime', 'boot_id', 'executable_sha256'):
        if expected[field] != current[field]:
            raise ValueError('service generation changed: ' + field)
    if current['state'] in ('Z', 'X', 'x'):
        raise ValueError('service not live')


def process(pid):
    proc = Path('/proc') / str(pid)
    before = (proc / 'stat').read_text().rsplit(')', 1)[1].split()
    with (proc / 'exe').open('rb') as stream:
        digest = stream_digest(stream)
    after = (proc / 'stat').read_text().rsplit(')', 1)[1].split()
    if before[19] != after[19]:
        raise ValueError('service changed during capture')
    return dict(pid=pid, starttime=int(after[19]), state=after[0],
                boot_id=Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
                executable_sha256=digest)


def product_command(config, role):
    root = PurePosixPath(config['root'])
    binary = PurePosixPath(config['release']) / 'bin'
    ports = config['services']
    address = config['address']
    master = address + ':' + str(ports['master_http'])
    # No inherited RDMA selector, credentials, shell configuration or tmp path.
    env = dict(PATH='/usr/bin:/bin', TZ='UTC', HOME=str(root / 'home'),
               XDG_CACHE_HOME=str(root / 'cache'), TMPDIR=str(root / 'tmp'),
               TMP=str(root / 'tmp'), TEMP=str(root / 'tmp'),
               WEED_MASTER_MAINTENANCE_SCRIPTS='gate-disabled',
               WEED_MASTER_MAINTENANCE_SLEEP_MINUTES='1000000')
    if role == 'master':
        argv = [str(binary / 'weed'), 'master', '-peers=none',
                '-mdir=' + str(root / 'master'), '-ip=' + address,
                '-port=' + str(ports['master_http']),
                '-port.grpc=' + str(ports['master_grpc']),
                '-volumeSizeLimitMB=32', '-defaultReplication=000']
    elif role == 'filer':
        argv = [str(binary / 'weed'), 'filer', '-master=' + master,
                '-ip=' + address, '-port=' + str(ports['filer_http']),
                '-port.grpc=' + str(ports['filer_grpc']),
                '-defaultStoreDir=' + str(root / 'filer'),
                '-localSocket=' + str(root / 'filer.sock'),
                '-options=' + str(root / 'filer.toml')]
    elif role == 'volume':
        argv = [str(binary / 'weed-volume'), '--port', str(ports['volume_http']),
                '--port.grpc', str(ports['volume_grpc']), '--ip', address,
                '--ip.bind', '0.0.0.0', '--master', master,
                '--dir', str(root / 'volume'), '--max', '24',
                '--fileSizeLimitMB', '32', '--rdma.enabled', '--rdma.ip', address,
                '--rdma.port', str(ports['rdma']), '--rdma.kv-transport', 'rc']
        env.update(SWFS_RDMA_DC_DEVICE=config['device'], SWFS_RDMA_DC_PORT='1',
                   SWFS_RDMA_DC_GID_INDEX=str(config['gid_index']),
                   SWFS_RDMA_DC_PUSH_CONTROL_PORT=str(ports['dc_control']),
                   SWFS_RDMA_PUSH_PORT=str(ports['rdma_push']),
                   SWFS_RDMA_PUSH_CONTROL_PORT=str(ports['rdma_control']),
                   RUST_LOG='info,rdma_rail=info')
    else:
        raise ValueError('undeclared service role')
    return argv, env


ROTATION_KEYS = {
    'collections': str, 'high_watermark': float, 'low_watermark': float,
    'min_age_minutes': int, 'max_volumes_per_cycle': int,
    'min_remaining_volumes': int, 'check_interval_seconds': int}
NAME = re.compile(r'[A-Za-z0-9_.-]{1,64}')


def master_toml(config):
    """The private master.toml. Without a plan 'rotation' block, cache-volume
    rotation stays off (the S0b profile). With one, rotation is on with exactly
    the declared settings; anything undeclared or malformed refuses."""
    rotation = config.get('rotation')
    if rotation is None:
        return '[master.cache_volume_rotation]\nenabled = false\n'
    if not isinstance(rotation, dict) or set(rotation) != set(ROTATION_KEYS):
        raise ValueError('rotation keys must be exactly ' + ','.join(sorted(ROTATION_KEYS)))
    for key, kind in ROTATION_KEYS.items():
        value = rotation[key]
        if isinstance(value, bool) or not isinstance(value, (int, float) if kind is float else kind):
            raise ValueError('rotation ' + key + ' type')
    if not all(NAME.fullmatch(c) for c in rotation['collections'].split(',')):
        raise ValueError('rotation collections')
    if not 0 < rotation['low_watermark'] < rotation['high_watermark'] < 1:
        raise ValueError('rotation watermarks need 0 < low < high < 1')
    # The product's own rules (master_cache_volume_rotation.go validate...),
    # plus caps far inside int64 and time.Duration: an out-of-range value either
    # stops the master (Fatalf) or makes it skip master.toml (rotation silently off).
    for key, low, high in (('min_age_minutes', 0, 10**6), ('max_volumes_per_cycle', 1, 10**6),
                           ('min_remaining_volumes', 1, 10**6), ('check_interval_seconds', 1, 86400)):
        if not low <= rotation[key] <= high:
            raise ValueError('rotation %s must be in [%d, %d]' % (key, low, high))
    lines = ['[master.cache_volume_rotation]', 'enabled = true',
             'collections = "' + rotation['collections'] + '"']
    lines += [key + ' = ' + repr(rotation[key]) for key in ROTATION_KEYS if key != 'collections']
    return '\n'.join(lines) + '\n'


# The product unit's open-file limit (seaweed-mono
# enterprise/terraform/modules/core/templates/weed.service.tftpl: LimitNOFILE=1048576).
PRODUCT_NOFILE = 1048576


def volume_nofile(config):
    """Optional plan 'volume_nofile': the soft RLIMIT_NOFILE the volume runs
    with, as the product unit sets it. The volume sizes its RDMA connection
    budget from the soft limit (soft/8, clamped 8..256), so a lab shell's 1024
    caps it at 128. Absent (no key): the inherited limit is unchanged. Present,
    it must be an integer, so an explicit null refuses. The hard limit is never
    raised; a value above it refuses."""
    if 'volume_nofile' not in config:
        return None
    value = config['volume_nofile']
    if isinstance(value, bool) or not isinstance(value, int) or not 1024 <= value <= PRODUCT_NOFILE:
        raise ValueError('volume_nofile must be an integer in [1024, %d]' % PRODUCT_NOFILE)
    import resource
    hard = resource.getrlimit(resource.RLIMIT_NOFILE)[1]
    if hard != resource.RLIM_INFINITY and value > hard:
        raise ValueError('volume_nofile %d exceeds the hard limit %d' % (value, hard))
    return value


def assign_probe_url(config, master):
    """Optional plan 'ready_assign' = the workload's own layout. Leader plus
    topology is not ready after a master restart: the master refuses Assign for
    its whole warm-up. The probe allocates one unused fid. If the layout has no
    writable volume, the master grows one for it; ready() records whether it did."""
    layout = config.get('ready_assign')
    if layout is None:
        return None
    if not isinstance(layout, dict) or set(layout) != {'collection', 'replication', 'ttl'}:
        raise ValueError('ready_assign keys must be exactly collection,replication,ttl')
    if (not isinstance(layout['collection'], str) or not NAME.fullmatch(layout['collection']) or
            not isinstance(layout['replication'], str) or not re.fullmatch(r'[0-9]{3}', layout['replication']) or
            not isinstance(layout['ttl'], str) or not re.fullmatch(r'([1-9][0-9]{0,4}[mhdwMy])?', layout['ttl'])):
        raise ValueError('ready_assign layout')
    return master + '/dir/assign?' + urlencode(dict(layout, count=1))


def layout_writables(topology, layout):
    """Writable volume ids of the ready_assign layout in a /dir/status answer."""
    try:
        rows = topology['Topology']['Layouts'] or []
        return sorted({int(v) for row in rows
                       if (row.get('collection'), row.get('replication'), row.get('ttl') or '') ==
                       (layout['collection'], layout['replication'], layout['ttl'])
                       for v in (row.get('writables') or [])})
    except (KeyError, TypeError, ValueError):
        raise ValueError('topology layouts unreadable')


def remaining(config, limit=2):
    deadline = datetime.datetime.fromisoformat(config['deadline'].replace('Z', '+00:00')).timestamp()
    seconds = deadline - time.time()
    if seconds <= 0:
        raise TimeoutError('original profile work deadline')
    return min(limit, seconds)


def private_root(config):
    root = Path(config['root'])
    info = root.lstat()
    if not stat.S_ISDIR(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o700 or info.st_uid != os.getuid():
        raise ValueError('profile output root must already be private and owned')
    if root.resolve() != root or root == Path('/'):
        raise ValueError('profile root alias')
    return root


def storage(config):
    root = private_root(config)
    rows = []
    for disk in config['disks']:
        if disk['format_allowed'] is not False:
            raise ValueError('formatting is not part of this profile')
        mount = Path(disk['mount'])
        device = Path(disk['device_id'])
        if not str(device).startswith('/dev/disk/by-id/') or device.parent != Path('/dev/disk/by-id'):
            raise ValueError('storage identity must be one exact by-id device')
        mount_info, device_info, root_info = mount.stat(), device.stat(), root.stat()
        if mount.resolve() != mount or not stat.S_ISBLK(device_info.st_mode) or device_info.st_rdev != mount_info.st_dev or root_info.st_dev != mount_info.st_dev:
            raise ValueError('output root/backing device/mount mismatch')
        fs = os.statvfs(root)
        if fs.f_bavail * fs.f_frsize < 512 * 1024 * 1024:
            raise ValueError('smoke filesystem free space below 512 MiB')
        rows.append(dict(id=disk['id'], mount=str(mount), device_id=str(device),
                         resolved_device=str(device.resolve()),
                         major=os.major(device_info.st_rdev), minor=os.minor(device_info.st_rdev),
                         available_bytes=fs.f_bavail * fs.f_frsize,
                         format_allowed=False))
    if 'volume' in config['roles'] and not rows:
        raise ValueError('volume backing device not declared')
    return rows


def free_service_ports(config):
    declared = set(config['services'].values())
    observed = []
    for name in ('tcp', 'tcp6'):
        for line in (Path('/proc/net') / name).read_text().splitlines()[1:]:
            fields = line.split()
            if fields[3] == '0A':
                port = int(fields[1].split(':')[1], 16)
                if port in declared:
                    observed.append(port)
    if observed:
        raise ValueError('declared profile listener already occupied: ' + repr(sorted(set(observed))))
    return dict(declared_ports=sorted(declared), listening_conflicts=[])


def filer_config(root):
    return '[leveldb2]\nenabled = true\ndir = ' + json.dumps(str(root / 'filer' / 'ldb2')) + '\n'


def filer_metadata(root, original=None):
    info = (root / 'filer' / 'ldb2').lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
        raise ValueError('filer metadata identity not private')
    identity = dict(device=info.st_dev, inode=info.st_ino)
    if original is not None and identity != original.get('metadata_directory'):
        raise ValueError('original filer metadata directory changed')
    return identity


def prepare_filer_replacement(root, original):
    filer_metadata(root, original)
    metadata = root / 'filer' / 'ldb2'
    for item in (metadata, root / 'filer.toml'):
        info = item.lstat()
        if info.st_uid != os.getuid() or info.st_mode & 0o022 or stat.S_ISLNK(info.st_mode):
            raise ValueError('replacement filer state not private')
    if not metadata.is_dir() or not any(metadata.iterdir()):
        raise ValueError('replacement metadata missing or empty')
    config = root / 'filer.toml'
    before = config.lstat()
    if not stat.S_ISREG(before.st_mode) or config.read_bytes() != filer_config(root).encode():
        raise ValueError('replacement filer config changed')
    after = config.lstat()
    fields = ('st_dev','st_ino','st_size','st_mtime_ns','st_ctime_ns')
    if any(getattr(before,k) != getattr(after,k) for k in fields):
        raise ValueError('replacement filer config unstable')
    sock = root / 'filer.sock'
    if os.path.lexists(sock):
        info = sock.lstat()
        if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
            raise ValueError('replacement filer socket not owned')
        if any(line.split(None,7)[7:] == [str(sock)] for line in Path('/proc/net/unix').read_text().splitlines()):
            raise ValueError('replacement filer socket still active')
        if sock.lstat() != info:raise ValueError('replacement filer socket changed')
        filer_metadata(root, original)
        sock.unlink()


STATE_MARKER = '.sw-test-runner-state'


def stable_file(path):
    """(stat, sha256) of a regular file opened without following symlinks."""
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise ValueError(path.name + ' is not a regular file')
        with os.fdopen(os.dup(fd), 'rb') as stream:
            return info, stream_digest(stream)
    finally:
        os.close(fd)


def master_state(root):
    """Identity of the master's retained state. Mutable raft contents are
    deliberately not hashed. A directory's inode alone cannot tell a deleted
    and recreated mdir (ext4 reuses the freed number at once), so the join
    also binds a runner marker written inside it at the first launch, and the
    write-once master.toml, each by inode, ctime and bytes. ctime cannot be
    carried over by a copy."""
    mdir = os.lstat(root / 'master')
    if not stat.S_ISDIR(mdir.st_mode):
        raise ValueError('master state dir is not a directory')
    marker, marker_sha = stable_file(root / 'master' / STATE_MARKER)
    toml, toml_sha = stable_file(root / 'master.toml')
    return dict(mdir_dev=mdir.st_dev, mdir_ino=mdir.st_ino, mdir_uid=mdir.st_uid,
                mdir_mode=stat.S_IMODE(mdir.st_mode),
                marker_ino=marker.st_ino, marker_ctime_ns=marker.st_ctime_ns, marker_sha256=marker_sha,
                toml_dev=toml.st_dev, toml_ino=toml.st_ino, toml_ctime_ns=toml.st_ctime_ns,
                toml_mtime_ns=toml.st_mtime_ns, toml_sha256=toml_sha)


def check_master_state(root, old, config):
    """Before a replacement master touches anything: the same retained state
    as the original generation, never a recreated or swapped one."""
    want = old.get('master_state')
    if not isinstance(want, dict):
        raise ValueError('original master state join missing')
    got = master_state(root)
    if got != want:
        raise ValueError('master state continuity differs (recreated, swapped or rewritten)')
    if got['mdir_uid'] != os.getuid() or got['mdir_mode'] != 0o700:
        raise ValueError('master state dir not private')
    if not any(e.name != STATE_MARKER for e in os.scandir(root / 'master')):
        raise ValueError('master state dir is empty')
    if got['toml_sha256'] != hashlib.sha256(master_toml(config).encode()).hexdigest():
        raise ValueError('replacement master config differs')


def launch(config, role, replacement=False):
    remaining(config)
    root = private_root(config)
    argv, env = product_command(config, role)
    if role == 'filer' and not replacement and os.path.lexists(root / 'filer.sock'):
        raise ValueError('private filer socket already exists')
    if role not in config['roles']:
        raise ValueError('role not assigned to this host')
    if role == 'master':
        master_toml(config)  # refuse a malformed rotation block before any effect
    nofile = volume_nofile(config) if role == 'volume' else None  # likewise
    # Custody authorizes the pinned Python helper. It then execs the protected,
    # hash-qualified product in the SAME PID/session; no alternate supervisor.
    fd = os.open(argv[0], os.O_RDONLY | os.O_NOFOLLOW)
    try:
        before = os.fstat(fd)
        with os.fdopen(os.dup(fd), 'rb') as stream:
            digest = stream_digest(stream)
        after = os.fstat(fd)
        fields = ('st_dev', 'st_ino', 'st_mode', 'st_uid', 'st_gid', 'st_size', 'st_mtime_ns', 'st_ctime_ns')
        if any(getattr(before, k) != getattr(after, k) for k in fields) or digest != config['products'][Path(argv[0]).name]:
            raise ValueError('protected product hash/stability mismatch')
        if not stat.S_ISREG(before.st_mode) or before.st_mode & 0o022:
            raise ValueError('product must not be group/other writable')
        if replacement:
            if role not in ('volume','filer','master'):raise ValueError('unsupported replacement role')
            old=json.loads((root / (role+'-generation.json')).read_text())
            if old['boot_id'] != config['boot_id']:raise ValueError('replacement boot differs')
            try:current=process(old['pid'])
            except FileNotFoundError:current=None
            if current and current['starttime']==old['starttime'] and current['state'] not in ('Z','X','x'):
                raise ValueError('old '+role+' is still live')
            info=(root / role).lstat()
            if not stat.S_ISDIR(info.st_mode) or info.st_uid!=os.getuid() or info.st_mode & 0o022:
                raise ValueError('replacement data dir not private')
            if role == 'filer':prepare_filer_replacement(root, old)
            if role == 'master':check_master_state(root, old, config)
        else:
            (root / role).mkdir(mode=0o700, exist_ok=False)
            if role == 'filer':(root / role / 'ldb2').mkdir(mode=0o700)
        for sub in ('home', 'cache', 'tmp'):
            (root / sub).mkdir(mode=0o700, exist_ok=True)
        if role == 'master' and not replacement:
            # Select a private config instead of an unrelated /etc master.toml.
            with (root / 'master.toml').open('x') as stream:
                stream.write(master_toml(config))
            marker = os.open(root / 'master' / STATE_MARKER, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(marker, 'w') as stream:
                stream.write(os.urandom(16).hex() + '\n')
        if role == 'filer' and not replacement:
            with os.fdopen(os.open(root / 'filer.toml', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as stream:
                stream.write(filer_config(root))
        if nofile is not None:
            import resource
            resource.setrlimit(resource.RLIMIT_NOFILE, (nofile, resource.getrlimit(resource.RLIMIT_NOFILE)[1]))
        identity = process(os.getpid())
        if role == 'filer':identity['metadata_directory'] = filer_metadata(root, old if replacement else None)
        if nofile is not None:
            identity['nofile'] = dict(zip(('soft', 'hard'), resource.getrlimit(resource.RLIMIT_NOFILE)))
        identity.update(executable_sha256=digest, role=role, argv=argv,
                        logs=[os.readlink('/proc/self/fd/' + str(fd)) for fd in (1, 2)])
        if role == 'master' and replacement:
            # Never record a fresh join: re-verify the ORIGINAL join right before
            # publication and record that, so a swap after the first check is
            # refused, not adopted (codex01 R3).
            check_master_state(root, old, config)
            identity['master_state'] = old['master_state']
        elif role == 'master':
            identity['master_state'] = master_state(root)  # the original: the reference join
        with (root / (role + ('-replacement' if replacement else '') + '-generation.json')).open('x') as stream:
            json.dump(identity, stream, sort_keys=True)
        print(json.dumps(dict(event='PRODUCT_EXEC', **identity)), flush=True)
        os.chdir(root)
        remaining(config)
        if replacement and role == 'filer':filer_metadata(root, old)
        if replacement and role == 'master':
            check_master_state(root, old, config)  # the last check before exec
        os.execve(fd, argv, env)
    finally:
        os.close(fd)


def http_json(url, config, require_json=True):
    # Reuse the accepted preflight producer's absolute-deadline reader pattern:
    # a trickling header/body cannot restart a socket timeout indefinitely.
    endpoint = urlsplit(url)
    if endpoint.scheme != 'http' or endpoint.username or endpoint.password or endpoint.fragment:
        raise ValueError('profile HTTP endpoint')
    expected = (str(ipaddress.ip_address(endpoint.hostname)), endpoint.port or 80)
    owned = socket.create_connection(expected, timeout=remaining(config))
    wire_bytes = 0
    class Reader(io.RawIOBase):
        def readable(self):
            return True
        def readinto(self, buffer):
            nonlocal wire_bytes
            owned.settimeout(remaining(config))
            count = owned.recv_into(buffer)
            wire_bytes += count
            if wire_bytes > 128 * 1024:
                raise ValueError('readiness wire size')
            remaining(config)
            return count
    class ResponseSocket:
        def makefile(self, mode):
            if mode != 'rb':
                raise ValueError('HTTP reader mode')
            return io.BufferedReader(Reader())
    response = None
    try:
        peer = owned.getpeername()
        if (str(ipaddress.ip_address(peer[0])), peer[1]) != expected:
            raise ValueError('readiness HTTP peer mismatch')
        target = endpoint.path or '/'
        if endpoint.query:
            target += '?' + endpoint.query
        if '\r' in target or '\n' in target:
            raise ValueError('HTTP target')
        owned.settimeout(remaining(config))
        owned.sendall(('GET ' + target + ' HTTP/1.1\r\nHost: ' + endpoint.netloc +
                       '\r\nConnection: close\r\n\r\n').encode('ascii'))
        response = http.client.HTTPResponse(ResponseSocket())
        response.begin()
        if response.status != 200:
            raise ValueError('readiness response status (redirects refused)')
        body = bytearray()
        while True:
            remaining(config)
            part = response.read1(8192)
            body.extend(part)
            if len(body) > 65536:
                raise ValueError('readiness body size')
            if not part:
                break
        remaining(config)
        return json.loads(body) if require_json else bytes(body)
    finally:
        if response is not None:
            response.close()
        owned.close()


def owned_listeners(pid):
    inodes = set()
    for fd in (Path('/proc') / str(pid) / 'fd').iterdir():
        try:
            target = os.readlink(fd)
        except FileNotFoundError:
            continue
        if target.startswith('socket:['):
            inodes.add(target[8:-1])
    ports = set()
    for name in ('tcp', 'tcp6'):
        for line in (Path('/proc') / str(pid) / 'net' / name).read_text().splitlines()[1:]:
            fields = line.split()
            if fields[3] == '0A' and fields[9] in inodes:
                ports.add(int(fields[1].split(':')[1], 16))
    return ports


def services(config):
    root = private_root(config)
    facts = {}
    required = dict(master=('master_http', 'master_grpc'),
                    filer=('filer_http', 'filer_grpc'),
                    volume=('volume_http', 'volume_grpc', 'rdma_control', 'dc_control'))
    for role in config['roles']:
        if role == 'client':
            continue
        generation = root / (role + '-generation.json')
        if role in ('volume','filer','master') and (root / (role+'-replacement-generation.json')).exists():
            generation=root / (role+'-replacement-generation.json')
        expected = json.loads(generation.read_text())
        # Do not accept a live foreign executable named in an editable PID file.
        binary = 'weed-volume' if role == 'volume' else 'weed'
        if expected['executable_sha256'] != config['products'][binary] or expected['role'] != role:
            raise ValueError('service generation product mismatch')
        current = process(expected['pid'])
        qualify_process(expected, current)
        ports = owned_listeners(expected['pid'])
        if not {config['services'][p] for p in required[role]} <= ports:
            raise ValueError('service listeners not owned by exact generation: ' + role)
        facts[role] = dict(identity=current, listening_ports=sorted(ports),
            scope_identity=dict(PID=current['pid'],StartTime=current['starttime'],BootID=current['boot_id'],
                                ExecutableSHA256=current['executable_sha256']))
        if role == 'volume':
            logs = log_evidence(root, expected)
            if not any('RDMA DC push-read writer active' in line
                       for log in logs for line in log['markers']):
                raise ValueError('owned volume DC writer not active')
            cm = 'RDMA listener bound on ' + config['server_address'] + ':' + str(config['services']['rdma'])
            if not any(line.rstrip().endswith(cm) for log in logs for line in log['markers']):
                raise ValueError('owned volume RDMA CM bind not observed')
            facts[role]['logs'] = logs
    return facts


def log_evidence(root, generation):
    output = []
    for name in generation['logs']:
        file = Path(name)
        if file.parent != root or file.resolve() != file:
            raise ValueError('service log outside owned output root')
        fd = os.open(file, os.O_RDONLY | os.O_NOFOLLOW)
        try:
            before = os.fstat(fd)
            if not stat.S_ISREG(before.st_mode) or before.st_size > 65536:
                raise ValueError('smoke service log bound/type')
            body = os.pread(fd, before.st_size, 0)
            after = os.fstat(fd)
            if len(body) != before.st_size or before.st_size != after.st_size or before.st_mtime_ns != after.st_mtime_ns:
                raise ValueError('service log changed during snapshot')
            output.append(dict(path=name, bytes=len(body), sha256=hashlib.sha256(body).hexdigest(),
                               markers=[line for line in body.decode('utf-8', 'replace').splitlines()
                                        if 'RDMA DC push-read writer active' in line or
                                        'RDMA listener bound on ' in line]))
        finally:
            os.close(fd)
    return output


def topology_contains(value, endpoint):
    # Reuse the accepted /dir/status topology-presence gate and typed path.
    try:
        nodes = [node for dc in value['Topology']['DataCenters'] for rack in dc['Racks']
                 for node in rack['DataNodes']]
        return sum(node['Url'] == endpoint for node in nodes) == 1
    except (KeyError, TypeError):
        return False


def ready(config):
    client_ack = time.monotonic()  # Probe entry is causally after the runner Kill acknowledgement.
    last = None
    # Read-only readiness retries only, under one named 45 s/original-work bound.
    original = datetime.datetime.fromisoformat(config['deadline'].replace('Z', '+00:00')).timestamp()
    stage_deadline = min(original, time.time() + 45)
    end = time.monotonic() + max(0, stage_deadline - time.time())
    config = dict(config, deadline=datetime.datetime.fromtimestamp(
        stage_deadline, datetime.timezone.utc).isoformat())
    master = 'http://' + config['server_address'] + ':' + str(config['server_services']['master_http'])
    probe_url = assign_probe_url(config, master)  # a malformed layout refuses before any request
    while time.monotonic() < end:
        try:
            facts = services(config)
            address, ports = config['server_address'], config['server_services']
            master = 'http://' + address + ':' + str(ports['master_http'])
            leader = http_json(master + '/cluster/status', config)
            if leader.get('IsLeader', leader.get('isLeader')) is not True:
                raise ValueError('master not leader')
            topology = http_json(master + '/dir/status', config)
            volume = address + ':' + str(ports['volume_http'])
            if not topology_contains(topology, volume):
                raise ValueError('volume missing from master topology')
            http_json('http://' + address + ':' + str(ports['filer_http']) + '/', config, False)
            row = dict(status='READY', processes=facts, topology=topology, client_mono=client_ack,
                       server_write_join='NOT_OBSERVED')
            if probe_url:
                layout = config['ready_assign']
                before = layout_writables(topology, layout)
                # Warm-up answers 503, which http_json raises: retried within this bound.
                answer = http_json(probe_url, config)
                if answer.get('error') or not isinstance(answer.get('fid'), str) or not answer['fid']:
                    raise ValueError('assign probe answered without a fid')
                mono = time.monotonic()
                after = layout_writables(http_json(master + '/dir/status', config), layout)
                row['assign_probe'] = dict(fid=answer['fid'], url=probe_url, mono=mono,
                                           writables_before=before, writables_after=after,
                                           grew=bool(set(after) - set(before)))
            # When every gate (and the Assign, if any) has actually succeeded;
            # client_mono stays the probe-entry time.
            row['ready_mono'] = time.monotonic()
            return row
        except (OSError, ValueError, KeyError) as error:
            last = repr(error)
            time.sleep(min(.2, max(0, end - time.monotonic())))
    raise TimeoutError('profile readiness 45s/original-work bound: ' + str(last))


def soak_layout(config):
    """The plan's soak setup block: the cache layout plus how many volumes to grow."""
    s = config.get('soak_setup')
    if not isinstance(s, dict) or set(s) != {'collection', 'replication', 'ttl', 'grow_count'}:
        raise ValueError('soak_setup keys must be exactly collection,replication,ttl,grow_count')
    layout = {k: s[k] for k in ('collection', 'replication', 'ttl')}
    if assign_probe_url(dict(ready_assign=layout), 'http://0.0.0.0:1') is None:  # same layout rules as ready_assign
        raise ValueError('soak_setup layout')
    n = s['grow_count']
    if isinstance(n, bool) or not isinstance(n, int) or not 1 <= n <= 16:
        raise ValueError('soak_setup grow_count must be in [1, 16]')
    return layout, n


def layout_live(topology, layout):
    """Live volume ids of the layout (the subset writes go to), like layout_writables."""
    try:
        return sorted({int(v) for row in (topology['Topology']['Layouts'] or [])
                       if (row.get('collection'), row.get('replication'), row.get('ttl') or '') ==
                       (layout['collection'], layout['replication'], layout['ttl'])
                       for v in (row.get('live') or [])})
    except (KeyError, TypeError, ValueError):
        raise ValueError('topology layouts unreadable')


def bounded(config, seconds):
    """config with its deadline cut to now + seconds (never past the original)."""
    original = datetime.datetime.fromisoformat(config['deadline'].replace('Z', '+00:00')).timestamp()
    return dict(config, deadline=datetime.datetime.fromtimestamp(
        min(original, time.time() + seconds), datetime.timezone.utc).isoformat())


def soak_setup(config):
    """M02, after READY: configure the cache layout with the run's own pinned
    weed, grow exactly N volumes once, and wait until exactly N are writable
    and live (the r9 setup, without fixed hosts or roots)."""
    layout, n = soak_layout(config)
    root = private_root(config)
    facts = services(config)  # the owned master/filer/volume generations are live
    address, ports = config['server_address'], config['server_services']
    master = 'http://%s:%d' % (address, ports['master_http'])
    _, env = product_command(config, 'master')
    weed = PurePosixPath(config['release']) / 'bin' / 'weed'
    fd = os.open(str(weed), os.O_RDONLY | os.O_NOFOLLOW)
    try:
        with os.fdopen(os.dup(fd), 'rb') as stream:
            if stream_digest(stream) != config['products']['weed']:
                raise ValueError('protected product hash mismatch')
        command = 'fs.configure -locationPrefix=/%s -collection=%s -replication=%s -ttl=%s -volumeGrowthCount=1 -apply\n' % (
            layout['collection'], layout['collection'], layout['replication'], layout['ttl'])
        # Exec the hash-checked open file itself, never the path again.
        argv = ['/proc/self/fd/%d' % fd, 'shell',
                '-master=%s:%d.%d' % (address, ports['master_http'], ports['master_grpc']),
                '-filer=%s:%d.%d' % (address, ports['filer_http'], ports['filer_grpc'])]
        shell = subprocess.run(argv, input=command.encode(), capture_output=True, env=env, cwd=root,
                               pass_fds=(fd,), timeout=min(20, remaining(config)))
    finally:
        os.close(fd)
    for name, data in (('fs-configure.stdout', shell.stdout), ('fs-configure.stderr', shell.stderr)):
        with (root / name).open('xb') as stream:
            stream.write(data)
    configure = dict(rc=shell.returncode, command=command.strip(),
                     stdout_sha256=hashlib.sha256(shell.stdout).hexdigest(),
                     stderr_sha256=hashlib.sha256(shell.stderr).hexdigest())
    if shell.returncode != 0:
        raise ValueError('fs.configure failed with rc %d' % shell.returncode)
    # One grow call, no retry: a second call could grow a second batch.
    grow = http_json(master + '/vol/grow?' + urlencode(dict(layout, count=n)), bounded(config, 120))
    if grow.get('count') != n:
        raise ValueError('volume grow answered count %r, want %d' % (grow.get('count'), n))
    staged = bounded(config, 60)
    end = time.monotonic() + max(0, remaining(staged))
    last = None
    while time.monotonic() < end:
        topology = http_json(master + '/dir/status', staged)
        writable, live = layout_writables(topology, layout), layout_live(topology, layout)
        if len(writable) == n and len(live) == n:
            used = os.statvfs(root / 'volume')
            return dict(status='SOAK_SETUP', processes=facts, configure=configure, grow=dict(count=n),
                        writables=writable, live=live,
                        volume_fs_used_fraction=1 - used.f_bavail / used.f_blocks)
        last = (writable, live)
        time.sleep(0.5)
    raise TimeoutError('layout not exactly %d writable and live within 60 s: %r' % (n, last))


def collect(config):
    root = private_root(config)
    facts = services(config)
    files = []
    # Non-recursive metadata only: no dataset or secret contents, no log truncation.
    for name in ('master', 'filer', 'volume'):
        directory = root / name
        if directory.exists():
            info = directory.stat()
            files.append(dict(name=name, device=info.st_dev, inode=info.st_ino))
    fs = os.statvfs(root)
    return dict(status='COLLECTED', processes=facts, directories=files,
                filesystem=dict(blocks=fs.f_blocks, available=fs.f_bavail,
                                fragment_size=fs.f_frsize), capture=time.time(),
                resource_gate='NOT_ASSESSED_SMOKE_ONLY')


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', required=True)
    parser.add_argument('--action', choices=('preflight', 'deploy', 'ready', 'soak_setup', 'collect'), required=True)
    parser.add_argument('--role', default='')
    parser.add_argument('--replacement-volume', action='store_true')
    parser.add_argument('--replacement-filer', action='store_true')
    parser.add_argument('--replacement-master', action='store_true')
    args = parser.parse_args()
    config = json.loads(args.config)
    remaining(config)
    if args.replacement_volume and (args.action,args.role)!=('deploy','volume'):
        raise ValueError('replacement flag requires volume deploy')
    if args.replacement_filer and ((args.action,args.role)!=('deploy','filer') or args.replacement_volume):
        raise ValueError('replacement flag requires filer deploy only')
    if args.replacement_master and ((args.action,args.role)!=('deploy','master') or args.replacement_volume or args.replacement_filer):
        raise ValueError('replacement flag requires master deploy only')
    if Path('/proc/sys/kernel/random/boot_id').read_text().strip() != config['boot_id']:
        raise ValueError('host rebooted since inventory')
    if args.action == 'preflight':
        private_root(config)
        row = qualify_hardware(config, observe(config['host']))
        row['storage'] = storage(config)
        row['ports'] = free_service_ports(config)
        row['process_limits'] = Path('/proc/self/limits').read_text()
    elif args.action == 'deploy':
        launch(config, args.role, args.replacement_volume or args.replacement_filer or args.replacement_master)
        raise AssertionError('exec returned')
    elif args.action == 'ready':
        row = ready(config)
    elif args.action == 'soak_setup':
        if args.role:
            raise ValueError('soak_setup takes no role')
        row = soak_setup(config)
    else:
        row = collect(config)
    print(json.dumps(row, sort_keys=True), flush=True)


if __name__ == '__main__':
    main()
