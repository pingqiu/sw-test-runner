"""Explicit local boundary control; no Azure or import-time effects."""
import argparse, hashlib, json, pathlib, subprocess

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--argv-file', required=True)
    ap.add_argument('--wrapper', required=True)
    ap.add_argument('--scratch', required=True)
    # Defaults reproduce the original audit (wrapper e5291aa8 via cmd.exe -> az.cmd). For another wrapper, pin its
    # sha256 and name the exact executable bytes it runs; only those are replaced, by --fake ({win} = scratch dir).
    ap.add_argument('--wrapper-sha256', default='e5291aa8891aa06beee9eb0b868e48bf11d6b9a4b42f6b2db5519597feebae3f')
    ap.add_argument('--target', default=r'C:\PROGRA~1\MIA713~1\Azure\CLI2\wbin\az.cmd ')
    ap.add_argument('--fake', default=r'{win}\fake-az.cmd ')
    ap.add_argument('--expect-old-query', choices=['broken', 'intact'], default='broken')
    a = ap.parse_args()
    scratch = pathlib.Path(a.scratch)
    scratch.mkdir(exist_ok=False)
    windows = subprocess.check_output(['wslpath', '-w', str(scratch)], text=True).strip()
    assert not any(c in windows for c in ' |&<>^%\"')
    (scratch/'echo.py').write_text('import json,sys\nprint(json.dumps(sys.argv[1:]))\n')
    (scratch/'fake-az.cmd').write_bytes(('@echo off\r\n'+r'C:\Python3\python.exe "'+windows+r'\echo.py" %*'+'\r\n').encode())
    raw = pathlib.Path(a.wrapper).read_bytes()
    assert hashlib.sha256(raw).hexdigest() == a.wrapper_sha256
    target = a.target.encode()
    assert raw.count(target) == 1
    # Substitute only the executable under test. All quoting/interop code is unchanged.
    probe = scratch/'az'
    probe.write_bytes(raw.replace(target, a.fake.replace('{win}', windows).encode()))
    probe.chmod(0o700)
    rows = [json.loads(x) for x in pathlib.Path(a.argv_file).read_text().splitlines()]
    assert rows and any(r[:2] == ['tag', 'update'] for r in rows)
    old = ['resource','list','--subscription','11111111-1111-1111-1111-111111111111','--query',"[?tags.seaweed_run=='output' || type=='Microsoft.Compute/disks'].id",'--output','json']
    broken = subprocess.run([str(probe), *old], capture_output=True, text=True, timeout=15)
    try:
        old_intact = broken.returncode == 0 and json.loads(broken.stdout) == old
    except ValueError:
        old_intact = False
    # Through cmd.exe the compound query must break (the original RED); a wrapper with no cmd.exe must carry it intact.
    if a.expect_old_query == 'broken':
        assert not old_intact, 'old compound query unexpectedly survived; distinguishing RED missing'
    else:
        assert old_intact, ('a wrapper without cmd.exe altered an argument', broken.returncode, broken.stdout, broken.stderr)
    log = [dict(case='old-query', returncode=broken.returncode, stdout=broken.stdout, stderr=broken.stderr, argv=old)]
    for args in sorted({tuple(row) for row in rows}):
        r = subprocess.run([str(probe), *args], capture_output=True, text=True, timeout=15)
        assert r.returncode == 0, (args,r.returncode,r.stderr)
        log.append(dict(case="safe-argv",argv=list(args),returncode=r.returncode,stdout=r.stdout,stderr=r.stderr))
        assert json.loads(r.stdout) == list(args), (args,r.stdout)
    receipt = {'result':'PASS','argv_count':len(rows),'distinct_argv':len({tuple(row) for row in rows}),'old_compound_query':'RED: argv not preserved' if a.expect_old_query == 'broken' else 'preserved intact','wrapper_sha256':hashlib.sha256(raw).hexdigest(),'substitution':'executable only (%s -> %s); real /init -> Windows process -> argv echo' % (a.target.strip(), a.fake.strip())}
    (scratch/'replay.jsonl').write_text(''.join(json.dumps(x)+'\n' for x in log))
    (scratch/'result.json').write_text(json.dumps(receipt,indent=2)+'\n')
    print(json.dumps(receipt))

if __name__ == '__main__':
    main()