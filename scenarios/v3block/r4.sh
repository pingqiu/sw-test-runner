#!/usr/bin/env bash
# R4 node loss (scenarios/v3block/r4-node-loss.yaml): the scenario's checks,
# one subcommand each, so every check has a failing control
# (packs/v3block/r4_scenario_test.go). Nothing here starts or stops a product
# process or a VM; the scenario does that through runner actions.
#
#   r4.sh write DEV LEDGER ROUND COUNT     COUNT acked 4 KiB writes (O_DIRECT|O_DSYNC)
#   r4.sh verify DEV LEDGER                every acked range reads back: 0 mismatches
#   r4.sh drop-control DEV LEDGER          the red control: one acked write the
#                                          device never got must make verify FAIL
#   r4.sh no-ack DEV LEDGER SECONDS        one write must NOT be acknowledged in SECONDS
#   r4.sh held STATUS... -- SECONDS EPOCH  no survivor becomes primary, none leaves EPOCH
#   r4.sh wait-role STATUS ROLE EPOCH SECONDS   STATUS reports ROLE (primary|replica) at EPOCH
#
# DEV is the NVMe namespace (or, in the controls, a file). LEDGER lines are
# "round offset length sha256", written only AFTER the write returned success:
# the ledger holds acknowledged writes and nothing else. STATUS is a command
# that prints one blockvolume /status JSON (for example
# "curl -s http://127.0.0.1:9203/status?volume=v1").
# R4_DD_OFLAG / R4_DD_IFLAG override the direct-I/O flags (the controls run on files).
set -euo pipefail

bs=4096
oflag="${R4_DD_OFLAG:-direct,dsync}"
iflag="${R4_DD_IFLAG:-direct}"

die() { echo "r4: $*" >&2; exit 1; }

sha_of_block() { # DEV OFFSET
  dd if="$1" bs=$bs count=1 skip=$(( $2 / bs )) iflag="$iflag" status=none 2>/dev/null | sha256sum | cut -d' ' -f1
}

# write_one DEV OFFSET FILE: one write of FILE at OFFSET; exit status is the ack.
write_one() {
  dd if="$3" of="$1" bs=$bs count=1 seek=$(( $2 / bs )) oflag="$oflag" conv=notrunc status=none 2>/dev/null
}

blocks_of() { # DEV -> number of 4 KiB blocks
  local n
  if [[ -b "$1" ]]; then n=$(blockdev --getsize64 "$1"); else n=$(stat -c %s "$1"); fi
  echo $(( n / bs ))
}

cmd_write() {
  local dev="$1" ledger="$2" round="$3" count="$4" tmp
  local blocks; blocks=$(blocks_of "$dev")
  (( blocks > 0 )) || die "write: $dev has no blocks"
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' RETURN
  local i off sum acked=0
  for (( i = 0; i < count; i++ )); do
    off=$(( (RANDOM * 32768 + RANDOM) % blocks * bs ))
    head -c $bs /dev/urandom > "$tmp"
    sum=$(sha256sum < "$tmp" | cut -d' ' -f1)
    if write_one "$dev" "$off" "$tmp"; then
      echo "$round $off $bs $sum" >> "$ledger"   # acknowledged: recorded
      acked=$((acked + 1))
    else
      echo "r4: write $round@$off not acknowledged" >&2
    fi
  done
  echo "acked=$acked of $count"
}

# verify: the LAST acknowledged write of each offset must read back.
cmd_verify() {
  local dev="$1" ledger="$2"
  [[ -s "$ledger" ]] || die "verify: empty ledger $ledger (nothing acknowledged)"
  local -A want=()
  local round off len sum
  while read -r round off len sum; do
    [[ "$len" == "$bs" ]] || die "verify: ledger line with length $len"
    want[$off]="$sum $round"
  done < "$ledger"
  local bad=0 got
  for off in "${!want[@]}"; do
    got=$(sha_of_block "$dev" "$off")
    if [[ "$got" != "${want[$off]%% *}" ]]; then
      echo "MISMATCH offset=$off round=${want[$off]##* } want=${want[$off]%% *} got=$got"
      bad=$((bad + 1))
    fi
  done
  echo "verified=${#want[@]} mismatches=$bad"
  (( bad == 0 ))
}

# drop-control: the ledger plus one acked write the device never received
# (a random block at a fresh offset): verify must fail on it.
cmd_drop_control() {
  local dev="$1" ledger="$2" ctl
  ctl=$(mktemp)
  trap 'rm -f "$ctl"' RETURN
  cp "$ledger" "$ctl"
  local blocks off
  blocks=$(blocks_of "$dev")
  off=$(( (blocks - 1) * bs ))
  echo "dropped $off $bs $(head -c $bs /dev/urandom | sha256sum | cut -d' ' -f1)" >> "$ctl"
  if cmd_verify "$dev" "$ctl" >/dev/null 2>&1; then
    die "drop-control: verify passed with a dropped acked write: the check cannot fail"
  fi
  echo "drop-control: verify failed on the dropped write, as it must"
}

# no-ack: a write in the window must not be acknowledged within SECONDS
# (RF2 after the failover, before the rejoin: quorum 2 with one copy up).
cmd_no_ack() {
  local dev="$1" ledger="$2" secs="$3" tmp off
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' RETURN
  head -c $bs /dev/urandom > "$tmp"
  off=0
  if timeout "$secs" dd if="$tmp" of="$dev" bs=$bs count=1 seek=0 oflag="$oflag" conv=notrunc status=none 2>/dev/null; then
    echo "window 0 $bs $(sha256sum < "$tmp" | cut -d' ' -f1)" >> "$ledger"
    die "no-ack: a write was ACKNOWLEDGED in the window (a FINDING: the volume ran on one copy)"
  fi
  echo "no-ack: no acknowledgement in ${secs}s"
}

field() { # JSON KEY: a top-level scalar
  python3 -c 'import json,sys; v=json.load(sys.stdin).get(sys.argv[1]); print("" if v is None else v)' "$1"
}

role_of() { # STATUS -> "role epoch healthy"
  local j
  j=$(bash -c "$1") || return 1
  echo "$(echo "$j" | field AuthorityRole) $(echo "$j" | field Epoch) $(echo "$j" | field Healthy) $(echo "$j" | field ReplicationRole)"
}

# held STATUS... -- SECONDS EPOCH: for SECONDS, no survivor reports primary,
# and none leaves EPOCH (no line was published while the primary is lost).
cmd_held() {
  local status=()
  while [[ $# -gt 0 && "$1" != "--" ]]; do status+=("$1"); shift; done
  [[ "${1:-}" == "--" ]] || die "held: STATUS... -- SECONDS EPOCH"
  local secs="$2" epoch="$3" end=$(( SECONDS + $2 )) s r
  (( ${#status[@]} > 0 )) || die "held: no survivor status"
  while :; do
    for s in "${status[@]}"; do
      r=$(role_of "$s") || die "held: status unreadable: $s"
      set -- $r
      [[ "$1" != "primary" ]] || die "held: a survivor became primary ($r): an automatic promotion"
      [[ "$2" == "$epoch" ]] || die "held: a survivor left epoch $epoch ($r): a line was published"
    done
    (( SECONDS < end )) || break
    sleep "${R4_POLL:-5}"
  done
  echo "held: no promotion for ${secs}s at epoch $epoch"
}

# wait-role STATUS ROLE EPOCH SECONDS: primary = AuthorityRole primary and
# Healthy; replica = not primary and ReplicationRole replica_ready.
cmd_wait_role() {
  local s="$1" want="$2" epoch="$3" end=$(( SECONDS + $4 )) r
  while :; do
    if r=$(role_of "$s"); then
      set -- $r
      if [[ "$2" == "$epoch" ]]; then
        if [[ "$want" == primary && "$1" == primary && "$3" == True ]]; then echo "primary at $epoch"; return 0; fi
        if [[ "$want" == replica && "$1" != primary && "$4" == replica_ready ]]; then echo "replica_ready at $epoch"; return 0; fi
      fi
    fi
    (( SECONDS < end )) || die "wait-role: not $want at epoch $epoch in time (last: ${r:-unreadable})"
    sleep "${R4_POLL:-2}"
  done
}

case "${1:-}" in
  write) shift; [[ $# -eq 4 ]] || die "write DEV LEDGER ROUND COUNT"; cmd_write "$@" ;;
  verify) shift; [[ $# -eq 2 ]] || die "verify DEV LEDGER"; cmd_verify "$@" ;;
  drop-control) shift; [[ $# -eq 2 ]] || die "drop-control DEV LEDGER"; cmd_drop_control "$@" ;;
  no-ack) shift; [[ $# -eq 3 ]] || die "no-ack DEV LEDGER SECONDS"; cmd_no_ack "$@" ;;
  held) shift; cmd_held "$@" ;;
  wait-role) shift; [[ $# -eq 4 ]] || die "wait-role STATUS ROLE EPOCH SECONDS"; cmd_wait_role "$@" ;;
  *) die "usage: r4.sh write|verify|drop-control|no-ack|held|wait-role ..." ;;
esac
