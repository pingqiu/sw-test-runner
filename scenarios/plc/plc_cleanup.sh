#!/usr/bin/env bash
# plc_cleanup.sh DIR TOKEN ARTIFACTS PYTHON: remove this invocation's run directory only once everything that
# decided the run is collected byte for byte. For each of evidence/, capture/ and lout/ that exists under DIR,
# ARTIFACTS/plc-<name>.tgz must exist and read to its end (the gzip trailer checks the stream), and its regular
# members must be exactly DIR/<name>'s files with the same sha256 each. A member with old or truncated bytes, a
# file changed after collection (collect_path accepts tar's "file changed as we read it"), a missing or extra
# file, or an unreadable archive keeps DIR (exit 1, reason printed): a failed or partial collection never
# deletes judgment evidence.
set -uf -o pipefail
d=$1 tok=$2 art=$3 py=${4:-python3}
here=$(cd "$(dirname "$0")" && pwd)
[ -e "$d" ] || { echo "cleanup: $d is already gone"; exit 0; }
bash "$here/plc_owned.sh" "$d" "$tok" || exit 1
"$py" -I - "$d" "$art" <<'PY' || { echo "cleanup: keeping $d" >&2; exit 1; }
import hashlib, sys, tarfile
from pathlib import Path
d, art = Path(sys.argv[1]), Path(sys.argv[2])
for sub in ('evidence', 'capture', 'lout'):
    src = d/sub
    if not src.is_dir():
        continue
    a = art/('plc-%s.tgz' % sub)
    if not a.is_file():
        sys.exit('cleanup: %s/ was not collected (%s missing)' % (sub, a))
    disk = {p.relative_to(d).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
            for p in src.rglob('*') if p.is_file()}
    got = {}
    try:
        with tarfile.open(a, 'r:gz') as tf:
            for m in tf:
                if m.isdir():
                    continue
                if not m.isfile():
                    sys.exit('cleanup: %s holds a non-regular member %s' % (a.name, m.name))
                got[m.name.removeprefix('./')] = hashlib.sha256(tf.extractfile(m).read()).hexdigest()
    except (OSError, EOFError, tarfile.TarError) as error:
        sys.exit('cleanup: %s does not read to its end: %s' % (a.name, error))
    if got != disk:
        diff = sorted(k for k in set(got) | set(disk) if got.get(k) != disk.get(k))
        sys.exit('cleanup: %s differs from %s/ in %d file(s): %s' % (a.name, sub, len(diff), ', '.join(diff[:5])))
PY
rm -rf -- "$d"
echo "cleanup: removed $d (evidence, capture and lout collected byte for byte)"
