#!/usr/bin/env bash
# plc_owned.sh DIR TOKEN: exit 0 only if DIR is this invocation's run directory: TOKEN is the 32-hex token
# prepare generated and DIR/.plc-owned holds it. Every always-phase step that writes or removes under the run
# directory calls this first, so a failed prepare (no token: the variable stays unrendered) or another run's
# directory is never touched.
set -uf
d=$1 tok=$2
printf '%s\n' "$tok" | grep -qxE '[0-9a-f]{32}' || { echo "owned: prepare acquired no run directory; $d untouched" >&2; exit 1; }
[ "$(cat "$d/.plc-owned" 2>/dev/null)" = "$tok" ] || { echo "owned: $d is not this invocation's run directory; untouched" >&2; exit 1; }
