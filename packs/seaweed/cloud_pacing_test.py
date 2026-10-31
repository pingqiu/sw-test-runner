import importlib.util
import pathlib
from unittest.mock import patch

path = pathlib.Path(__file__).with_name('cloud_batch_test.py')
spec = importlib.util.spec_from_file_location('existing_controls', path)
controls = importlib.util.module_from_spec(spec)
spec.loader.exec_module(controls)
clock = [0.0]
sleeps = []

def sleep(seconds):
    assert seconds >= 0
    sleeps.append(seconds)
    clock[0] += seconds

with patch.object(controls.batch.time, 'monotonic', side_effect=lambda: clock[0]), patch.object(controls.batch.time, 'sleep', side_effect=sleep):
    # The existing fixture patches sleep too; replace that patch only in a private
    # compiled copy below, leaving the captured source unchanged.
    source = path.read_text().replace("patch.object(batch.time,'sleep'), ", '')
    namespace = {'__file__': str(path), '__name__': 'reviewer_pacing_fixture'}
    exec(compile(source, str(path), 'exec'), namespace)
    namespace['CloudBatchTests']().exercise('ok')
assert len(sleeps) == 32, sleeps
assert clock[0] == 864.0, clock
print({'batches': len(sleeps), 'paced_seconds': clock[0], 'real_sleep': False})
