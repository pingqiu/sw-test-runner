import hashlib
import json
import os
import shutil
import subprocess
import tempfile
import threading
import time
import unittest
from pathlib import Path

import soak_sampler
import soak_stats


class Sampler(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        self.root.chmod(0o700)
        self.addCleanup(shutil.rmtree, self.root)
        self.saved = soak_sampler.INTERVAL_S
        soak_sampler.INTERVAL_S = 0.1
        self.addCleanup(setattr, soak_sampler, 'INTERVAL_S', self.saved)
        self.children = []

    def child(self):
        p = subprocess.Popen(['sleep', '30'])
        self.children.append(p)
        self.addCleanup(lambda: (p.kill(), p.wait()))
        return p

    def generation(self, name, p):
        start = soak_stats.identity(p.pid)['starttime']
        (self.root / name).write_text(json.dumps(dict(pid=p.pid, starttime=start)))

    def test_samples_original_and_replacement_until_the_horizon(self):
        volume = self.child()
        self.generation('volume-generation.json', volume)
        self.generation('master-generation.json', self.child())

        def restart():  # mid-run: the original volume exits, its replacement appears
            time.sleep(0.15)
            volume.kill()
            volume.wait()
            self.generation('volume-replacement-generation.json', self.child())
        threading.Thread(target=restart).start()
        out = soak_sampler.run(self.root, 0.4)
        rows = [json.loads(line) for line in (self.root / out['file']).read_text().splitlines()]
        self.assertEqual((out['rows'], len(rows)), (6, 6))  # ticks 0..4 plus the final sample
        self.assertTrue(rows[-1]['final'])
        self.assertEqual(out['sha256'], hashlib.sha256((self.root / out['file']).read_bytes()).hexdigest())
        last = {p['generation']: p for p in rows[-1]['processes']}
        self.assertEqual(last['volume-generation.json']['status'], 'GONE')
        self.assertEqual(last['volume-replacement-generation.json']['status'], 'OBSERVED')
        self.assertGreater(last['master-generation.json']['rss_bytes'], 0)
        self.assertIn('fds', last['master-generation.json'])

    def test_reused_pid_raises(self):
        p = self.child()
        with self.assertRaisesRegex(ValueError, 'reused'):
            soak_stats.process_sample(p.pid, soak_stats.identity(p.pid)['starttime'] + 1)

    def test_refuses_a_shared_root_or_unbounded_horizon(self):
        self.root.chmod(0o755)
        with self.assertRaisesRegex(ValueError, 'private'):
            soak_sampler.run(self.root, 1)
        self.root.chmod(0o700)
        for horizon in (0, -1, 10 ** 6):
            with self.subTest(horizon=horizon), self.assertRaisesRegex(ValueError, 'horizon'):
                soak_sampler.run(self.root, horizon)

    def test_never_overwrites_an_existing_series(self):
        (self.root / 'soak-series-m02.jsonl').write_text('')
        with self.assertRaises(FileExistsError):
            soak_sampler.run(self.root, 0.1)


if __name__ == '__main__':
    unittest.main()
