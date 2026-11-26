#!/usr/bin/env bash
# plc_gate.sh ARCHIVE CLASS PYTHON: the gate value of a power-loss run, read back from its COLLECTED evidence.
# Prints 1 only for class=counted with seal.txt sealed=yes and a PASS verdict in judge.json; prints 0 for a PASS
# rehearsal or dry run (recorded, never counted). Exits 1 (no value) when the collected verdict is not PASS or
# the evidence is missing: mark_pass then fails and gate_pass stays 0.
set -euf -o pipefail
archive=$1 class=$2 py=$3
# One reader for both members; a missing member fails the pipeline (pipefail).
verdict=$(tar -xzO -f - evidence/judge.json < "$archive" | "$py" -I -c 'import json,sys; sys.stdout.write(json.load(sys.stdin)["verdict"])') \
  || { echo 'plc_gate: no collected judge.json' >&2; exit 1; }
[ "$verdict" = PASS ] || { echo "plc_gate: collected verdict $verdict" >&2; exit 1; }
seal=$(tar -xzO -f - evidence/seal.txt < "$archive" | "$py" -I -c 'import sys; sys.stdout.write(sys.stdin.read().strip())') \
  || { echo 'plc_gate: no collected seal.txt' >&2; exit 1; }
if [ "$class" = counted ] && [ "$seal" = sealed=yes ]; then
  echo 1
else
  echo "plc_gate: $class run, $seal: PASS recorded, not counted" >&2
  echo 0
fi
