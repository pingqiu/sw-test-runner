import json
import datetime
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'nightly-regression.py'
TEMPLATE = SCRIPT.parent.parent / 'inventory/testdata/http.requested.json'


class NightlyRegression(unittest.TestCase):
    def test_reconstruction_rust_prepares_the_existing_direct_interface(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            (base / 'artifacts').mkdir()
            for name in ('weed', 'weed-volume', 'native.tar'):
                (base / 'artifacts' / name).write_bytes(b'fixture')
            (base / 'host.pub').write_text('ssh-ed25519 PUBLIC_FIXTURE')
            result = subprocess.run([sys.executable, str(SCRIPT), 'prepare', str(base),
                                     str(TEMPLATE), 'ec-reconstruction-rust', 'a' * 40],
                                    capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr.decode())
            config = json.loads((base / 'config.json').read_bytes())
            inventory = json.loads((base / 'inventory.json').read_bytes())
            self.assertTrue(config['ec_stage1'])
            self.assertTrue(config['ec_reconstruction'])
            self.assertEqual(config['ec_volume_impl'], 'rust')
            self.assertEqual(set(config['files']), {'weed', 'weed-volume'})
            self.assertEqual(inventory['target'], 'onprem')
            self.assertEqual(inventory['hosts'][0]['management']['user'], 'testdev')
            self.assertEqual(inventory['transport'], 'http')

    def test_other_selectors_keep_their_existing_config_contracts(self):
        modes = [('ec-2a', {'ec_stage1': True, 'ec_under_quorum': True}, {'weed'}),
                 ('master-writes', {'master_writes': True}, {'weed', 'weed-volume', 'native.tar'}),
                 ('c5', {'replication': '001', 'cluster_chaos_cases': ['C5-two-kill']},
                  {'weed', 'weed-volume', 'native.tar'})]
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            (base / 'artifacts').mkdir()
            for name in ('weed', 'weed-volume', 'native.tar'):
                (base / 'artifacts' / name).write_bytes(b'fixture')
            (base / 'host.pub').write_text('ssh-ed25519 PUBLIC_FIXTURE')
            for cell, flags, files in modes:
                with self.subTest(cell=cell):
                    result = subprocess.run([sys.executable, str(SCRIPT), 'prepare', str(base),
                                             str(TEMPLATE), cell, 'a' * 40], capture_output=True, timeout=10)
                    self.assertEqual(result.returncode, 0, result.stderr.decode())
                    config = json.loads((base / 'config.json').read_bytes())
                    self.assertEqual(set(config['files']), files)
                    for key, value in flags.items():
                        self.assertEqual(config[key], value)
                    self.assertNotIn('ec_reconstruction', config)


FAKE = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
p=pathlib.Path(os.environ['FAKE_STATE']); args=sys.argv[1:]; tool=pathlib.Path(sys.argv[0]).name
with (p/'commands').open('a') as f:f.write(json.dumps([tool]+args)+'\n')
if tool=='df':
 if os.environ.get('MODE')=='df-fails':print('disk read failed',file=sys.stderr);sys.exit(23)
 print('Filesystem 1024-blocks Used Available Capacity Mounted on')
 for name in args[-2:]:
  full=(os.environ.get('MODE')=='disk-nvme-full' and name=='/data/nvme') or (os.environ.get('MODE')=='disk-docker-full' and name=='/fixture-docker-root')
  used=86 if full else (85 if os.environ.get('MODE')=='boundary-85' else 50)
  print(f'fixture 100 50 50 {used}% {name}')
elif tool=='jq':print(json.loads(pathlib.Path(args[-1]).read_text())[args[-2][1:]])
elif tool=='timeout':os.execv('/usr/bin/timeout',sys.argv)
elif tool=='tar':
 d=pathlib.Path(args[args.index('-C')+1]);(d/'enterprise/seaweed-volume').mkdir(parents=True,exist_ok=True)
elif tool=='make':
 d=pathlib.Path(args[args.index('-C')+1])/'bin';d.mkdir();(d/'seaweed-cloud').write_text('controller')
elif tool=='go':
 if args==['version']:print('fixture Go')
 elif os.environ.get('MODE')=='build-fails':sys.exit(19)
 else:pathlib.Path(args[args.index('-o')+1]).write_text('weed')
elif tool=='cargo':
 d=pathlib.Path(os.environ['CARGO_TARGET_DIR'])/'release';d.mkdir(parents=True);(d/'weed-volume').write_text('volume')
elif tool=='docker':
 state=p/'containers';names=json.loads(state.read_text()) if state.exists() else []
 if args[0]=='info':
  if os.environ.get('MODE')=='info-fails':print('Docker root unavailable',file=sys.stderr);sys.exit(23)
  print('/fixture-docker-root')
 elif args[:2]==['image','inspect']:
  if os.environ.get('MODE')=='image-missing':print('No such image: qualified-local-fixture',file=sys.stderr);sys.exit(1)
  mode=os.environ.get('MODE')
  image='sha256:'+'b'*64
  if mode=='image-wrong-id':image='sha256:'+'c'*64
  if mode!='image-empty':print(image)
  if mode=='image-multiple':print(image)
 elif args[0]=='run':names.append(args[args.index('--name')+1])
 elif args[0]=='rm':
  if os.environ.get('MODE')=='rm-fails':sys.exit(23)
  names.remove(args[-1])
 elif args[0]=='ps':
  if os.environ.get('MODE')=='ps-fails':sys.exit(23)
  wanted=args[args.index('--filter')+1][7:-1];print('\n'.join(n for n in names if n==wanted))
 elif args[0]=='inspect':print('FOREIGN' if os.environ.get('MODE')=='foreign-label' else args[-1])
 elif args[0]=='cp':
  if '-ec-go:' in args[1] and os.environ.get('MODE')=='collect-fails':sys.exit(22)
  d=pathlib.Path(args[-1])
  if d.name=='result':d.mkdir();(d/'cloud-postflight.json').write_text('{"exit_code":0}')
  else:d.write_text('{}')
 elif args[0]=='exec' and '/input/runner/bin/seaweed-cloud' in args:
  if '-ec-go' in args[1] and os.environ.get('MODE')=='one-fails':sys.exit(17)
  if '-ec-go' in args[1] and os.environ.get('MODE')=='cli-timeout':sys.exit(124)
  print('{"code":"PASS"}')
 state.write_text(json.dumps(names))
'''


@unittest.skipUnless(sys.platform.startswith('linux'), 'real Bash/flock/timeout boundary')
class NightlyCommand(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bin = self.root / 'fake-bin'
        self.bin.mkdir()
        for tool in ('docker', 'df', 'tar', 'make', 'go', 'cargo', 'jq', 'timeout'):
            path = self.bin / tool
            path.write_text(FAKE)
            path.chmod(0o755)
        inbox = self.root / 'inbox'
        inbox.mkdir()
        (inbox / 'input.json').write_text(json.dumps(dict(
            mono='a' * 40, night=datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%d'),
            image='sha256:' + 'b' * 64)))
        (inbox / 'native.tar').write_bytes(b'bridge fixture')
        for name in ('mono.tar.gz', 'runner.tar'):
            (inbox / name).write_bytes(b'fixture archive')
        self.env = dict(os.environ, PATH=str(self.bin) + ':' + os.environ['PATH'], FAKE_STATE=str(self.root))

    def run_night(self, mode=''):
        return subprocess.run(['bash', str(SCRIPT.with_suffix('.sh')), str(self.root)],
                              env=dict(self.env, MODE=mode), capture_output=True, timeout=30)

    def test_full_data_disk_reports_environment_without_starting_any_cell(self):
        result = self.run_night('disk-nvme-full')
        self.assertEqual(result.returncode, 78, result.stderr.decode())
        out = self.root / ('results/' + 'a' * 40)
        self.assertEqual((out / 'CLASSIFICATION').read_text().strip(), 'ENVIRONMENT')
        self.assertEqual((out / 'TABLE.md').read_text().count('| NOT_RUN |'), 11)
        calls = [json.loads(s) for s in (self.root / 'commands').read_text().splitlines()]
        self.assertFalse(any(c[0] in ('make', 'go', 'cargo') or c[:2] == ['docker', 'run'] for c in calls))
        self.assertFalse((self.root / 'work').exists())

    def test_full_docker_disk_also_blocks_the_whole_run(self):
        result = self.run_night('disk-docker-full')
        self.assertEqual(result.returncode, 78, result.stderr.decode())
        out = self.root / ('results/' + 'a' * 40)
        self.assertEqual((out / 'CLASSIFICATION').read_text().strip(), 'ENVIRONMENT')
        self.assertEqual((out / 'TABLE.md').read_text().count('| NOT_RUN |'), 11)
        self.assertIn('/fixture-docker-root', (out / 'disk-preflight.txt').read_text())
        calls = [json.loads(s) for s in (self.root / 'commands').read_text().splitlines()]
        self.assertFalse(any(c[0] in ('make', 'go', 'cargo') or c[:2] == ['docker', 'run'] for c in calls))

    def test_failed_docker_root_inspection_is_an_environment_refusal(self):
        result = self.run_night('info-fails')
        self.assertEqual(result.returncode, 78, result.stderr.decode())
        out = self.root / ('results/' + 'a' * 40)
        self.assertEqual((out / 'CLASSIFICATION').read_text().strip(), 'ENVIRONMENT')
        self.assertEqual((out / 'TABLE.md').read_text().count('| NOT_RUN |'), 11)
        self.assertIn('Docker root unavailable', (out / 'disk-preflight.stderr').read_text())

    def test_missing_qualified_image_is_environment_and_never_builds(self):
        result = self.run_night('image-missing')
        self.assertEqual(result.returncode, 78, result.stderr.decode())
        out = self.root / ('results/' + 'a' * 40)
        self.assertEqual((out / 'CLASSIFICATION').read_text().strip(), 'ENVIRONMENT')
        self.assertEqual((out / 'EXIT').read_text().strip(), '78')
        self.assertEqual((out / 'TABLE.md').read_text().count('| NOT_RUN |'), 11)
        self.assertIn('No such image', (out / 'image-inspect.stderr').read_text())
        calls = [json.loads(s) for s in (self.root / 'commands').read_text().splitlines()]
        self.assertFalse(any(c[0] in ('make', 'go', 'cargo') or c[:2] == ['docker', 'run'] for c in calls))
        self.assertFalse((self.root / 'work').exists())

    def assert_environment_admission(self, result, diagnostic):
        self.assertEqual(result.returncode, 78, result.stderr.decode())
        out = self.root / ('results/' + 'a' * 40)
        self.assertEqual((out / 'CLASSIFICATION').read_text().strip(), 'ENVIRONMENT')
        self.assertEqual((out / 'EXIT').read_text().strip(), '78')
        self.assertEqual((out / 'TABLE.md').read_text().count('| NOT_RUN |'), 11)
        self.assertTrue((out / diagnostic).read_text().strip())
        calls = [json.loads(s) for s in (self.root / 'commands').read_text().splitlines()]
        self.assertFalse(any(c[0] in ('tar', 'make', 'go', 'cargo') or c[:2] == ['docker', 'run'] for c in calls))
        self.assertFalse((self.root / 'work').exists())

    def test_missing_runner_archive_is_environment(self):
        (self.root / 'inbox/runner.tar').unlink()
        self.assert_environment_admission(self.run_night(), 'input-preflight.stderr')

    def test_missing_native_archive_is_environment(self):
        (self.root / 'inbox/native.tar').unlink()
        self.assert_environment_admission(self.run_night(), 'input-preflight.stderr')

    def test_wrong_image_id_is_environment(self):
        self.assert_environment_admission(self.run_night('image-wrong-id'), 'image-inspect.stderr')

    def test_empty_image_id_is_environment(self):
        self.assert_environment_admission(self.run_night('image-empty'), 'image-inspect.stderr')

    def test_duplicate_image_ids_are_environment(self):
        self.assert_environment_admission(self.run_night('image-multiple'), 'image-inspect.stderr')

    def test_image_alias_is_not_a_frozen_digest(self):
        path = self.root / 'inbox/input.json'
        doc = json.loads(path.read_text())
        doc['image'] = 'qualified:latest'
        path.write_text(json.dumps(doc))
        self.assert_environment_admission(self.run_night(), 'image-inspect.stderr')

    def test_failed_disk_measurement_is_an_environment_refusal(self):
        result = self.run_night('df-fails')
        self.assertEqual(result.returncode, 78, result.stderr.decode())
        out = self.root / ('results/' + 'a' * 40)
        self.assertEqual((out / 'CLASSIFICATION').read_text().strip(), 'ENVIRONMENT')
        self.assertEqual((out / 'TABLE.md').read_text().count('| NOT_RUN |'), 11)
        self.assertIn('disk read failed', (out / 'disk-preflight.stderr').read_text())

    def test_failed_cleanup_readback_cannot_pass_or_start_the_next_cell(self):
        result = self.run_night('ps-fails')
        self.assertNotEqual(result.returncode, 0)
        output = self.root / ('results/' + 'a' * 40) / 'TABLE.md'
        self.assertTrue(output.exists(), result.stderr.decode())
        table = output.read_text()
        self.assertIn('| ec-go | FAIL | cleanup:', table)
        self.assertIn('| ec-rust | NOT_RUN |', table)

    def test_failed_cell_continues_without_retry_and_unchanged_head_skips(self):
        result = self.run_night('one-fails')
        self.assertNotEqual(result.returncode, 0, result.stderr.decode())
        output = self.root / ('results/' + 'a' * 40)
        table = (output / 'TABLE.md').read_text()
        self.assertEqual(table.count('| PASS |'), 10)
        self.assertIn('| ec-go | FAIL | cli: exit 17', table)
        self.assertEqual(json.loads((self.root / 'containers').read_text()), [])
        self.assertFalse((self.root / 'work').exists())
        commands = [s for s in (self.root / 'commands').read_text().splitlines()
                    if json.loads(s)[0] != 'jq']
        self.assertEqual(self.run_night().returncode, 0)
        self.assertEqual([s for s in (self.root / 'commands').read_text().splitlines()
                          if json.loads(s)[0] != 'jq'], commands)
        self.assertEqual((output / 'EXIT').read_text().strip(), '1')

    def test_timeout_is_failed_once_and_the_real_outer_bound_is_used(self):
        result = self.run_night('cli-timeout')
        self.assertNotEqual(result.returncode, 0)
        table = (self.root / ('results/' + 'a' * 40) / 'TABLE.md').read_text()
        self.assertIn('| ec-go | FAIL | cli: exit 124', table)
        self.assertEqual(table.count('| PASS |'), 10)
        commands = [json.loads(s) for s in (self.root / 'commands').read_text().splitlines()]
        self.assertEqual(sum(c[:3] == ['timeout', '-k5', '1650'] for c in commands), 11)

    def test_busy_flock_does_no_work(self):
        import fcntl
        with (self.root / 'lock').open('w') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.assertEqual(self.run_night().returncode, 0)
        self.assertFalse((self.root / 'commands').exists())

    def test_failed_removal_retains_work_and_blocks_future_work(self):
        self.assertNotEqual(self.run_night('rm-fails').returncode, 0)
        self.assertTrue((self.root / 'cleanup-required').exists())
        self.assertTrue((self.root / 'work').exists())
        commands = (self.root / 'commands').read_bytes()
        self.assertNotEqual(self.run_night().returncode, 0)
        self.assertEqual((self.root / 'commands').read_bytes(), commands)

    def test_failed_collection_is_named_and_does_not_stop_later_cells(self):
        self.assertNotEqual(self.run_night('collect-fails').returncode, 0)
        table = (self.root / ('results/' + 'a' * 40) / 'TABLE.md').read_text()
        self.assertIn('| ec-go | FAIL | collect:', table)
        self.assertEqual(table.count('| PASS |'), 10)

    def test_build_failure_yields_eleven_not_run_rows_and_a_failed_job(self):
        result = self.run_night('build-fails')
        self.assertNotEqual(result.returncode, 0)
        output = self.root / ('results/' + 'a' * 40)
        self.assertEqual((output / 'TABLE.md').read_text().count('| NOT_RUN |'), 11)
        self.assertEqual((output / 'EXIT').read_text().strip(), '19')
        self.assertFalse((self.root / 'work').exists())

    def test_all_pass_at_85_percent_has_a_complete_table_and_verifiable_local_checksums(self):
        result = self.run_night('boundary-85')
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        output = self.root / ('results/' + 'a' * 40)
        self.assertEqual((output / 'TABLE.md').read_text().count('| PASS |'), 11)
        self.assertEqual((output / 'EXIT').read_text().strip(), '0')
        checks = subprocess.run(['sha256sum', '-c', 'SHA256SUMS'], cwd=output,
                                capture_output=True, timeout=10)
        self.assertEqual(checks.returncode, 0, checks.stderr.decode())
        self.assertFalse((self.root / 'work').exists())

    def test_a_changed_head_does_not_allow_a_second_run_the_same_night(self):
        self.assertEqual(self.run_night().returncode, 0)
        path = self.root / 'inbox/input.json'
        doc = json.loads(path.read_bytes())
        doc['mono'] = 'b' * 40
        path.write_text(json.dumps(doc))
        self.assertEqual(self.run_night().returncode, 0)
        self.assertFalse((self.root / ('results/' + 'b' * 40)).exists())

    def test_foreign_container_label_is_never_deleted(self):
        self.assertNotEqual(self.run_night('foreign-label').returncode, 0)
        calls = [json.loads(s) for s in (self.root / 'commands').read_text().splitlines()]
        self.assertFalse(any(c[:2] == ['docker', 'rm'] for c in calls))
        self.assertTrue((self.root / 'cleanup-required').exists())


if __name__ == '__main__':
    unittest.main()
