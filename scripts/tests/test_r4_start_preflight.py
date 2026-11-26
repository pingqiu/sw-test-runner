import importlib.util
import json
from pathlib import Path, PurePosixPath
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

import yaml

spec = importlib.util.spec_from_file_location(
    'r4_start', Path(__file__).resolve().parents[1]/'r4/start-preflight.py')
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)


class R4StartPreflight(unittest.TestCase):
    def test_leftover_child_is_a_failure(self):
        child = Mock()
        child.poll.return_value = None
        with self.assertRaisesRegex(ValueError, 'left live child'):
            smoke.stop([child])

    def test_exact_scenario_master_keeps_auth_and_audit(self):
        scenario = yaml.safe_load((Path(__file__).resolve().parents[2]/'scenarios/v3block/r4-node-loss.yaml').read_text())
        action = next(p for p in scenario['phases'] if p['name'] == 'start')['actions'][0]
        for rf in ('2','3'):
            role, argv = smoke.command(action, dict(scenario['env'],rf=rf,run_id='smoke'), PurePosixPath('/tmp/smoke'))
            self.assertEqual(role,'master')
            self.assertEqual(argv[argv.index('--expected-slots-per-volume')+1],rf)
            self.assertEqual(argv[argv.index('--lifecycle-product-loop-interval')+1],'100ms')
            self.assertIn('--tls-client-ca',argv)
            self.assertIn('--role-file',argv)
            self.assertEqual(argv[argv.index('--audit-log')+1],'/tmp/smoke/master/audit.jsonl')

    def test_no_cloud_call_when_local_start_fails(self):
        with patch.object(smoke,'smoke',side_effect=ValueError('master refused audit')), patch.object(smoke.subprocess,'call') as cloud:
            with self.assertRaisesRegex(ValueError,'master refused'):
                smoke.main(['unused','--launch-token','a'*24])
            cloud.assert_not_called()

    def test_cloud_is_called_only_after_smoke(self):
        events=[]
        with patch.object(smoke,'smoke',side_effect=lambda _:events.append('smoke')), patch.object(smoke.subprocess,'call',side_effect=lambda _:events.append('cloud') or 0):
            self.assertEqual(smoke.main(['unused','--launch-token','a'*24]),0)
        self.assertEqual(events,['smoke','cloud'])

    def test_missing_listening_or_early_exit_is_failure_and_children_are_reaped(self):
        for script,phase in [('import time; time.sleep(30)','listening'),('raise SystemExit(2)','listening')]:
            with tempfile.TemporaryDirectory() as d:
                log=Path(d)/'child.log'
                with log.open('wb') as f:
                    child=subprocess.Popen([sys.executable,'-c',script],stdout=f,stderr=subprocess.STDOUT)
                    try:
                        with self.assertRaises(ValueError):
                            smoke.watch([child],[log],[phase],.2)
                    finally:
                        smoke.stop([child])
                self.assertIsNotNone(child.poll())

    def test_listening_requires_live_process_for_whole_window(self):
        with tempfile.TemporaryDirectory() as d:
            log=Path(d)/'child.log'
            script='import time; print('+repr(json.dumps({'phase':'listening'}))+',flush=True); time.sleep(30)'
            with log.open('wb') as f:
                child=subprocess.Popen([sys.executable,'-c',script],stdout=f,stderr=subprocess.STDOUT)
                try:
                    smoke.watch([child],[log],['listening'],.3)
                finally:
                    smoke.stop([child])
            self.assertIsNotNone(child.poll())

    def test_ready_log_without_actual_listener_is_not_enough(self):
        with tempfile.TemporaryDirectory() as d:
            log=Path(d)/'child.log'
            address='127.0.0.1:'+smoke.port('127.0.0.1')
            script='import time; print('+repr(json.dumps({'phase':'listening'}))+',flush=True); time.sleep(30)'
            with log.open('wb') as f:
                child=subprocess.Popen([sys.executable,'-c',script],stdout=f,stderr=subprocess.STDOUT)
                try:
                    with self.assertRaises(OSError):
                        smoke.watch([child],[log],['listening'],.2,[address])
                finally:
                    smoke.stop([child])
            self.assertIsNotNone(child.poll())


if __name__ == '__main__':
    unittest.main()
