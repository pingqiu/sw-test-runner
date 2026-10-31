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
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            root, release = base / 'run', base / 'release'
            root.mkdir(mode=0o700)
            (release / 'bin').mkdir(parents=True)
            product = release / 'bin' / 'weed'
            shutil.copyfile('/bin/true', product)
            product.chmod(0o755)
            c = dict(self.config(), root=str(root), release=str(release),
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
                short = dict(config,deadline=(datetime.datetime.now(datetime.timezone.utc)+
                                              datetime.timedelta(seconds=.3)).isoformat())
                refused = subprocess.run([sys.executable,'-I','-B',profile.__file__,
                    '--config',json.dumps(short),'--action','ready'],capture_output=True,timeout=3)
                self.assertNotEqual(refused.returncode,0)
                self.assertIn(b'listeners not owned',refused.stderr)
                generation.write_text(original)
                # RDMA CM has no TCP listener. Every actual TCP port remains mandatory.
                for missing in ('volume_http', 'volume_grpc', 'rdma_control', 'dc_control'):
                    changed = copy.deepcopy(config)
                    changed['services'][missing] = ports['rdma']  # Deliberately unbound TCP port.
                    changed['deadline'] = (datetime.datetime.now(datetime.timezone.utc)+
                                           datetime.timedelta(seconds=.3)).isoformat()
                    refused = subprocess.run([sys.executable,'-I','-B',profile.__file__,
                        '--config',json.dumps(changed),'--action','ready'],capture_output=True,timeout=3)
                    self.assertNotEqual(refused.returncode,0,missing)
                    self.assertIn(b'listeners not owned',refused.stderr,missing)
                # Active TCP and DC writer cannot substitute for the exact CM bind marker.
                original_log = (root/'volume.stdout').read_text()
                for replacement in ('', 'RDMA listener bound on 127.0.0.2:'+str(ports['rdma'])):
                    lines = [line for line in original_log.splitlines() if 'RDMA listener bound on ' not in line]
                    (root/'volume.stdout').write_text('\n'.join(lines+[replacement])+'\n')
                    short = dict(config,deadline=(datetime.datetime.now(datetime.timezone.utc)+
                                                  datetime.timedelta(seconds=.3)).isoformat())
                    refused = subprocess.run([sys.executable,'-I','-B',profile.__file__,
                        '--config',json.dumps(short),'--action','ready'],capture_output=True,timeout=3)
                    self.assertNotEqual(refused.returncode,0)
                    self.assertIn(b'RDMA CM bind not observed',refused.stderr)
                (root/'volume.stdout').write_text(original_log)
                # A live service/socket alone cannot replace the writer marker.
                (root/'volume.stdout').write_text('FIXTURE_LISTENING\n')
                short = dict(config,deadline=(datetime.datetime.now(datetime.timezone.utc)+
                                              datetime.timedelta(seconds=.3)).isoformat())
                refused = subprocess.run([sys.executable,'-I','-B',profile.__file__,
                    '--config',json.dumps(short),'--action','ready'],capture_output=True,timeout=3)
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


if __name__ == '__main__':
    unittest.main()
