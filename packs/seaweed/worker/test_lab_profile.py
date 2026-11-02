import copy
import hashlib
import datetime
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import unittest

import lab_profile as profile


class HardwareChecks(unittest.TestCase):
    def setUp(self):
        self.config = dict(host='m01', boot_id='boot-a', device='mlx5_0',
                           port=1, gid_index=3, address='10.0.2.1',
                           nic='enp2s0v0', mtu=9000, active_mtu=4096,
                           pci='/sys/devices/pci0000:00/0000:01:00.1',
                           physical_pci='/sys/devices/pci0000:00/0000:01:00.0',
                           firmware='16.33.1048')
        self.raw = dict(host='m01', boot_id='boot-a', capture_start=10,
                        capture_end=11, rdma_devices={'mlx5_0': dict(
                            state='4: ACTIVE', active_mtu=4096,
                            pci=self.config['pci'], physical_pci=self.config['physical_pci'],
                            max_tx_rate_mbps=40000, chosen=dict(index=3,
                            gid='::ffff:10.0.2.1', type='RoCE v2',
                            netdev='enp2s0v0'))}, links=[dict(
                            ifname='enp2s0v0', mtu=9000,
                            flags=['UP', 'LOWER_UP'])],
                        devices_raw={'mlx5_0':'fw_ver: 16.33.1048\n'})

    def test_exact_hardware_passes_without_claiming_dc_capability(self):
        row = profile.qualify_hardware(self.config, self.raw)
        self.assertEqual(row['status'], 'OBSERVED')
        self.assertEqual(row['dc_capability'], 'NOT_PROVEN')

    def test_changed_pci_pf_or_firmware_refuses(self):
        for field in ('pci', 'physical_pci', 'firmware'):
            with self.subTest(field=field):
                row = copy.deepcopy(self.raw)
                if field == 'firmware':
                    row['devices_raw']['mlx5_0'] = 'fw_ver: 99.0.0\n'
                else:
                    row['rdma_devices']['mlx5_0'][field] += '-changed'
                with self.assertRaises(ValueError):
                    profile.qualify_hardware(self.config, row)

    def test_wrong_boot_gid_nic_mtu_or_address_refuses(self):
        edits = [('boot', lambda r: r.update(boot_id='other')),
                 ('gid', lambda r: r['rdma_devices']['mlx5_0']['chosen'].update(index=5)),
                 ('nic', lambda r: r['rdma_devices']['mlx5_0']['chosen'].update(netdev='other')),
                 ('mtu', lambda r: r['links'][0].update(mtu=1500)),
                 ('address', lambda r: r['rdma_devices']['mlx5_0']['chosen'].update(gid='::ffff:10.0.3.1')),
                 ('down', lambda r: r['links'][0].update(flags=['UP'])),
                 ('bracket', lambda r: r.update(capture_end=9))]
        for name, edit in edits:
            with self.subTest(name=name):
                row = copy.deepcopy(self.raw)
                edit(row)
                with self.assertRaises(ValueError):
                    profile.qualify_hardware(self.config, row)


class GenerationChecks(unittest.TestCase):
    def test_ready_requires_exact_live_generation_and_executable(self):
        expected = dict(pid=123, starttime=456, boot_id='boot',
                        executable_sha256=hashlib.sha256(b'product').hexdigest())
        current = dict(expected, state='S')
        profile.qualify_process(expected, current)
        for field, value in [('pid', 124), ('starttime', 457),
                             ('boot_id', 'other'), ('state', 'Z'),
                             ('executable_sha256', '0' * 64)]:
            with self.subTest(field=field):
                with self.assertRaises(ValueError):
                    profile.qualify_process(expected, dict(current, **{field: value}))


class LaunchContract(unittest.TestCase):
    def config(self):
        return dict(root='/private/run', release='/opt/protected', address='10.0.2.3',
                    server_address='10.0.2.3', services=dict(master_http=19755,
                    master_grpc=29755,filer_http=19105,filer_grpc=29105,
                    volume_http=19601,volume_grpc=29601,rdma=18980,
                    rdma_control=18982,dc_control=18983,rdma_push=18981))

    def test_single_master_explicit_and_no_forced_growth_or_rotation(self):
        argv, env = profile.product_command(self.config(), 'master')
        self.assertIn('-peers=none', argv)
        self.assertIn('-port.grpc=29755', argv)
        self.assertEqual(argv[0], '/opt/protected/bin/weed')
        self.assertNotIn('SWFS_RDMA_DC_DEVICE', env)

    @unittest.skipUnless(sys.platform == 'linux' and shutil.which('cc'), 'Linux CLI/product fixture')
    def test_filer_private_socket_preserves_outside_sentinel(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            root, release = base / 'run', base / 'release'
            root.mkdir(mode=0o700)
            (release / 'bin').mkdir(parents=True)
            product = release / 'bin' / 'weed'
            c = dict(self.config(), root=str(root), release=str(release),
                     roles=['filer'], host='m02',
                     boot_id=Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
                     deadline=(datetime.datetime.now(datetime.timezone.utc) +
                               datetime.timedelta(seconds=20)).isoformat())
            with socket.socket() as selected:
                selected.bind(('127.0.0.1', 0))
                c['services']['filer_http'] = selected.getsockname()[1]
            sentinel = Path('/tmp/seaweedfs-filer-' + str(c['services']['filer_http']) + '.sock')
            # This ELF fixture reproduces af8c's default-path unlink boundary.
            # Only its exec transport replaces the actual product binary.
            source = ('#include <string.h>\n#include <unistd.h>\n#include <fcntl.h>\n'
                      'int main(int n,char **v){const char *p=' + json.dumps(str(sentinel)) +
                      ';for(int i=1;i<n;i++)if(!strncmp(v[i],"-localSocket=",13))p=v[i]+13;'
                      'unlink(p);int f=open(p,O_WRONLY|O_CREAT|O_EXCL,0600);'
                      'if(f<0)return 2;write(f,"BOUND",5);close(f);return 0;}')
            built = subprocess.run(['cc','-x','c','-','-o',str(product)],
                                   input=source.encode(),capture_output=True,timeout=10)
            self.assertEqual(built.returncode, 0, built.stderr)
            product.chmod(0o755)
            c['products'] = {'weed':hashlib.sha256(product.read_bytes()).hexdigest()}
            with sentinel.open('xb') as stream:
                stream.write(b'FOREIGN_DO_NOT_UNLINK')
            try:
                argv = [sys.executable, '-I', '-B', profile.__file__, '--config',
                        json.dumps(c), '--action', 'deploy', '--role', 'filer']
                child = subprocess.run(argv,capture_output=True,timeout=12)
                self.assertEqual(child.returncode, 0, child.stderr)
                row = json.loads(child.stdout)
                metadata = (root / 'filer' / 'ldb2').stat()
                self.assertEqual(row['metadata_directory'], dict(device=metadata.st_dev, inode=metadata.st_ino))
                self.assertEqual(json.loads((root/'filer-generation.json').read_text())['metadata_directory'], row['metadata_directory'])
                self.assertEqual(sentinel.read_bytes(), b'FOREIGN_DO_NOT_UNLINK')
                self.assertIn('-localSocket=' + str(root / 'filer.sock'), row['argv'])
                self.assertEqual((root / 'filer.sock').read_bytes(), b'BOUND')
                root2 = base / 'run2'
                root2.mkdir(mode=0o700)
                (root2 / 'filer.sock').write_bytes(b'EXISTING')
                c['root'] = str(root2)
                argv[argv.index('--config') + 1] = json.dumps(c)
                refused = subprocess.run(argv,capture_output=True,timeout=12)
                self.assertNotEqual(refused.returncode, 0)
                self.assertEqual((root2 / 'filer.sock').read_bytes(), b'EXISTING')
            finally:
                sentinel.unlink()  # only the exact exclusive-created local CPU fixture

    def test_dc_volume_uses_declared_ports_and_device(self):
        c = dict(self.config(), device='mlx5_0', gid_index=3)
        argv, env = profile.product_command(c, 'volume')
        self.assertEqual(argv[0], '/opt/protected/bin/weed-volume')
        self.assertEqual(env['SWFS_RDMA_DC_DEVICE'], 'mlx5_0')
        self.assertEqual(env['SWFS_RDMA_DC_GID_INDEX'], '3')
        self.assertEqual(env['SWFS_RDMA_PUSH_PORT'], '18981')
        self.assertIn('--rdma.kv-transport', argv)
        with self.assertRaises(ValueError):
            profile.product_command(c, 's3')

    @unittest.skipUnless(sys.platform == 'linux', 'actual Linux fd-exec seam')
    def test_actual_cli_execs_exact_product_in_same_pid(self):
        root = self.deploy_master({})
        self.assertEqual((root / 'master.toml').read_text(), '[master.cache_volume_rotation]\nenabled = false\n')

    @unittest.skipUnless(sys.platform == 'linux', 'actual Linux fd-exec seam')
    def test_actual_cli_writes_declared_rotation(self):
        root = self.deploy_master(dict(rotation=RotationConfig.ROTATION))
        text = (root / 'master.toml').read_text()
        self.assertIn('enabled = true\ncollections = "lmcache"\n', text)
        self.assertIn('check_interval_seconds = 2\n', text)

    def deploy_master(self, extra):
        temporary = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, temporary)
        base = Path(temporary)
        root, release = base / 'run', base / 'release'
        root.mkdir(mode=0o700)
        (release / 'bin').mkdir(parents=True)
        product = release / 'bin' / 'weed'
        shutil.copyfile('/bin/true', product)
        product.chmod(0o755)
        c = dict(self.config(), **extra, root=str(root), release=str(release),
                 roles=['master'], host='m02',
                 boot_id=Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
                 products={'weed':hashlib.sha256(product.read_bytes()).hexdigest()},
                 deadline=(datetime.datetime.now(datetime.timezone.utc) +
                           datetime.timedelta(seconds=10)).isoformat())
        with subprocess.Popen([sys.executable, '-I', '-B', profile.__file__,
                               '--config', json.dumps(c), '--action', 'deploy',
                               '--role', 'master'], stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE) as child:
            out, err = child.communicate(timeout=12)
            self.assertEqual(child.returncode, 0, err)
            row = json.loads(out)
            self.assertEqual(row['pid'], child.pid)
            self.assertEqual(row['event'], 'PRODUCT_EXEC')
            self.assertIn('-peers=none', row['argv'])
            self.assertEqual(json.loads((root / 'master-generation.json').read_text())['pid'], child.pid)
        return root


class ReadinessHTTP(unittest.TestCase):
    def test_topology_presence_is_a_typed_data_node_not_arbitrary_url(self):
        endpoint = '10.0.2.3:19601'
        self.assertFalse(profile.topology_contains({'debug':{'Url':endpoint}}, endpoint))
        self.assertTrue(profile.topology_contains({'Topology':{'DataCenters':[
            {'Racks':[{'DataNodes':[{'Url':endpoint,'Volumes':0}]}]}]}}, endpoint))

    def test_slow_body_cannot_extend_original_deadline(self):
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_GET(self):
                self.send_response(200)
                self.send_header('Content-Length', '8')
                self.end_headers()
                try:
                    for byte in b'{"a": 1}':
                        self.wfile.write(bytes([byte]))
                        self.wfile.flush()
                        time.sleep(.1)
                except (OSError, BrokenPipeError):
                    pass
        with ThreadingHTTPServer(('127.0.0.1', 0), Handler) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            started = time.monotonic()
            c = dict(deadline=(datetime.datetime.now(datetime.timezone.utc) +
                              datetime.timedelta(seconds=.25)).isoformat())
            try:
                with self.assertRaises((TimeoutError, OSError)):
                    profile.http_json('http://127.0.0.1:' + str(server.server_port), c)
                self.assertLess(time.monotonic() - started, .6)
            finally:
                server.shutdown()
                thread.join(timeout=2)


@unittest.skipUnless(sys.platform == 'linux', 'actual Linux process/listener seam')
class OwnedServiceReadiness(unittest.TestCase):
    def test_actual_cli_joins_owned_generations_and_collects(self):
        def negative_probe(config):
            # Keep CLI parsing/entry checks, but start the diagnostic budget at
            # ready(), not before Python startup. Delay exceeds the old 300ms.
            bootstrap = '''import sys,time,datetime
source=sys.argv.pop(1)
namespace={'__name__':'readiness_probe','__file__':source}
exec(compile(open(source,'rb').read(),source,'exec'),namespace)
original=namespace['ready']
def entry(config):
    config=dict(config,deadline=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=.3)).isoformat())
    return original(config)
namespace['ready']=entry
time.sleep(.4)
namespace['main']()
'''
            bounded = dict(config, deadline=(datetime.datetime.now(datetime.timezone.utc)+
                                            datetime.timedelta(seconds=10)).isoformat())
            return subprocess.run([sys.executable,'-I','-B','-c',bootstrap,profile.__file__,
                '--config',json.dumps(bounded),'--action','ready'],capture_output=True,timeout=5)

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            sockets = [socket.socket() for _ in range(10)]
            try:
                for sock in sockets:
                    sock.bind(('127.0.0.1', 0))
                ports = dict(zip(('master_http','master_grpc','filer_http','filer_grpc',
                                  'volume_http','volume_grpc','rdma','rdma_control',
                                  'dc_control','rdma_push'), [s.getsockname()[1] for s in sockets]))
            finally:
                for sock in sockets:
                    sock.close()
            digest = hashlib.sha256(Path(sys.executable).read_bytes()).hexdigest()
            config = dict(root=str(root), host='m02', roles=['master','filer','volume'],
                          boot_id=Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
                          products={'weed':digest,'weed-volume':digest},
                          services=ports, server_services=ports, server_address='127.0.0.1',
                          deadline=(datetime.datetime.now(datetime.timezone.utc) +
                                    datetime.timedelta(seconds=15)).isoformat())
            children, streams = [], []
            try:
                for role in config['roles']:
                    out, err = root / (role+'.stdout'), root / (role+'.stderr')
                    streams.extend([out.open('wb'), err.open('wb')])
                    child = subprocess.Popen([sys.executable, '-B',
                        str(Path(profile.__file__).with_name('profile_service_fixture.py')),
                        json.dumps(config), role], stdout=streams[-2], stderr=streams[-1])
                    children.append(child)
                    deadline = time.monotonic() + 5
                    while b'FIXTURE_LISTENING' not in out.read_bytes():
                        if child.poll() is not None or time.monotonic() > deadline:
                            self.fail('local service fixture failed: '+err.read_text())
                        time.sleep(.02)
                    identity = profile.process(child.pid)
                    identity.update(role=role, logs=[str(out),str(err)])
                    (root/(role+'-generation.json')).write_text(json.dumps(identity))
                for action in ('ready','collect'):
                    result = subprocess.run([sys.executable,'-I','-B',profile.__file__,
                        '--config',json.dumps(config),'--action',action],
                        capture_output=True,timeout=7)
                    self.assertEqual(result.returncode,0,result.stderr)
                    record = json.loads(result.stdout)
                    self.assertEqual(record['status'],'READY' if action=='ready' else 'COLLECTED')
                    self.assertEqual(set(record['processes']),{'master','filer','volume'})
                    for role,child in zip(config['roles'],children):
                        self.assertEqual(record['processes'][role]['identity']['pid'],child.pid)
                # The declared ports are live globally, but on another PID.
                # An exact live generation alone cannot claim those listeners.
                generation = root/'volume-generation.json'
                original = generation.read_text()
                foreign = json.loads(original)
                foreign.update(profile.process(children[0].pid))
                generation.write_text(json.dumps(foreign))
                refused = negative_probe(config)
                self.assertNotEqual(refused.returncode,0)
                self.assertIn(b'listeners not owned',refused.stderr)
                generation.write_text(original)
                # RDMA CM has no TCP listener. Every actual TCP port remains mandatory.
                for missing in ('volume_http', 'volume_grpc', 'rdma_control', 'dc_control'):
                    changed = copy.deepcopy(config)
                    changed['services'][missing] = ports['rdma']  # Deliberately unbound TCP port.
                    refused = negative_probe(changed)
                    self.assertNotEqual(refused.returncode,0,missing)
                    self.assertIn(b'listeners not owned',refused.stderr,missing)
                # Active TCP and DC writer cannot substitute for the exact CM bind marker.
                original_log = (root/'volume.stdout').read_text()
                for replacement in ('', 'RDMA listener bound on 127.0.0.2:'+str(ports['rdma'])):
                    lines = [line for line in original_log.splitlines() if 'RDMA listener bound on ' not in line]
                    (root/'volume.stdout').write_text('\n'.join(lines+[replacement])+'\n')
                    refused = negative_probe(config)
                    self.assertNotEqual(refused.returncode,0)
                    self.assertIn(b'RDMA CM bind not observed',refused.stderr)
                (root/'volume.stdout').write_text(original_log)
                # A live service/socket alone cannot replace the writer marker.
                (root/'volume.stdout').write_text('FIXTURE_LISTENING\n')
                refused = negative_probe(config)
                self.assertNotEqual(refused.returncode,0)
                self.assertIn(b'DC writer not active',refused.stderr)
            finally:
                for child in children:
                    if child.poll() is None:
                        child.terminate()
                for child in children:
                    try:
                        child.wait(timeout=2)
                    except subprocess.TimeoutExpired:
                        child.kill()
                        child.wait(timeout=2)
                for stream in streams:
                    stream.close()


class RotationConfig(unittest.TestCase):
    ROTATION = dict(collections='lmcache', high_watermark=0.3, low_watermark=0.15,
                    min_age_minutes=0, max_volumes_per_cycle=1, min_remaining_volumes=4,
                    check_interval_seconds=2)

    def test_without_rotation_block_the_s0b_profile_is_unchanged(self):
        self.assertEqual(profile.master_toml({}), '[master.cache_volume_rotation]\nenabled = false\n')

    def test_declared_rotation_is_written_exactly(self):
        text = profile.master_toml(dict(rotation=self.ROTATION))
        self.assertEqual(text.splitlines(), [
            '[master.cache_volume_rotation]', 'enabled = true', 'collections = "lmcache"',
            'high_watermark = 0.3', 'low_watermark = 0.15', 'min_age_minutes = 0',
            'max_volumes_per_cycle = 1', 'min_remaining_volumes = 4', 'check_interval_seconds = 2'])

    def test_malformed_rotation_refuses(self):
        bad = [dict(self.ROTATION, extra=1), {k: v for k, v in self.ROTATION.items() if k != 'collections'},
               dict(self.ROTATION, collections='lm"cache'), dict(self.ROTATION, collections='a\nenabled = false'),
               dict(self.ROTATION, low_watermark=0.3), dict(self.ROTATION, high_watermark=1.0),
               dict(self.ROTATION, min_age_minutes=True), dict(self.ROTATION, check_interval_seconds=0),
               dict(self.ROTATION, max_volumes_per_cycle='1'), 'enabled',
               # Accepted by r1 but rejected or dropped by the product (claude03 P2-1):
               dict(self.ROTATION, min_remaining_volumes=0), dict(self.ROTATION, check_interval_seconds=2**63),
               dict(self.ROTATION, check_interval_seconds=86401), dict(self.ROTATION, min_age_minutes=10**6 + 1),
               dict(self.ROTATION, max_volumes_per_cycle=0), dict(self.ROTATION, min_remaining_volumes=10**6 + 1)]
        for rotation in bad:
            with self.subTest(rotation=rotation), self.assertRaises(ValueError):
                profile.master_toml(dict(rotation=rotation))

    def test_boundary_rotation_is_accepted(self):
        for edge in (dict(min_age_minutes=0, max_volumes_per_cycle=1, min_remaining_volumes=1, check_interval_seconds=1),
                     dict(min_age_minutes=10**6, max_volumes_per_cycle=10**6, min_remaining_volumes=10**6,
                          check_interval_seconds=86400)):
            with self.subTest(edge=edge):
                self.assertIn('enabled = true', profile.master_toml(dict(rotation=dict(self.ROTATION, **edge))))

    @unittest.skipUnless(sys.platform == 'linux', 'actual Linux fd-exec seam')
    def test_malformed_rotation_refuses_before_any_effect(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            root, release = base / 'run', base / 'release'
            root.mkdir(mode=0o700)
            (release / 'bin').mkdir(parents=True)
            product = release / 'bin' / 'weed'
            shutil.copyfile('/bin/true', product)
            product.chmod(0o755)
            c = dict(LaunchContract().config(), rotation=dict(self.ROTATION, min_remaining_volumes=0),
                     root=str(root), release=str(release), roles=['master'], host='m02',
                     boot_id=Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
                     products={'weed': hashlib.sha256(product.read_bytes()).hexdigest()},
                     deadline=(datetime.datetime.now(datetime.timezone.utc) +
                               datetime.timedelta(seconds=10)).isoformat())
            result = subprocess.run([sys.executable, '-I', '-B', profile.__file__, '--config', json.dumps(c),
                                     '--action', 'deploy', '--role', 'master'], capture_output=True, timeout=12)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(b'min_remaining_volumes', result.stderr)
            self.assertNotIn(b'PRODUCT_EXEC', result.stdout)
            self.assertEqual(sorted(p.name for p in root.iterdir()), [], 'no dir, config or generation file')


@unittest.skipUnless(sys.platform == 'linux', 'actual Linux fd-exec seam')
class MasterReplacementState(unittest.TestCase):
    """R5: a replacement master reuses the original's retained state, never a
    recreated or swapped one, and never while the original is live."""

    def setUp(self):
        temporary = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, temporary)
        base = Path(temporary)
        self.root, release = base / 'run', base / 'release'
        self.root.mkdir(mode=0o700)
        (release / 'bin').mkdir(parents=True)
        product = release / 'bin' / 'weed'
        shutil.copyfile('/bin/true', product)  # exits at once: the original is settled
        product.chmod(0o755)
        self.config = dict(LaunchContract().config(), rotation=RotationConfig.ROTATION,
                           root=str(self.root), release=str(release), roles=['master'], host='m02',
                           boot_id=Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
                           products={'weed': hashlib.sha256(product.read_bytes()).hexdigest()},
                           deadline=(datetime.datetime.now(datetime.timezone.utc) +
                                     datetime.timedelta(seconds=10)).isoformat())
        first = self.deploy(False)
        self.assertEqual(first.returncode, 0, first.stderr)
        (self.root / 'master' / 'log').write_bytes(b'raft')  # what the product leaves behind

    def deploy(self, replacement):
        argv = [sys.executable, '-I', '-B', profile.__file__, '--config', json.dumps(self.config),
                '--action', 'deploy', '--role', 'master'] + (['--replacement-master'] if replacement else [])
        return subprocess.run(argv, capture_output=True, timeout=12)

    def refused(self, reason):
        result = self.deploy(True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(reason.encode(), result.stderr)
        self.assertNotIn(b'PRODUCT_EXEC', result.stdout)
        self.assertFalse((self.root / 'master-replacement-generation.json').exists())

    def test_retained_state_is_replaced_without_rewriting_config(self):
        toml = (self.root / 'master.toml').stat()
        result = self.deploy(True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(b'PRODUCT_EXEC', result.stdout)
        again = (self.root / 'master.toml').stat()
        self.assertEqual((again.st_ino, again.st_mtime_ns), (toml.st_ino, toml.st_mtime_ns))
        record = json.loads((self.root / 'master-replacement-generation.json').read_text())
        original = json.loads((self.root / 'master-generation.json').read_text())
        self.assertEqual(record['master_state'], original['master_state'])

    def test_mdir_recreated_with_copied_bytes_is_refused(self):
        (self.root / 'master').rename(self.root / 'master.old')
        shutil.copytree(self.root / 'master.old', self.root / 'master')
        (self.root / 'master').chmod(0o700)
        self.refused('continuity differs')

    def test_mdir_deleted_and_recreated_with_same_inode_is_refused(self):
        # ext4 hands the freed inode straight back, so dev/inode alone passes this.
        saved = {p.name: p.read_bytes() for p in (self.root / 'master').iterdir()}
        before = (self.root / 'master').lstat().st_ino
        shutil.rmtree(self.root / 'master')
        (self.root / 'master').mkdir(mode=0o700)
        for name, data in saved.items():
            (self.root / 'master' / name).write_bytes(data)
        if (self.root / 'master').lstat().st_ino != before:
            self.skipTest('this filesystem did not reuse the inode')
        self.refused('continuity differs')

    def test_missing_state_marker_is_refused(self):
        (self.root / 'master' / profile.STATE_MARKER).unlink()
        self.refused(profile.STATE_MARKER)

    def test_config_recreated_with_identical_bytes_is_refused(self):
        data = (self.root / 'master.toml').read_bytes()
        (self.root / 'master.toml').unlink()
        (self.root / 'master.toml').write_bytes(data)
        self.refused('continuity differs')

    def test_symlinked_config_is_refused(self):
        (self.root / 'master.toml').rename(self.root / 'real.toml')
        (self.root / 'master.toml').symlink_to(self.root / 'real.toml')
        self.refused('Too many levels of symbolic links')

    def test_still_live_original_is_refused(self):
        path = self.root / 'master-generation.json'
        record = json.loads(path.read_text())
        me = profile.process(os.getpid())  # a live process stands in for the original
        record.update(pid=me['pid'], starttime=me['starttime'])
        path.write_text(json.dumps(record))
        self.refused('still live')

    def test_empty_state_dir_is_refused(self):
        (self.root / 'master' / 'log').unlink()
        self.refused('state dir is empty')

    def swap_after_check(self, call):
        """Run the replacement deploy with check_master_state wrapped so that the
        mdir is deleted and recreated (same bytes) right after check `call` passes."""
        bootstrap = '''import sys,shutil,os
source=sys.argv.pop(1); call=int(sys.argv.pop(1))
ns={'__name__':'swap_probe','__file__':source}
exec(compile(open(source,'rb').read(),source,'exec'),ns)
original=ns['check_master_state']; count=[0]
def wrapped(root,old,config):
    original(root,old,config); count[0]+=1
    if count[0]==call:
        m=root/'master'; saved={p.name:p.read_bytes() for p in m.iterdir()}
        shutil.rmtree(m); m.mkdir(mode=0o700)
        for n,b in saved.items(): (m/n).write_bytes(b)
ns['check_master_state']=wrapped
ns['main']()
'''
        argv = [sys.executable, '-I', '-B', '-c', bootstrap, profile.__file__, str(call), '--config',
                json.dumps(self.config), '--action', 'deploy', '--role', 'master', '--replacement-master']
        return subprocess.run(argv, capture_output=True, timeout=12)

    def test_swap_after_first_check_is_refused_before_publication(self):
        result = self.swap_after_check(1)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b'continuity differs', result.stderr)
        self.assertNotIn(b'PRODUCT_EXEC', result.stdout)
        self.assertFalse((self.root / 'master-replacement-generation.json').exists())

    def test_swap_after_publication_is_refused_before_exec(self):
        result = self.swap_after_check(2)
        self.assertNotEqual(result.returncode, 0, 'the product (/bin/true) must never be exec-ed')
        self.assertIn(b'continuity differs', result.stderr)
        record = json.loads((self.root / 'master-replacement-generation.json').read_text())
        original = json.loads((self.root / 'master-generation.json').read_text())
        self.assertEqual(record['master_state'], original['master_state'], 'the original join, never a fresh one')

    def test_services_reads_the_replacement_master_generation(self):
        self.assertEqual(self.deploy(True).returncode, 0)
        replacement = json.loads((self.root / 'master-replacement-generation.json').read_text())
        seen = []
        saved = (profile.process, profile.qualify_process, profile.owned_listeners)
        profile.process = lambda pid: (seen.append(pid), dict(pid=pid, starttime=1, boot_id='b', executable_sha256='x'))[1]
        profile.qualify_process = lambda expected, current: None
        profile.owned_listeners = lambda pid: set(self.config['services'].values())
        try:
            facts = profile.services(dict(self.config, roles=['master']))
        finally:
            profile.process, profile.qualify_process, profile.owned_listeners = saved
        self.assertEqual((seen, facts['master']['identity']['pid']), ([replacement['pid']], replacement['pid']))

    def test_changed_rotation_config_is_refused(self):
        self.config['rotation'] = dict(RotationConfig.ROTATION, check_interval_seconds=3)
        self.refused('replacement master config differs')


@unittest.skipUnless(sys.platform == 'linux', 'Linux fd exec of the pinned product')
class SoakCollect(unittest.TestCase):
    """M02 rotation evidence from this run's own master generations only."""

    def setUp(self):
        base = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, base)
        self.root = base / 'run'
        for d in ('tmp', 'volume'):
            (self.root / d).mkdir(parents=True)
        self.root.chmod(0o700)
        line = 'I1102 00:10:{0:02d}.0 rotation.go:1] rotated non-replicated cache Volume {1} collection={2} size=33554432\n'
        (self.root / 'master.stderr').write_text(line.format(1, 2, 'lmcache') + line.format(2, 5, 'other') + 'noise\n')
        (self.root / 'master.stdout').write_text('')
        (self.root / 'tmp' / 'weed.m02.testdev.log.INFO.20261102-000000.4242').write_text(line.format(3, 4, 'lmcache'))
        (self.root / 'tmp' / 'weed.m02.testdev.log.INFO.20261102-000000.999').write_text(line.format(4, 6, 'lmcache'))  # foreign pid
        (self.root / 'master-replacement.stderr').write_text(line.format(5, 9, 'lmcache'))
        self.gen('master-generation.json', 4242, ['master.stdout', 'master.stderr'])
        self.gen('master-replacement-generation.json', 4343, ['master-replacement.stderr'])
        for name in ('lmcache_3.dat', 'lmcache_x_7.dat', 'other_5.dat'):
            (self.root / 'volume' / name).write_bytes(b'')
        self.config = dict(root=str(self.root), soak_setup=dict(collection='lmcache', replication='000', ttl='2h', grow_count=4))

    def gen(self, name, pid, logs):
        (self.root / name).write_text(json.dumps(dict(pid=pid, logs=[str(self.root / x) for x in logs])))

    def test_rotations_of_this_runs_master_generations_and_dat_ids(self):
        row = profile.soak_collect(self.config)
        self.assertEqual([r['vid'] for r in row['rotated']], [2, 4, 9])  # not 5 (other), not 6 (foreign pid)
        self.assertEqual(row['dat_vids'], [3, 7])
        self.assertEqual(len(row['sources']), 4)

    def test_a_foreign_symlinked_or_unbounded_log_fails_closed(self):
        (self.root / 'master.stderr').rename(self.root / 'real.stderr')
        (self.root / 'master.stderr').symlink_to(self.root / 'real.stderr')
        with self.assertRaises((ValueError, OSError)):
            profile.soak_collect(self.config)
        self.setUp()
        self.gen('master-generation.json', 4242, ['../outside.log'])
        with self.assertRaisesRegex(ValueError, 'outside'):
            profile.soak_collect(self.config)
        self.setUp()
        saved = profile.LOG_BOUND
        profile.LOG_BOUND = 10
        self.addCleanup(setattr, profile, 'LOG_BOUND', saved)
        with self.assertRaisesRegex(ValueError, 'bound'):
            profile.soak_collect(self.config)

    def test_no_master_generation_fails_closed(self):
        (self.root / 'master-generation.json').unlink()
        (self.root / 'master-replacement-generation.json').unlink()
        with self.assertRaisesRegex(ValueError, 'no master generation'):
            profile.soak_collect(self.config)


class SoakSetup(unittest.TestCase):
    """soak_setup against a fake master and a fake pinned weed product."""
    SETUP = dict(collection='lmcache', replication='000', ttl='2h', grow_count=4)

    def build(self, shell_rc=0, grow_count=None, settle=True, grow_delay=0):
        base = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, base)
        self.root, release = base / 'run', base / 'release'
        (self.root / 'volume').mkdir(parents=True, mode=0o700)
        self.root.chmod(0o700)
        (release / 'bin').mkdir(parents=True)
        product = release / 'bin' / 'weed'
        self.record = base / 'shell-record'
        product.write_text('#!/bin/sh\nprintf "%%s\\n" "$@" > %s.argv\ncat > %s.stdin\necho configured\nexit %d\n'
                           % (self.record, self.record, shell_rc))
        product.chmod(0o755)
        self.calls, state = [], dict(grown=False)
        n = self.SETUP['grow_count']
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_GET(self):
                self_calls.append(self.path)
                body = {}
                if self.path.startswith('/vol/grow?'):
                    time.sleep(grow_delay)  # a real master answers only after allocating
                    state['grown'] = True
                    body = dict(count=n if grow_count is None else grow_count)
                elif self.path == '/dir/status':
                    ids = list(range(1, n + 1)) if state['grown'] and settle else []
                    body = dict(Topology=dict(Layouts=[dict(collection='lmcache', replication='000', ttl='2h',
                                                           writables=ids, live=ids)]))
                raw = json.dumps(body).encode()
                self.send_response(200)
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
        self_calls = self.calls
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        port = server.server_address[1]
        services = dict(master_http=port, master_grpc=port + 1, filer_http=port + 2, filer_grpc=port + 3,
                        volume_http=port, volume_grpc=port, rdma=1, rdma_control=1, dc_control=1, rdma_push=1)
        original = profile.services
        profile.services = lambda config: {'master': {}}
        self.addCleanup(setattr, profile, 'services', original)
        return dict(LaunchContract().config(), root=str(self.root), release=str(release), roles=['master', 'filer', 'volume'],
                    address='127.0.0.1', server_address='127.0.0.1', services=services, server_services=services,
                    products={'weed': hashlib.sha256(product.read_bytes()).hexdigest()}, soak_setup=dict(self.SETUP),
                    deadline=(datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=30)).isoformat())

    def test_configures_grows_once_and_waits_for_exactly_n(self):
        config = self.build()
        row = profile.soak_setup(config)
        port = config['server_services']['master_http']
        self.assertEqual((row['status'], row['writables'], row['live'], row['configure']['rc']), ('SOAK_SETUP', [1, 2, 3, 4], [1, 2, 3, 4], 0))
        self.assertEqual(Path(str(self.record) + '.stdin').read_text(),
                         'fs.configure -locationPrefix=/lmcache -collection=lmcache -replication=000 -ttl=2h -volumeGrowthCount=1 -apply\n')
        self.assertEqual(Path(str(self.record) + '.argv').read_text().split(),
                         ['shell', '-master=127.0.0.1:%d.%d' % (port, port + 1), '-filer=127.0.0.1:%d.%d' % (port + 2, port + 3)])
        self.assertEqual([c for c in self.calls if c.startswith('/vol/grow')],
                         ['/vol/grow?collection=lmcache&replication=000&ttl=2h&count=4'])
        self.assertEqual((self.root / 'fs-configure.stdout').read_text(), 'configured\n')

    def test_a_grow_slower_than_the_2_s_read_cap_completes(self):
        # 11-02 parity run: an 8-volume grow took ~10 s and the 2 s per-read cap raised TimeoutError.
        row = profile.soak_setup(self.build(grow_delay=3.5))
        self.assertEqual((row['status'], row['grow']), ('SOAK_SETUP', dict(count=4)))

    def test_a_grow_past_the_work_deadline_still_times_out(self):
        config = self.build(grow_delay=6)
        config['deadline'] = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=3)).isoformat()
        with self.assertRaises(TimeoutError):
            profile.soak_setup(config)

    def test_failed_configure_refuses_before_any_growth(self):
        config = self.build(shell_rc=3)
        with self.assertRaisesRegex(ValueError, 'rc 3'):
            profile.soak_setup(config)
        self.assertFalse(any(c.startswith('/vol/grow') for c in self.calls))

    def test_grow_count_mismatch_refuses(self):
        with self.assertRaisesRegex(ValueError, 'count'):
            profile.soak_setup(self.build(grow_count=3))

    def test_layout_never_reaching_n_times_out(self):
        config = self.build(settle=False)
        config['deadline'] = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=2)).isoformat()
        with self.assertRaises(TimeoutError):
            profile.soak_setup(config)

    def test_changed_product_bytes_refuse_before_exec(self):
        config = self.build()
        config['products'] = dict(weed='0' * 64)
        with self.assertRaisesRegex(ValueError, 'hash'):
            profile.soak_setup(config)
        self.assertFalse(Path(str(self.record) + '.argv').exists())

    def test_malformed_setup_block_refuses(self):
        config = self.build()
        for bad in (dict(self.SETUP, extra=1), dict(self.SETUP, grow_count=0), dict(self.SETUP, grow_count=17),
                    dict(self.SETUP, grow_count=True), dict(self.SETUP, collection='a b'), dict(self.SETUP, ttl='2h&x=1')):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                profile.soak_setup(dict(config, soak_setup=bad))
        self.assertEqual(self.calls, [])


class AssignReadiness(unittest.TestCase):
    """ready() against a local master that refuses Assign until its warm-up ends."""
    LAYOUT = dict(collection='lmcache', replication='000', ttl='2h')

    def serve(self, warmup, assign_body=None, writables=(5,), grown=()):
        calls = []
        warm_end = time.monotonic() + warmup
        def layouts():
            # The probe "grows" the listed ids once it has been answered.
            assigned = any(p.startswith('/dir/assign?') for p in calls[:-1]) and time.monotonic() >= warm_end
            ids = list(writables) + (list(grown) if assigned else [])
            return [dict(collection='lmcache', replication='000', ttl='2h', writables=ids),
                    dict(collection='other', replication='000', ttl='', writables=[77])]
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_GET(self):
                calls.append(self.path)
                status, body = 200, {}
                if self.path == '/cluster/status':
                    body = dict(IsLeader=True)
                elif self.path == '/dir/status':
                    body = dict(Topology=dict(DataCenters=[dict(Racks=[dict(DataNodes=[
                        dict(Url='127.0.0.1:' + str(port), Volumes=1)])])], Layouts=layouts()))
                elif self.path.startswith('/dir/assign?'):
                    if time.monotonic() < warm_end:
                        status, body = 503, dict(error='master is warming up, topology is still loading')
                    else:
                        body = assign_body if assign_body is not None else dict(fid='5,01637037d6', count=1)
                raw = json.dumps(body).encode()
                self.send_response(status)
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        port = server.server_address[1]
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        ports = dict(master_http=port, volume_http=port, filer_http=port)
        config = dict(server_address='127.0.0.1', server_services=ports,
                      deadline=(datetime.datetime.now(datetime.timezone.utc) +
                                datetime.timedelta(seconds=4)).isoformat())
        original = profile.services
        profile.services = lambda config: {}
        self.addCleanup(setattr, profile, 'services', original)
        return config, calls

    def test_ready_waits_for_an_assign_after_warmup(self):
        config, calls = self.serve(warmup=1.0)
        start = time.monotonic()
        row = profile.ready(dict(config, ready_assign=self.LAYOUT))
        self.assertGreaterEqual(time.monotonic() - start, 1.0)
        self.assertEqual(row['assign_probe']['fid'], '5,01637037d6')
        assigns = [p for p in calls if p.startswith('/dir/assign?')]
        self.assertGreater(len(assigns), 1, 'warm-up refusals must have been retried')
        self.assertEqual(assigns[0], '/dir/assign?collection=lmcache&replication=000&ttl=2h&count=1')
        probe = row['assign_probe']
        self.assertEqual((probe['writables_before'], probe['writables_after'], probe['grew']), ([5], [5], False))
        # The all-gates-passed time, after the successful Assign; client_mono is the probe entry.
        self.assertTrue(row['client_mono'] <= probe['mono'] <= row['ready_mono'])

    def test_probe_records_growth_on_a_layout_without_writables(self):
        # /dir/assign grows a volume when the layout has none writable; say so.
        config, calls = self.serve(warmup=0, writables=(), grown=(9,))
        probe = profile.ready(dict(config, ready_assign=self.LAYOUT))['assign_probe']
        self.assertEqual((probe['writables_before'], probe['writables_after'], probe['grew']), ([], [9], True))

    def test_control_without_layout_reports_ready_during_warmup(self):
        # Today's gate: leader + topology passes while Assign is still refused.
        config, calls = self.serve(warmup=3.0)
        row = profile.ready(config)
        self.assertNotIn('assign_probe', row)
        self.assertFalse(any(p.startswith('/dir/assign') for p in calls))

    def test_an_assign_answer_without_fid_is_not_ready(self):
        for body in (dict(error='no writable volumes'), dict(fid='', count=1),
                     dict(fid='5,01637037d6', error='no writable volumes')):
            with self.subTest(body=body):
                config, calls = self.serve(warmup=0, assign_body=body)
                config = dict(config, deadline=(datetime.datetime.now(datetime.timezone.utc) +
                                                datetime.timedelta(seconds=1)).isoformat())
                with self.assertRaises(TimeoutError):
                    profile.ready(dict(config, ready_assign=self.LAYOUT))

    def test_malformed_layout_refuses_before_any_request(self):
        config, calls = self.serve(warmup=0)
        for layout in (dict(self.LAYOUT, extra='x'), dict(self.LAYOUT, collection='a&b'),
                       dict(self.LAYOUT, replication='00'), dict(self.LAYOUT, ttl='2h&count=9'),
                       dict(self.LAYOUT, ttl=2)):
            with self.subTest(layout=layout), self.assertRaises(ValueError):
                profile.ready(dict(config, ready_assign=layout))
        self.assertEqual(calls, [])


class VolumeNofile(unittest.TestCase):
    """K8: the volume's RDMA connection budget is soft RLIMIT_NOFILE/8; the
    product unit runs it with LimitNOFILE=1048576, the lab shell with 1024."""

    def test_absent_is_unchanged_and_malformed_refuses(self):
        self.assertIsNone(profile.volume_nofile({}))
        self.assertEqual(profile.volume_nofile(dict(volume_nofile=4096)), 4096)
        for bad in (None, True, '1048576', 4096.0, 1023, 1048577, -1, [4096]):
            with self.assertRaises(ValueError, msg=repr(bad)):
                profile.volume_nofile(dict(volume_nofile=bad))

    def deploy_volume(self, extra, hard=None):
        temporary = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, temporary)
        base = Path(temporary)
        root, release = base / 'run', base / 'release'
        root.mkdir(mode=0o700)
        (release / 'bin').mkdir(parents=True)
        product = release / 'bin' / 'weed-volume'
        # The exec'd "volume" records the open-file limits it was started with.
        source = ('#include <stdio.h>\n#include <sys/resource.h>\n'
                  'int main(void){struct rlimit r;getrlimit(RLIMIT_NOFILE,&r);'
                  'FILE *f=fopen("nofile","wx");if(!f)return 2;'
                  'fprintf(f,"%llu %llu",(unsigned long long)r.rlim_cur,(unsigned long long)r.rlim_max);'
                  'return fclose(f)?3:0;}')
        built = subprocess.run(['cc', '-x', 'c', '-', '-o', str(product)],
                               input=source.encode(), capture_output=True, timeout=20)
        self.assertEqual(built.returncode, 0, built.stderr)
        product.chmod(0o755)
        c = dict(LaunchContract.config(self), **extra, root=str(root), release=str(release),
                 roles=['volume'], host='m02', device='mlx5_0', gid_index=3,
                 boot_id=Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
                 products={'weed-volume': hashlib.sha256(product.read_bytes()).hexdigest()},
                 deadline=(datetime.datetime.now(datetime.timezone.utc) +
                           datetime.timedelta(seconds=10)).isoformat())
        def limit():
            import resource
            if hard is not None:
                resource.setrlimit(resource.RLIMIT_NOFILE, (1024, hard))
        child = subprocess.run([sys.executable, '-I', '-B', profile.__file__, '--config', json.dumps(c),
                                '--action', 'deploy', '--role', 'volume'],
                               capture_output=True, timeout=15, preexec_fn=limit)
        return root, child

    def limits(self, root):
        return tuple(int(v) for v in (root / 'nofile').read_text().split())

    @unittest.skipUnless(sys.platform == 'linux' and shutil.which('cc'), 'Linux CLI/product fixture')
    def test_actual_cli_runs_the_volume_with_the_declared_soft_limit(self):
        import resource
        hard = resource.getrlimit(resource.RLIMIT_NOFILE)[1]
        want = min(hard, profile.PRODUCT_NOFILE)
        if want <= 1024:
            self.skipTest('need a hard open-file limit above 1024')
        # The child starts at soft 1024 (hard unchanged), as a lab shell does,
        # so the raise is observable even where soft already equals hard (CI).
        root, child = self.deploy_volume(dict(volume_nofile=want), hard=hard)
        self.assertEqual(child.returncode, 0, child.stderr)
        self.assertEqual(self.limits(root), (want, hard))
        row = json.loads((root / 'volume-generation.json').read_text())
        self.assertEqual(row['nofile'], dict(soft=want, hard=hard))

    @unittest.skipUnless(sys.platform == 'linux' and shutil.which('cc'), 'Linux CLI/product fixture')
    def test_actual_cli_without_the_field_keeps_the_inherited_limit(self):
        import resource
        root, child = self.deploy_volume({})
        self.assertEqual(child.returncode, 0, child.stderr)
        self.assertEqual(self.limits(root), resource.getrlimit(resource.RLIMIT_NOFILE))
        self.assertNotIn('nofile', json.loads((root / 'volume-generation.json').read_text()))

    @unittest.skipUnless(sys.platform == 'linux' and shutil.which('cc'), 'Linux CLI/product fixture')
    def test_explicit_null_refuses_before_any_effect(self):
        root, child = self.deploy_volume(dict(volume_nofile=None))
        self.assertNotEqual(child.returncode, 0)
        self.assertIn(b'volume_nofile must be an integer', child.stdout + child.stderr)
        self.assertEqual(sorted(p.name for p in root.iterdir()), [])

    @unittest.skipUnless(sys.platform == 'linux' and shutil.which('cc'), 'Linux CLI/product fixture')
    def test_above_the_hard_limit_refuses_before_any_effect(self):
        root, child = self.deploy_volume(dict(volume_nofile=8192), hard=4096)
        self.assertNotEqual(child.returncode, 0)
        self.assertIn(b'exceeds the hard limit 4096', child.stdout + child.stderr)
        self.assertEqual(sorted(p.name for p in root.iterdir()), [])


if __name__ == '__main__':
    unittest.main()
