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
import sys
import types
import http.client
import io
from urllib.parse import urlsplit


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


def launch(config, role):
    remaining(config)
    root = private_root(config)
    argv, env = product_command(config, role)
    if role == 'filer' and os.path.lexists(root / 'filer.sock'):
        raise ValueError('private filer socket already exists')
    if role not in config['roles']:
        raise ValueError('role not assigned to this host')
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
        (root / role).mkdir(mode=0o700, exist_ok=False)
        for sub in ('home', 'cache', 'tmp'):
            (root / sub).mkdir(mode=0o700, exist_ok=True)
        if role == 'master':
            # Select a private config instead of an unrelated /etc master.toml.
            with (root / 'master.toml').open('x') as stream:
                stream.write('[master.cache_volume_rotation]\nenabled = false\n')
        if role == 'filer':
            with (root / 'filer.toml').open('x') as stream:
                stream.write('[leveldb2]\nenabled = true\ndir = ' +
                             json.dumps(str(root / 'filer' / 'ldb2')) + '\n')
        identity = process(os.getpid())
        identity.update(executable_sha256=digest, role=role, argv=argv,
                        logs=[os.readlink('/proc/self/fd/' + str(fd)) for fd in (1, 2)])
        with (root / (role + '-generation.json')).open('x') as stream:
            json.dump(identity, stream, sort_keys=True)
        print(json.dumps(dict(event='PRODUCT_EXEC', **identity)), flush=True)
        os.chdir(root)
        remaining(config)
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
        expected = json.loads((root / (role + '-generation.json')).read_text())
        # Do not accept a live foreign executable named in an editable PID file.
        binary = 'weed-volume' if role == 'volume' else 'weed'
        if expected['executable_sha256'] != config['products'][binary] or expected['role'] != role:
            raise ValueError('service generation product mismatch')
        current = process(expected['pid'])
        qualify_process(expected, current)
        ports = owned_listeners(expected['pid'])
        if not {config['services'][p] for p in required[role]} <= ports:
            raise ValueError('service listeners not owned by exact generation: ' + role)
        facts[role] = dict(identity=current, listening_ports=sorted(ports))
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
    last = None
    # Read-only readiness retries only, under one named 45 s/original-work bound.
    original = datetime.datetime.fromisoformat(config['deadline'].replace('Z', '+00:00')).timestamp()
    stage_deadline = min(original, time.time() + 45)
    end = time.monotonic() + max(0, stage_deadline - time.time())
    config = dict(config, deadline=datetime.datetime.fromtimestamp(
        stage_deadline, datetime.timezone.utc).isoformat())
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
            return dict(status='READY', processes=facts, topology=topology,
                        server_write_join='NOT_OBSERVED')
        except (OSError, ValueError, KeyError) as error:
            last = repr(error)
            time.sleep(min(.2, max(0, end - time.monotonic())))
    raise TimeoutError('profile readiness 45s/original-work bound: ' + str(last))


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
    parser.add_argument('--action', choices=('preflight', 'deploy', 'ready', 'collect'), required=True)
    parser.add_argument('--role', default='')
    args = parser.parse_args()
    config = json.loads(args.config)
    remaining(config)
    if Path('/proc/sys/kernel/random/boot_id').read_text().strip() != config['boot_id']:
        raise ValueError('host rebooted since inventory')
    if args.action == 'preflight':
        private_root(config)
        row = qualify_hardware(config, observe(config['host']))
        row['storage'] = storage(config)
        row['ports'] = free_service_ports(config)
        row['process_limits'] = Path('/proc/self/limits').read_text()
    elif args.action == 'deploy':
        launch(config, args.role)
        raise AssertionError('exec returned')
    elif args.action == 'ready':
        row = ready(config)
    else:
        row = collect(config)
    print(json.dumps(row, sort_keys=True), flush=True)


if __name__ == '__main__':
    main()
