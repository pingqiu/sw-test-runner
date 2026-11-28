#!/usr/bin/env bash
# m0-own.sh: what one M0 run owns, and the only things its teardown touches.
#
#   m0-own.sh claim WORK                      create WORK exclusively; print this run's owner token
#   m0-own.sh spawn WORK TOKEN NAME PORTS -- CMD...
#                                             refuse if a PORT already listens; start CMD; record
#                                             "pid starttime NAME"; wait until every PORT listens
#                                             and is held by that process (or it exits: fail)
#   m0-own.sh record WORK TOKEN NAME PID      record a process started elsewhere, with its start time
#   m0-own.sh pause WORK TOKEN NAME BOUND     pause the recorded process NAME, found by its pid and
#                                             start time in WORK/owned only, through ONE detached
#                                             helper: it re-checks the start time, sends SIGSTOP,
#                                             writes its acknowledgement (the paused time) to a file
#                                             in WORK, sleeps BOUND seconds (1..600), re-checks the
#                                             start time and sends SIGCONT, whatever happens to the
#                                             caller, so no stop comes after its continue. The caller
#                                             waits for the acknowledgement (OWN_ACK_WAIT seconds,
#                                             default 5) and prints one JSON line {outcome, process,
#                                             pid, paused_ns, bound_seconds, error}: ok with it;
#                                             refused (exit 2) when nothing was stopped (the helper
#                                             cannot launch, or found the process gone); unknown
#                                             (exit 1), "pause not acknowledged", when none came
#   m0-own.sh continue WORK TOKEN NAME        continue (SIGCONT) that process now; print
#                                             {outcome, process, pid, continued_ns, error}
#   m0-own.sh teardown WORK TOKEN NQN_PREFIX  only if WORK/.owner holds TOKEN: delete the kernel
#                                             NVMe controllers whose subsystem NQN starts with this
#                                             run's own prefix; stop each recorded process only if
#                                             its start time still matches (a recycled pid is
#                                             another process); fail if anything owned remains
#
# Identity is (pid, start time from /proc/PID/stat), never a pid alone. A run
# that did not acquire WORK (no token, or a token that is not WORK's) changes
# nothing. NVME_SYSFS (default /sys/class/nvme), PROC (default /proc),
# OWN_SETSID (default setsid: the pause helper's launcher) and OWN_ACK_DELAY
# (default 0: seconds the pause waits before reading its acknowledgement)
# exist for the controls.
set -u
SYS=${NVME_SYSFS:-/sys/class/nvme}
PROC=${PROC:-/proc}

die() { echo "m0-own: $*" >&2; exit 1; }

# start_of PID: the process's start time (field 22 of stat; the command name
# in field 2 may hold spaces and parentheses, so cut after the last ')').
start_of() {
  local s
  s=$(cat "$PROC/$1/stat" 2>/dev/null) || return 1
  s=${s##*) }
  set -- $s
  echo "${20}"
}

# proc_state PID START: running (that pid runs with that start time), gone
# (no such pid, or another process now holds it), or unreadable (it exists
# but its identity cannot be read: nothing may be concluded).
proc_state() {
  # Keep the arguments: 'set --' below replaces the positional parameters.
  local pid=$1 want=$2 s
  [ -e "$PROC/$pid" ] || { echo gone; return; }
  s=$(cat "$PROC/$pid/stat" 2>/dev/null) || { echo unreadable; return; }
  s=${s##*) }
  set -- $s
  [ -n "${20:-}" ] || { echo unreadable; return; }
  [ "${20}" = "$want" ] && echo running || echo gone
}

owned() { # WORK TOKEN
  case "$2" in ''|*[!0-9a-f]*) return 1 ;; esac
  [ "${#2}" = 32 ] || return 1
  [ "$(cat "$1/.owner" 2>/dev/null)" = "$2" ]
}

listening() { # PORT -> the pids holding a listener on it, or nothing; fails if ss cannot be read
  local out
  out=$(ss -Hltnp "sport = :$1") || return 2
  printf '%s\n' "$out" | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u
  return 0
}

# ledger_proc NAME: "pid start" of the last process recorded as NAME in the
# ownership ledger, or nothing.
ledger_proc() {
  awk -v n="$2" '$3 == n { p = $1 " " $2 } END { if (p != "") print p }' "$1/owned"
}

# fail_json PROCESS MESSAGE: an unknown answer, as one JSON line, exit 1.
fail_json() {
  printf '{"outcome":"unknown","process":"%s","pid":0,"error":"%s"}\n' "$1" "$(printf '%s' "$2" | tr -d '"\\')"
  exit 1
}

# refuse_json PROCESS MESSAGE: a refused answer (nothing changed), exit 2.
refuse_json() {
  printf '{"outcome":"refused","process":"%s","pid":0,"error":"%s"}\n' "$1" "$(printf '%s' "$2" | tr -d '"\\')"
  exit 2
}

case "${1:-}" in
pause)
  w=$2 tok=$3 name=$4 bound=$5
  owned "$w" "$tok" || fail_json "$name" "not this run's WORK: nothing paused"
  case "$bound" in ''|*[!0-9]*) fail_json "$name" "bound $bound" ;; esac
  [ "$bound" -ge 1 ] && [ "$bound" -le 600 ] || fail_json "$name" "bound $bound outside 1..600"
  set -- $(ledger_proc "$w" "$name")
  pid=${1:-} st=${2:-}
  [ -n "$pid" ] || fail_json "$name" "$name is not in the ownership ledger"
  [ "$(proc_state "$pid" "$st")" = running ] || fail_json "$name" "$name (pid $pid) is not running with its recorded start time"
  # ONE detached helper stops and continues, in that order: the pause ends by
  # BOUND even if this script, or the runner, dies or stalls, and no stop can
  # come after the continue. Its acknowledgement ("paused NS", or "refused
  # WHY" when it stopped nothing) is written whole (a rename) to ack.
  launcher=${OWN_SETSID:-setsid}
  command -v "$launcher" > /dev/null 2>&1 || refuse_json "$name" "the pause helper cannot launch ($launcher): nothing stopped"
  ack="$w/.pause-$name-$$-$(date +%s%N).ack"
  "$launcher" bash -c 'pid=$1 st=$2 bound=$3 proc=$4 ack=$5
    startof() { local s; s=$(cat "$proc/$pid/stat" 2>/dev/null) || return 1; s=${s##*) }; set -- $s; echo "${20}"; }
    say() { printf "%s\n" "$1" > "$ack.tmp" && mv "$ack.tmp" "$ack"; }
    [ "$(startof)" = "$st" ] || { say "refused the process is gone or recycled"; exit 0; }
    kill -STOP "$pid" || { say "refused SIGSTOP of pid $pid failed"; exit 0; }
    say "paused $(date +%s%N)"
    sleep "$bound"
    [ "$(startof)" = "$st" ] && kill -CONT "$pid"' _ "$pid" "$st" "$bound" "$PROC" "$ack" > /dev/null 2>&1 < /dev/null &
  sleep "${OWN_ACK_DELAY:-0}"
  i=0
  while [ ! -e "$ack" ] && [ "$i" -lt $(( ${OWN_ACK_WAIT:-5} * 10 )) ]; do
    sleep 0.1
    i=$((i + 1))
  done
  [ -e "$ack" ] || fail_json "$name" "pause not acknowledged within ${OWN_ACK_WAIT:-5} s: the helper may still stop pid $pid, and it continues it by $bound s"
  read -r what rest < "$ack"
  case "$what" in
  paused) printf '{"outcome":"ok","process":"%s","pid":%s,"paused_ns":%s,"bound_seconds":%s,"error":""}\n' "$name" "$pid" "$rest" "$bound" ;;
  refused) refuse_json "$name" "$rest: nothing stopped" ;;
  *) fail_json "$name" "pause acknowledgement unreadable: $what $rest" ;;
  esac ;;
continue)
  w=$2 tok=$3 name=$4
  owned "$w" "$tok" || fail_json "$name" "not this run's WORK: nothing continued"
  set -- $(ledger_proc "$w" "$name")
  pid=${1:-} st=${2:-}
  [ -n "$pid" ] || fail_json "$name" "$name is not in the ownership ledger"
  [ "$(proc_state "$pid" "$st")" = running ] || fail_json "$name" "$name (pid $pid) is not running with its recorded start time"
  kill -CONT "$pid" || fail_json "$name" "SIGCONT of pid $pid failed"
  printf '{"outcome":"ok","process":"%s","pid":%s,"continued_ns":%s,"error":""}\n' "$name" "$pid" "$(date +%s%N)" ;;
claim)
  w=$2
  case "$w" in /*) ;; *) die "WORK must be absolute" ;; esac
  mkdir "$w" 2>/dev/null || die "$w exists: this run acquires nothing"
  tok=$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')
  printf '%s\n' "$tok" > "$w/.owner"
  : > "$w/owned"
  echo "$tok" ;;
spawn)
  w=$2 tok=$3 name=$4; shift 4
  owned "$w" "$tok" || die "not this run's WORK: nothing started"
  ports=()
  while [ $# -gt 0 ] && [ "$1" != -- ]; do ports+=("$1"); shift; done
  [ "${1:-}" = -- ] || die "spawn needs -- before the command"; shift
  for p in "${ports[@]}"; do
    held=$(listening "$p") || die "cannot read the listeners of port $p: $name not started"
    [ -z "$held" ] || die "port $p already listens (pids $(echo $held)): $name not started"
  done
  setsid "$@" > "$w/log/$name.log" 2>&1 < /dev/null &
  pid=$!
  st=$(start_of "$pid") || die "$name exited at once (see $w/log/$name.log)"
  echo "$pid $st $name" >> "$w/owned"
  for i in $(seq 1 240); do
    [ "$(start_of "$pid" 2>/dev/null)" = "$st" ] || die "$name (pid $pid) exited before it listened (see $w/log/$name.log)"
    all=1
    for p in "${ports[@]}"; do held=$(listening "$p") && printf '%s\n' "$held" | grep -qx "$pid" || all=0; done
    [ $all = 1 ] && { echo "$pid"; exit 0; }
    sleep 0.5
  done
  die "$name (pid $pid) did not hold ports ${ports[*]} within 120 s" ;;
record)
  w=$2 tok=$3 name=$4 pid=$5
  owned "$w" "$tok" || die "not this run's WORK: nothing recorded"
  case "$pid" in ''|*[!0-9]*) die "pid $pid" ;; esac
  st=$(start_of "$pid") || die "$name (pid $pid) is not running"
  echo "$pid $st $name" >> "$w/owned" ;;
teardown)
  w=$2 tok=$3 prefix=$4
  if ! owned "$w" "$tok"; then echo "m0-own: this run acquired no WORK ($w): nothing touched"; exit 0; fi
  case "$prefix" in nqn.*-) ;; *) die "NQN prefix $prefix is not run-scoped" ;; esac
  rc=0
  for d in "$SYS"/nvme*; do
    [ -e "$d" ] || continue
    q=$(cat "$d/subsysnqn" 2>/dev/null) || { echo "m0-own: unreadable $d/subsysnqn" >&2; rc=1; continue; }
    case "$q" in "$prefix"*) echo 1 > "$d/delete_controller" || { echo "m0-own: delete of $d failed" >&2; rc=1; } ;; esac
  done
  # Newest first: the run's own processes before the store they depend on.
  # The list is read once; an unreadable list certifies nothing.
  list=$(tac "$w/owned") || { echo "m0-own: cannot read $w/owned: cleanup not certified" >&2; exit 1; }
  while read -r pid st name; do
    [ -n "$pid" ] || continue
    case "$(proc_state "$pid" "$st")" in
      gone) echo "m0-own: $name (pid $pid) already gone or recycled: untouched"; continue ;;
      unreadable) echo "m0-own: identity of $name (pid $pid) unreadable: not certified" >&2; rc=1; continue ;;
    esac
    kill "$pid" 2>/dev/null
    for i in $(seq 1 60); do [ "$(proc_state "$pid" "$st")" = running ] || break; sleep 0.5; done
    [ "$(proc_state "$pid" "$st")" = running ] && kill -9 "$pid" 2>/dev/null && sleep 1
    case "$(proc_state "$pid" "$st")" in
      gone) echo "m0-own: stopped $name (pid $pid)" ;;
      running) echo "m0-own: $name (pid $pid) still running" >&2; rc=1 ;;
      *) echo "m0-own: $name (pid $pid): its state after the stop is unreadable: not certified" >&2; rc=1 ;;
    esac
  done <<< "$list"
  # The kernel removes a deleted controller asynchronously: wait (bounded)
  # until none of this run's prefix is left before calling one a leftover.
  left=""
  for i in $(seq 1 40); do
    left=""
    for d in "$SYS"/nvme*; do
      [ -e "$d" ] || continue
      case "$(cat "$d/subsysnqn" 2>/dev/null)" in "$prefix"*) left="$left $d" ;; esac
    done
    [ -z "$left" ] && break
    sleep 0.25
  done
  [ -z "$left" ] || { echo "m0-own: controllers of this run remain after 10 s:$left" >&2; rc=1; }
  exit $rc ;;
*) die "usage: claim | spawn | record | pause | continue | teardown" ;;
esac
