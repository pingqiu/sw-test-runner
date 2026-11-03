#!/usr/bin/env bash
# Local disposable pack: real product, real worker/Engine, no lab authority.
set -euo pipefail
if [[ $# != 2 || $EUID != 0 ]]; then
  echo 'usage (Linux root): bash scripts/run-ec-stage1.sh /absolute/weed13230 /absolute/NEW-evidence-dir' >&2
  exit 2
fi
cd "$(dirname "$0")/.."
export EC_LOCAL_WEED="$(realpath "$1")"
test "$(sha256sum "$EC_LOCAL_WEED" | cut -d' ' -f1)" = e15a1951b6e7d2b159bbeb5740f8bce5370d1d9ee3eaaaf4d9f2e38e1c2926f4
[[ $2 = /* && ! -e $2 ]]
for cmd in go python3 getcap flock tar; do command -v "$cmd" >/dev/null; done
export EC_LOCAL_ADVERTISE="${EC_LOCAL_ADVERTISE:-$(hostname -I | awk '{print $1}')}"
# Fixed profile ports: refuse a collision before starting any service.
python3 - <<'PY'
import socket
ports=[23555,33555,23888,33888,23833,33833]+list(range(24100,24114))+list(range(34100,34114))
sockets=[]
try:
    for port in ports:
        s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('0.0.0.0',port));sockets.append(s)
finally:
    for s in sockets:s.close()
PY
umask 077
mkdir -m 0700 -- "$2"
export EC_LOCAL_EVIDENCE="$2"
export EC_LOCAL_RUNNER="$2/sw-test-runner"
export CGO_ENABLED=0
go build -o "$EC_LOCAL_RUNNER" ./cmd/sw-test-runner
sha256sum "$EC_LOCAL_WEED" "$EC_LOCAL_RUNNER" > "$2/BINARY-SHA256SUMS"
go version > "$2/go-version.txt"
python3 --version > "$2/python-version.txt"
# Windows-created worktrees can have a .git pointer unusable from WSL.
git rev-parse HEAD > "$2/runner-source.txt" 2>/dev/null || printf 'use SOURCE-SHA256SUMS (Windows worktree)\n' > "$2/runner-source.txt"
sha256sum scripts/run-ec-stage1.sh infra/ec_local_linux_test.go infra/deployment_linux_test.go packs/seaweed/local-ec/* packs/seaweed/worker/ec_*.py packs/seaweed/worker/chaos_load.py > "$2/SOURCE-SHA256SUMS"
set +e
go test -count=1 -timeout=660s -run '^TestECLocalStage1$' -v ./infra > "$2/run.log" 2>&1
status=$?
set -e
cat "$2/run.log"
if [[ $status == 0 ]]; then
  python3 packs/seaweed/local-ec/report.py "$2" > "$2/rows.log"
  cat "$2/rows.log"
fi
exit "$status"
