#!/usr/bin/env bash
# reaper.sh SEAWEED_CLOUD NAME:RG [NAME:RG...]: the controller-side half of the idle guard. The box holds no Azure
# identity, so its idle guard can only power the OS off, which leaves the VM "stopped" (allocated, compute still
# billing). Every REAP_INTERVAL seconds (default 300) this asks `standing status` for each box and runs `standing stop`
# (= deallocate) for a box reported stopped. Anything else (running, deallocated, an error) is left alone and logged.
# REAP_ONCE=1 runs a single pass (tests, cron). Uses only the box's pinned identity through seaweed-cloud.
set -uo pipefail
[ $# -ge 2 ] || { echo "usage: reaper.sh SEAWEED_CLOUD NAME:RG [NAME:RG...]" >&2; exit 2; }
sc=$1; shift
interval=${REAP_INTERVAL:-300}
state() { sed -n 's/.*"power_state":"\([a-z]*\)".*/\1/p' <<< "$1"; }
while :; do
  for box in "$@"; do
    name=${box%%:*} rg=${box#*:}
    out=$("$sc" standing status --name "$name" --resource-group "$rg" 2>&1); s=$(state "$out")
    if [ "$s" = stopped ]; then
      r=$("$sc" standing stop --name "$name" --resource-group "$rg" 2>&1)
      echo "$(date -u +%FT%TZ) $name: stopped (allocated, billing) -> deallocate: $(state "$r" || true) $(tail -c 200 <<< "$r")"
    else
      echo "$(date -u +%FT%TZ) $name: ${s:-UNKNOWN} (left alone)"
    fi
  done
  [ "${REAP_ONCE:-0}" = 1 ] && exit 0
  sleep "$interval"
done
