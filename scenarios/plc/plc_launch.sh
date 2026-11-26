#!/usr/bin/env bash
# plc_launch.sh PIDFILE CMD [ARGS...]: run CMD under a launcher whose lifetime covers it, for the settle barrier.
# Before CMD starts, the launcher records its own "pid starttime" to PIDFILE (atomically); it then runs CMD as
# its child, records the child's pid to PIDFILE.child, and exits only after the child has exited, with the
# child's status. TERM/INT/HUP are forwarded to the child (whose own cancellation then runs) and never end the
# launcher early. So "the recorded process has exited" means the cloud child has exited. When the scenario's
# shell is killed on timeout or cancel, the launcher and its child keep running and the barrier waits on them.
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
printf '%s\n' "$child" > "$pf.child"
while :; do
  wait "$child"
  rc=$?
  kill -0 "$child" 2>/dev/null || break
done
exit "$rc"
