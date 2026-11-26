"""plc_judge.py: the controller-side judge of one power-loss run (the platform judges; the exec pack only executes).

python3 -I plc_judge.py PACKET RUN_ROOT RUN OUT_JSON

PACKET is the sealed power-loss packet (its powerloss.py and drill/powerloss-fs.json decide; nothing here
re-implements a verdict). RUN_ROOT holds lout/<fs>/plc/<case>/ (the produced cases) and out/info.txt (the
run's one runtime metadata source, already verified against the run's hashed evidence). RUN is the run id the
cases must carry. The verdict is written to OUT_JSON whatever it is.

Exit 0 only for PASS; 1 for FAIL; 2 for UNKNOWN or any error. A missing packet file, policy or metadata is
UNKNOWN, never PASS.
"""
import json
import runpy
import sys
from pathlib import Path

# The four sealed cases of a counted run; judge_cases refuses any other set.
CASES = ['pl-flushed-exact', 'pl-nonprefix', 'pl-quiet-certified', 'pl-oracle-control']
EXIT = {'PASS': 0, 'FAIL': 1}


def judge(packet, run_root, run):
    packet, run_root = Path(packet), Path(run_root)
    module = runpy.run_path(str(packet/'powerloss.py'))
    return module['judge_run'](run_root, run, list(CASES), packet/'drill'/'powerloss-fs.json', run_root/'out'/'info.txt')


def main(argv):
    if len(argv) != 5:
        print(__doc__.strip().splitlines()[2], file=sys.stderr)
        return 2
    _, packet, run_root, run, out = argv
    try:
        result = judge(packet, run_root, run)
    except Exception as error:  # an unreadable packet is UNKNOWN, never PASS
        result = {'verdict': 'UNKNOWN', 'filesystems': {}, 'errors': ['judge could not run: %s' % error]}
    if not isinstance(result, dict) or result.get('verdict') not in ('PASS', 'FAIL', 'UNKNOWN'):
        result = {'verdict': 'UNKNOWN', 'filesystems': {}, 'errors': ['judge returned no verdict'], 'raw': repr(result)[:2000]}
    Path(out).write_text(json.dumps(result, indent=1, sort_keys=True) + '\n', encoding='utf-8')
    print('VERDICT %s' % result['verdict'])
    return EXIT.get(result['verdict'], 2)


if __name__ == '__main__':
    sys.exit(main(sys.argv))
