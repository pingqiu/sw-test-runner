#!/usr/bin/env bash
# plc_launch.sh PIDFILE CMD [ARGS...]: run CMD under a launcher that leaves a terminal receipt, for the settle
# barrier. Before CMD starts, the launcher records its own "pid starttime" to PIDFILE (atomically). It then runs
# CMD as its child and records the child's "pid starttime" to PIDFILE.child (for the recovery handle). When the
# launcher's wait has REAPED the child, it writes the child's exit status to PIDFILE.exit (atomically), then exits
# with that status. PIDFILE.exit is the only proof that the cloud child terminated: a launcher that dies any other
# way (OOM, SIGKILL) leaves no receipt, and settle reports UNKNOWN. TERM/INT/HUP are forwarded to the child
# (whose own cancellation then runs) and never end the launcher early. When the scenario's shell is killed on
# timeout or cancel, the launcher and its child keep running.
set -uf
pf=$1; shift
read -r st < "/proc/$$/stat" || exit 97
f=(${st##*) })
printf '%s %s\n' "$$" "${f[19]}" > "$pf.tmp" && mv -f "$pf.tmp" "$pf" || exit 97
child=
forward() { [ -n "$child" ] && kill "-$1" "$child" 2>/dev/null; }
trap 'forward TERM' TERM
trap 'forward INT' INT
trap 'forward HUP' HUP
"$@" &
child=$!
cst=-
if read -r st < "/proc/$child/stat" 2>/dev/null; then f=(${st##*) }); cst=${f[19]}; fi
printf '%s %s\n' "$child" "$cst" > "$pf.child"
# A forwarded signal interrupts wait while the child still runs; loop until the child is reaped (it then leaves
# the job table, so a reused pid can never be mistaken for it).
while :; do
  wait "$child"
  rc=$?
  [ -z "$(jobs -rp)" ] && break
done
printf '%s\n' "$rc" > "$pf.exit.tmp" && mv -f "$pf.exit.tmp" "$pf.exit"
exit "$rc"
