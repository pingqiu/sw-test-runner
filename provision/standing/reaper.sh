#!/usr/bin/env bash
# reaper.sh SEAWEED_CLOUD NAME:RG [NAME:RG...]: the controller-side half of the idle guard. The box holds no Azure
# identity, so its idle guard can only power the OS off, which leaves the VM "stopped" (allocated, compute still
# billing). Every REAP_INTERVAL seconds (default 300) this asks `standing status` for each box and runs `standing stop`
# (= deallocate) for a box reported stopped. Anything else (running, deallocated) is left alone and logged.
# Silent failure is not allowed: a box whose status is UNKNOWN, or whose stop fails, REAP_ALERT_AFTER passes in a row
# (default 3) prints an "ALERT" line (the 6-hourly cost line picks it up), and a single pass (REAP_ONCE=1) then exits 3.
# REAP_STATE (default ~/.local/state/seaweed-cloud/standing/reaper) keeps the per-box failure counts across passes and
# the "last" stamp (time of the last completed pass) that the cost line reports.
set -uo pipefail
[ $# -ge 2 ] || { echo "usage: reaper.sh SEAWEED_CLOUD NAME:RG [NAME:RG...]" >&2; exit 2; }
sc=$1; shift
interval=${REAP_INTERVAL:-300} after=${REAP_ALERT_AFTER:-3}
st=${REAP_STATE:-$HOME/.local/state/seaweed-cloud/standing/reaper}
mkdir -p "$st" || { echo "reaper: cannot create $st" >&2; exit 2; }
state() { sed -n 's/.*"power_state":"\([a-z]*\)".*/\1/p' <<< "$1"; }
while :; do
  alert=0
  for box in "$@"; do
    name=${box%%:*} rg=${box#*:} ok=1
    out=$("$sc" standing status --name "$name" --resource-group "$rg" 2>&1); s=$(state "$out")
    if [ "$s" = stopped ]; then
      r=$("$sc" standing stop --name "$name" --resource-group "$rg" 2>&1); rs=$(state "$r")
      [ "$rs" = deallocated ] || [ "$rs" = deallocating ] || ok=0
      echo "$(date -u +%FT%TZ) $name: stopped (allocated, billing) -> deallocate: ${rs:-FAILED} $(tail -c 160 <<< "$r")"
    elif [ -z "$s" ] || [ "$s" = UNKNOWN ]; then
      ok=0; echo "$(date -u +%FT%TZ) $name: UNKNOWN status: $(tail -c 160 <<< "$out")"
    else
      echo "$(date -u +%FT%TZ) $name: $s (left alone)"
    fi
    n=$(cat "$st/$name.fail" 2>/dev/null || echo 0)
    if [ $ok = 1 ]; then n=0; else n=$((n + 1)); fi
    echo "$n" > "$st/$name.fail"
    if [ "$n" -ge "$after" ]; then alert=1; echo "$(date -u +%FT%TZ) ALERT $name: $n consecutive passes without a known state or a successful deallocate"; fi
  done
  date -u +%FT%TZ > "$st/last"
  if [ "${REAP_ONCE:-0}" = 1 ]; then [ $alert = 1 ] && exit 3; exit 0; fi
  sleep "$interval"
done
