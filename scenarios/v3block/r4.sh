#!/usr/bin/env bash
# R4 node loss (scenarios/v3block/r4-node-loss.yaml): the scenario's host
# steps and checks, one subcommand each, so every one has a failing control
# (packs/v3block/r4_scenario_test.go). VMs are powered off and on by runner
# actions, not here.
#
# Checks:
#   r4.sh write DEV LEDGER ROUND COUNT       COUNT acked 4 KiB writes (O_DIRECT|O_DSYNC);
#                                            fails unless every one is acknowledged
#   r4.sh stream DEV LEDGER ROUND COUNT      continuous fault-boundary writes;
#                                            COUNT=0 until the owned unit stops
#   r4.sh writer-ready TAG SECONDS           writer active and at least one ACK
#   r4.sh writer-stop TAG                    writer survived the fault, now stop
#   r4.sh verify DEV LEDGER [ROUND...]       every acked write reads back: 0 mismatches;
#                                            each ROUND named must have acked writes
#   r4.sh drop-control DEV LEDGER            the red control: one acked write the
#                                            device never got must make verify FAIL
#   r4.sh no-ack DEV LEDGER SECONDS          one write must NOT be acknowledged in SECONDS
#   r4.sh held STATUS... -- SECONDS EPOCH    no survivor becomes primary, none leaves EPOCH
#   r4.sh wait-role STATUS ROLE EPOCH SECONDS   STATUS reports ROLE (primary|replica) at EPOCH
# Client:
#   r4.sh client-check                       nvme_tcp loaded, native multipath on, nvme-cli
#   r4.sh connect NQN ADDR PORT SECONDS      (re)connect NQN at ADDR, print its device (dev)
#   r4.sh dev NQN ADDR SECONDS               the one namespace of NQN with a live path to ADDR
# Host (TAG = the run's tag; work dir R4_ROOT/TAG, units r4-TAG-master|volume):
#   r4.sh install PKG TAG MASTER_IP          refuse unless fresh; copy the checked package
#   r4.sh spec TAG RF N1 N2 N3 DPORT CPORT   write the cluster spec for rf 3 or 2
#   r4.sh unit TAG ROLE -- EXEC ARG...       persistent master/volume/writer unit
#   r4.sh start TAG ROLE                     start it (again, after a reboot)
#   r4.sh cleanup TAG                        stop the run's units, keep their evidence,
#                                            remove them and prove they are gone
#   r4.sh purge TAG                          remove the run's work dir and uploads
#
# DEV is the NVMe namespace (or, in the controls, a file with R4_ALLOW_FILE=1).
# LEDGER lines are "intent|ack op round offset length sha256". Intent precedes
# the device write; ack follows success. After the latest ack, unacked intents
# at that offset are also possible contents. No other content is accepted.
# The device's last block is kept out of write's range: no-ack and
# drop-control use it, so a window write still in flight can never change a
# ledger block. STATUS is a command that prints one blockvolume /status JSON
# (for example "curl -sf --max-time 5 http://127.0.0.1:19603/status?volume=v1");
# each read is bounded by R4_STATUS_TIMEOUT.
#
# The units are the run's own, not the installed deploy/systemd units: root,
# flags inline, not enabled (a rebooted VM starts its blockvolume only when the
# scenario says so), with the example units' restart policy: Restart=on-failure
# and RestartPreventExitStatus=78, so a final refusal is not retried.
#
# Overrides for the controls: R4_DD_OFLAG / R4_DD_IFLAG (direct-I/O flags),
# R4_DD (the dd), R4_POLL, R4_ALLOW_FILE, R4_SYS / R4_DEVDIR (sysfs and /dev),
# R4_ROOT, R4_UNITDIR, R4_HOSTS, R4_SYSTEMCTL, R4_JOURNALCTL, R4_PGREP.
set -euo pipefail

bs=4096
oflag="${R4_DD_OFLAG:-direct,dsync}"
iflag="${R4_DD_IFLAG:-direct}"
dd_bin="${R4_DD:-dd}"
root="${R4_ROOT:-/var/lib/r4}"
scratch="${R4_TMP:-/tmp}"
unitdir="${R4_UNITDIR:-/etc/systemd/system}"
hosts="${R4_HOSTS:-/etc/hosts}"
systemctl_bin="${R4_SYSTEMCTL:-systemctl}"
journalctl_bin="${R4_JOURNALCTL:-journalctl}"
pgrep_bin="${R4_PGREP:-pgrep}"

die() { echo "r4: $*" >&2; exit 1; }

# A device the checks may use: a block device (a regular file only in the
# controls). A missing or stale device is a setup failure, never a result.
need_dev() {
  if [[ -b "$1" ]]; then return 0; fi
  if [[ "${R4_ALLOW_FILE:-}" == 1 && -f "$1" ]]; then return 0; fi
  die "$2: $1 is not a usable device (setup failure)"
}

sha_of_block() { # DEV OFFSET
  dd if="$1" bs=$bs count=1 skip=$(( $2 / bs )) iflag="$iflag" status=none 2>/dev/null | sha256sum | cut -d' ' -f1
}

# write_one DEV OFFSET FILE: one write of FILE at OFFSET; exit status is the ack.
write_one() {
  "$dd_bin" if="$3" of="$1" bs=$bs count=1 seek=$(( $2 / bs )) oflag="$oflag" conv=notrunc status=none 2>/dev/null
}

ledger_line() { # LEDGER KIND OP ROUND OFFSET SHA
  printf '%s %s %s %s %s %s\n' "$2" "$3" "$4" "$5" "$bs" "$6" >> "$1"
}

blocks_of() { # DEV -> number of 4 KiB blocks
  local n
  if [[ -b "$1" ]]; then n=$(blockdev --getsize64 "$1"); else n=$(stat -c %s "$1"); fi
  echo $(( n / bs ))
}

cmd_write() {
  local dev="$1" ledger="$2" round="$3" count="$4" tmp
  need_dev "$dev" write
  local blocks; blocks=$(blocks_of "$dev")
  (( blocks > 1 )) || die "write: $dev has too few blocks"
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' RETURN
  local i off sum acked=0
  for (( i = 0; i < count; i++ )); do
    off=$(( (RANDOM * 32768 + RANDOM) % (blocks - 1) * bs ))   # never the last block
    head -c $bs /dev/urandom > "$tmp"
    sum=$(sha256sum < "$tmp" | cut -d' ' -f1)
    local op="$round-$BASHPID-$i"
    ledger_line "$ledger" intent "$op" "$round" "$off" "$sum"
    if write_one "$dev" "$off" "$tmp"; then
      ledger_line "$ledger" ack "$op" "$round" "$off" "$sum"
      acked=$((acked + 1))
    else
      echo "r4: write $round@$off not acknowledged" >&2
    fi
  done
  echo "acked=$acked of $count"
  (( acked == count )) || die "write: only $acked of $count writes acknowledged in round $round"
}

# The client writer remains alive across the fault. Its run-owned systemd
# unit bounds and stops it; every failed attempt remains an unacked intent.
# Count=0 streams until stopped; a positive count is useful for file controls.
cmd_stream() {
  local dev="$1" ledger="$2" round="$3" count="$4" seq=0 off sum op
  need_dev "$dev" stream
  local blocks; blocks=$(blocks_of "$dev")
  (( blocks > 1 )) || die "stream: too few blocks"
  local d; d=$(mktemp -d "$(dirname "$ledger")/stream.XXXXXX")
  # Keep an open reference: a removed/renumbered /dev name must never redirect
  # this writer onto a different device. Stop this writer before reconnecting.
  exec 9<>"$dev"
  local bound="/proc/$$/fd/9"
  while (( count == 0 || seq < count )); do
    off=$(( (RANDOM * 32768 + RANDOM) % (blocks - 1) * bs ))
    head -c $bs /dev/urandom > "$d/block"
    sum=$(sha256sum < "$d/block" | cut -d' ' -f1)
    op="$round-$BASHPID-$seq"
    ledger_line "$ledger" intent "$op" "$round" "$off" "$sum"
    if write_one "$bound" "$off" "$d/block"; then
      ledger_line "$ledger" ack "$op" "$round" "$off" "$sum"
      printf '%s\n' "$op" > "$(dirname "$ledger")/writer-ready"
    else
      echo "stream: $op unacknowledged" >&2
      sleep 0.2
    fi
    seq=$((seq + 1))
  done
}

cmd_writer_ready() {
  local tag="$1" end=$(( SECONDS + $2 )) w="$root/$1"
  check_tag "$tag"
  while :; do
    unit_active "$(unit_of "$tag" writer)" || die "writer exited before the fault"
    [[ ! -s "$w/writer-ready" ]] || { echo "writer-ready: $(cat "$w/writer-ready")"; return 0; }
    (( SECONDS < end )) || die "writer did not acknowledge its first write"
    sleep 0.2
  done
}

cmd_writer_stop() {
  local tag="$1" u
  check_tag "$tag"; u=$(unit_of "$tag" writer)
  unit_active "$u" || die "writer was not running through power-off"
  timeout 45 "$systemctl_bin" stop "$u" || die "writer stop is unknown; retained"
  ! unit_active "$u" || die "writer is still alive; no reconnect or verify"
  echo "writer stopped after power-off"
}

# Verify the last ACK or a later unacked intent, with each required round
# having at least one ACK. Unrelated contents and unmatched ACKs always fail.
cmd_verify() {
  local dev="$1" ledger="$2"; shift 2
  need_dev "$dev" verify
  [[ -s "$ledger" ]] || die "verify: empty ledger $ledger (nothing acknowledged)"
  python3 - "$dev" "$ledger" "$iflag" "$@" <<'PY'
import hashlib,re,subprocess,sys
dev,ledger,iflag,*required=sys.argv[1:]
intents={};acked=set();values={};has_ack=set();rounds={}
for line in open(ledger):
    parts=line.split()
    if len(parts)!=6: raise SystemExit('verify: malformed ledger')
    kind,op,round,offset,length,sha=parts
    if not offset.isdecimal() or int(offset)%4096 or length!='4096' or not re.fullmatch('[0-9a-f]{64}',sha):
        raise SystemExit('verify: malformed range/hash')
    off=int(offset); fact=(round,off,sha)
    if kind=='intent':
        if op in intents: raise SystemExit('verify: duplicate intent')
        intents[op]=fact
        if off in has_ack: values[off].add(sha)
    elif kind=='ack':
        if intents.get(op)!=fact or op in acked: raise SystemExit('verify: unmatched/duplicate ack')
        acked.add(op);has_ack.add(off);values[off]={sha}
        rounds[round]=rounds.get(round,0)+1
    else: raise SystemExit('verify: unknown ledger record')
for round in required:
    if not rounds.get(round): raise SystemExit('verify: no acknowledged write of round '+round+' in the ledger')
if not has_ack: raise SystemExit('verify: no acknowledged offsets')
bad=0
for off in sorted(has_ack):
    read=subprocess.run(['dd','if='+dev,'bs=4096','count=1','skip='+str(off//4096),'iflag='+iflag,'status=none'],capture_output=True)
    if read.returncode or len(read.stdout)!=4096: raise SystemExit('verify: device read failed at '+str(off))
    got=hashlib.sha256(read.stdout).hexdigest()
    if got not in values[off]:
        print('MISMATCH offset='+str(off)+' got='+got+' allowed='+','.join(sorted(values[off])))
        bad+=1
print('verified='+str(len(has_ack))+' mismatches='+str(bad))
raise SystemExit(1 if bad else 0)
PY
}

# drop-control: the ledger plus one acked write the device never received
# (a random block at the reserved last block): verify must fail on it.
cmd_drop_control() {
  local dev="$1" ledger="$2" ctl
  need_dev "$dev" drop-control
  ctl=$(mktemp)
  trap 'rm -f "$ctl"' RETURN
  cp "$ledger" "$ctl"
  local blocks off
  blocks=$(blocks_of "$dev")
  off=$(( (blocks - 1) * bs ))
  local sum; sum=$(head -c $bs /dev/urandom | sha256sum | cut -d' ' -f1)
  ledger_line "$ctl" intent dropped-control dropped "$off" "$sum"
  ledger_line "$ctl" ack dropped-control dropped "$off" "$sum"
  if cmd_verify "$dev" "$ctl" >/dev/null 2>&1; then
    die "drop-control: verify passed with a dropped acked write: the check cannot fail"
  fi
  echo "drop-control: verify failed on the dropped write, as it must"
}

# no-ack: a write in the window must not be acknowledged within SECONDS
# (RF2 after the failover, before the rejoin: quorum 2 with one copy up).
# Passes only on a write still in flight at the deadline, or one the device
# refused with an I/O error. A missing device, or dd failing for any other
# reason, is a setup failure. The write goes to the reserved last block and
# is not waited for (a write in uninterruptible wait cannot be killed).
cmd_no_ack() {
  local dev="$1" ledger="$2" secs="$3" d tmp off rc
  need_dev "$dev" no-ack
  d=$(mktemp -d "$(dirname "$ledger")/no-ack.XXXXXX")
  head -c $bs /dev/urandom > "$d/block"
  off=$(( ($(blocks_of "$dev") - 1) * bs ))
  local op="window-$BASHPID" sum; sum=$(sha256sum < "$d/block" | cut -d' ' -f1)
  ledger_line "$ledger" intent "$op" window "$off" "$sum"
  ( set +e; "$dd_bin" if="$d/block" of="$dev" bs=$bs count=1 seek=$(( off / bs )) oflag="$oflag" conv=notrunc status=none 2>"$d/err" &
    child=$!; echo "$child" > "$d/pid"; wait "$child"
    echo $? > "$d/rc.tmp"; mv "$d/rc.tmp" "$d/rc" ) </dev/null >/dev/null 2>&1 &
  local pid=$! end=$(( SECONDS + secs ))
  while [[ ! -e "$d/rc" ]] && (( SECONDS < end )); do sleep 0.2; done
  if [[ ! -e "$d/rc" ]]; then
    # The waiter retains the receipt if this write completes after the window.
    # Cleanup owns d/pid and refuses to purge while its I/O is still alive.
    need_dev "$dev" "no-ack (after the window)"
    echo "no-ack: no acknowledgement in ${secs}s (the write is still in flight at the reserved offset $off)"
    return 0
  fi
  rc=$(cat "$d/rc")
  if (( rc == 0 )); then
    ledger_line "$ledger" ack "$op" window "$off" "$sum"
    rm -rf "$d"
    die "no-ack: a write was ACKNOWLEDGED in the window (a FINDING: the volume ran on one copy)"
  fi
  if grep -qE 'Input/output error|Remote I/O error' "$d/err"; then
    need_dev "$dev" "no-ack (after the refusal)"
    echo "no-ack: the write was refused (rc=$rc: $(head -c 200 "$d/err"))"
    rm -rf "$d"
    return 0
  fi
  local err; err=$(head -c 300 "$d/err"); rm -rf "$d"
  die "no-ack: the write failed for another reason (rc=$rc: $err): a setup or tool failure, not a refusal"
}

field() { # JSON KEY: a top-level scalar
  python3 -c 'import json,sys; v=json.load(sys.stdin).get(sys.argv[1]); print("" if v is None else v)' "$1"
}

role_of() { # STATUS -> "role epoch healthy replication"
  local j
  j=$(timeout "${R4_STATUS_TIMEOUT:-10}" bash -c "$1") || return 1
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
      if [[ "$want" == replica && "$1" == primary && "$3" == True ]]; then
        die "wait-role: old node is primary and healthy at epoch $2"
      fi
      if [[ "$2" == "$epoch" ]]; then
        if [[ "$want" == primary && "$1" == primary && "$3" == True ]]; then echo "primary at $epoch"; return 0; fi
        if [[ "$want" == replica && "$1" != primary && "$4" == replica_ready ]]; then echo "replica_ready at $epoch"; return 0; fi
      fi
    fi
    (( SECONDS < end )) || die "wait-role: not $want at epoch $epoch in time (last: ${r:-unreadable})"
    sleep "${R4_POLL:-2}"
  done
}

cmd_client_check() {
  command -v nvme >/dev/null || die "client-check: nvme-cli is not installed"
  modprobe nvme_tcp || die "client-check: cannot load nvme_tcp"
  [[ -d /sys/module/nvme_tcp ]] || die "client-check: nvme_tcp is not loaded"
  local mp; mp=$(cat /sys/module/nvme_core/parameters/multipath 2>/dev/null || echo absent)
  [[ "$mp" == Y ]] || die "client-check: native NVMe multipath is off ($mp)"
  echo "client-check: nvme-cli $(nvme version | head -1), nvme_tcp loaded, multipath=$mp"
}

# dev NQN ADDR SECONDS: exactly one subsystem carries NQN, it has a live
# controller at ADDR and exactly one namespace; print that namespace's device.
# Anything else (none, two, a stale path) fails: the run never guesses a name.
cmd_dev() {
  local nqn="$1" addr="$2" end=$(( SECONDS + $3 ))
  local sys="${R4_SYS:-/sys/class/nvme-subsystem}" devdir="${R4_DEVDIR:-/dev}"
  local why
  while :; do
    why=""
    local subs=() s c n
    for s in "$sys"/*; do
      [[ -f "$s/subsysnqn" ]] || continue
      [[ "$(cat "$s/subsysnqn")" == "$nqn" ]] && subs+=("$s")
    done
    if (( ${#subs[@]} != 1 )); then
      why="${#subs[@]} subsystems carry $nqn"
    else
      s="${subs[0]}"
      local live=0 nss=()
      for c in "$s"/*; do
        n=$(basename "$c")
        if [[ "$n" =~ ^nvme[0-9]+$ && -f "$c/state" && -f "$c/address" ]]; then
          if [[ "$(cat "$c/state")" == live ]] && tr ',' '\n' < "$c/address" | grep -qxF "traddr=$addr"; then live=1; fi
        elif [[ "$n" =~ ^nvme[0-9]+n[0-9]+$ ]]; then
          nss+=("$n")
        fi
      done
      if (( live == 0 )); then why="no live path to $addr for $nqn"
      elif (( ${#nss[@]} != 1 )); then why="${#nss[@]} namespaces under $nqn"
      elif [[ ! -b "$devdir/${nss[0]}" ]] && ! [[ "${R4_ALLOW_FILE:-}" == 1 && -e "$devdir/${nss[0]}" ]]; then why="$devdir/${nss[0]} is not a block device"
      else echo "$devdir/${nss[0]}"; return 0
      fi
    fi
    (( SECONDS < end )) || die "dev: $why"
    sleep "${R4_POLL:-1}"
  done
}

cmd_connect() {
  local nqn="$1" addr="$2" port="$3" secs="$4"
  timeout 30 nvme disconnect -n "$nqn" >/dev/null 2>&1 || true
  local args=(connect -t tcp -a "$addr" -s "$port" -n "$nqn" --fast_io_fail_tmo=5 --ctrl-loss-tmo=60)
  # The action transcript records these fault-window settings in the bundle.
  { printf 'nvme'; printf ' %q' "${args[@]}"; printf '\n'; } >&2
  timeout 60 nvme "${args[@]}" >&2 || die "connect: nvme connect $addr:$port $nqn failed"
  cmd_dev "$nqn" "$addr" "$secs"
}

check_tag() { [[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || die "bad tag '$1'"; }
unit_of() { echo "r4-$1-$2"; } # TAG ROLE

# A unit that is loaded in any form (a transient one too) is not absent.
unit_active() { # UNIT: active, activating, deactivating or reloading
  local st rc
  if st=$("$systemctl_bin" is-active "$1" 2>/dev/null); then rc=0; else rc=$?; fi
  case "$st" in
    active|activating|deactivating|reloading) return 0 ;;
    inactive|failed|unknown) [[ "$rc" == 0 || "$rc" == 3 || "$rc" == 4 ]] && return 1 ;;
  esac
  die "cannot establish unit state for $1 (rc=$rc, $st); retained"
}

cmd_install() {
  local pkg="$1" tag="$2" mip="$3" w r u
  check_tag "$tag"; w="$root/$tag"
  [[ ! -e "$w" ]] || die "install: $w exists: not fresh work"
  for r in master volume writer; do
    u=$(unit_of "$tag" "$r")
    [[ ! -e "$unitdir/$u.service" ]] || die "install: $unitdir/$u.service exists: not fresh work"
    ! unit_active "$u" || die "install: $u is running: not fresh work"
  done
  ! grep -qE '(^|[[:space:]])blockmaster([[:space:]]|$)' "$hosts" || die "install: $hosts already maps blockmaster"
  (cd "$pkg" && sha256sum -c --quiet SHA256SUMS) || die "install: the package does not match its SHA256SUMS"
  mkdir -p "$root"
  cp -a "$pkg" "$w"
  printf '%s\n' "$tag" > "$w/.r4-owner"
  chmod 0755 "$w"
  echo "$mip blockmaster # r4-$tag" >> "$hosts"
  sync
  echo "install: $w fresh, package checked, blockmaster -> $mip"
}

cmd_spec() {
  local tag="$1" rf="$2" n1="$3" n2="$4" n3="$5" dp="$6" cp="$7"
  check_tag "$tag"
  local out="$root/$tag/cluster-spec.yaml"
  node() { printf '  - server_id: %s\n    data_addr: %s:%s\n    ctrl_addr: %s:%s\n' "$1" "$2" "$dp" "$2" "$cp"; }
  place() { printf '      - server_id: %s\n        replica_id: %s\n' "$1" "$2"; }
  case "$rf" in
    3) { echo nodes:; node s1 "$n1"; node s2 "$n2"; node s3 "$n3"
         printf 'volumes:\n  - id: v1\n    size_bytes: 268435456\n    replication_factor: 3\n    placements:\n'
         place s2 r1; place s1 r2; place s3 r3; } > "$out" ;;
    2) { echo nodes:; node s2 "$n2"; node s3 "$n3"
         printf 'volumes:\n  - id: v1\n    size_bytes: 268435456\n    replication_factor: 2\n    placements:\n'
         place s2 r1; place s3 r2; } > "$out" ;;
    *) die "spec: rf must be 3 or 2" ;;
  esac
  sync
  echo "$out"
}

# unit TAG ROLE -- EXEC ARG...: the run's persistent unit. Written once
# (fresh work), synced to disk before the VM can lose power.
cmd_unit() {
  local tag="$1" role="$2"; shift 2
  check_tag "$tag"
  [[ "$role" == master || "$role" == volume || "$role" == writer ]] || die "unit: invalid role"
  [[ "${1:-}" == "--" ]] || die "unit TAG ROLE -- EXEC ARG..."; shift
  (( $# > 0 )) && [[ "$1" == /* ]] || die "unit: EXEC must be an absolute path"
  local a
  for a in "$@"; do
    [[ "$a" =~ ^[A-Za-z0-9_./:=,@+-]+$ ]] || die "unit: argument '$a' needs quoting; refused"
  done
  local u; u=$(unit_of "$tag" "$role")
  local f="$unitdir/$u.service"
  [[ ! -e "$f" ]] || die "unit: $f exists: not fresh work"
  cat > "$f" <<EOF
# R4 run unit ($tag $role), written by scenarios/v3block/r4.sh; removed by r4.sh cleanup.
[Unit]
Description=R4 run $tag $role
After=network-online.target
StartLimitIntervalSec=600
StartLimitBurst=10

[Service]
Type=simple
ExecStart=$*
KillSignal=SIGTERM
TimeoutStopSec=30s
Restart=on-failure
RestartPreventExitStatus=78
RestartSec=5s
EOF
  sync
  if [[ "$role" == writer ]]; then
    printf '\n# Writer is never restarted or allowed to outlive its fault window.\nRestart=no\nRuntimeMaxSec=600\n' >> "$f"
    sync
  fi
  "$systemctl_bin" daemon-reload
  echo "unit: $f"
}

cmd_start() {
  local tag="$1" role="$2" u
  check_tag "$tag"; u=$(unit_of "$tag" "$role")
  [[ -f "$unitdir/$u.service" ]] || die "start: $unitdir/$u.service is missing"
  "$systemctl_bin" start "$u"
  echo "start: $u $("$systemctl_bin" is-active "$u" 2>/dev/null || true)"
}

# cleanup TAG: stop the run's units, keep each one's status and journal (and
# the run's own files) in WORK/evidence, remove the units and the blockmaster
# mapping, then prove nothing of the run is left running.
cmd_cleanup() {
  local tag="$1" w ev r u f bad=0
  check_tag "$tag"; w="$root/$tag"; ev="$w/evidence"
  [[ -f "$w/.r4-owner" && "$(cat "$w/.r4-owner")" == "$tag" ]] || die "cleanup: no ownership proof for $w; retained"
  rm -f "$w/.cleanup-ok"
  mkdir -p "$ev"
  for r in master volume writer; do
    u=$(unit_of "$tag" "$r")
    if [[ -e "$unitdir/$u.service" ]] || unit_active "$u"; then
      "$systemctl_bin" stop "$u" || echo "cleanup: stop $u failed" >&2
      "$systemctl_bin" status "$u" --no-pager > "$ev/$u.status" 2>&1 || true
      "$journalctl_bin" -u "$u" --no-pager > "$ev/$u.journal" 2>&1 || true
    fi
    rm -f "$unitdir/$u.service"
  done
  "$systemctl_bin" daemon-reload || true
  for f in ledger failover.txt verify-after-failover.txt verify-after-rejoin.txt no-ack.txt cluster-spec.yaml SHA256SUMS; do
    [[ -f "$w/$f" ]] && cp "$w/$f" "$ev/"
  done
  if [[ -f "$hosts" ]] && grep -q "# r4-$tag\$" "$hosts"; then
    local keep; keep=$(grep -v "# r4-$tag\$" "$hosts" || true)
    printf '%s\n' "$keep" > "$hosts"
  fi
  # The absence proof.
  for r in master volume writer; do
    u=$(unit_of "$tag" "$r")
    [[ ! -e "$unitdir/$u.service" ]] || { echo "cleanup: $u.service still present" >&2; bad=1; }
    if unit_active "$u"; then echo "cleanup: $u is still $("$systemctl_bin" is-active "$u" 2>/dev/null)" >&2; bad=1; fi
  done
  local prc
  if "$pgrep_bin" -f -- "$w/bin/" >/dev/null 2>&1; then prc=0; else prc=$?; fi
  case "$prc" in
    0) echo "cleanup: a process from $w/bin is still running" >&2; bad=1 ;;
    1) : ;;
    *) echo "cleanup: process query failed ($prc); retained" >&2; bad=1 ;;
  esac
  for f in "$w"/no-ack.*/pid; do
    [[ -f "$f" ]] || continue
    local pid; pid=$(cat "$f")
    [[ "$pid" =~ ^[0-9]+$ ]] || { bad=1; continue; }
    if kill -0 "$pid" 2>/dev/null; then
      # A reused pid is never killed: the input file must still name this run.
      if tr '\0' ' ' < "/proc/$pid/cmdline" | grep -qF "${f%/pid}/block"; then
        kill -TERM "$pid" 2>/dev/null || true
      fi
      local end=$(( SECONDS + 5 ))
      while kill -0 "$pid" 2>/dev/null && (( SECONDS < end )); do sleep 0.2; done
      if kill -0 "$pid" 2>/dev/null; then echo "cleanup: pending window I/O $pid remains; retained" >&2; bad=1; fi
    fi
  done
  if grep -q "# r4-$tag\$" "$hosts" 2>/dev/null; then echo "cleanup: the blockmaster mapping is still in $hosts" >&2; bad=1; fi
  (( bad == 0 )) || die "cleanup: the run's services are not gone"
  printf '%s\n' "$tag" > "$w/.cleanup-ok"
  echo "cleanup: r4-$tag units stopped and absent; evidence in $ev"
}

cmd_purge() {
  local tag="$1" w
  check_tag "$tag"; w="$root/$tag"
  [[ -f "$w/.cleanup-ok" ]] || die "purge: no successful cleanup proof; retained"
  cmd_cleanup "$tag" # Recheck absence immediately before deleting owned work.
  rm -rf "${w:?}" "$scratch/r4-$tag.tgz" "$scratch/r4-$tag-x"
  echo "purge: $w removed"
}

case "${1:-}" in
  write) shift; [[ $# -eq 4 ]] || die "write DEV LEDGER ROUND COUNT"; cmd_write "$@" ;;
  stream) shift; [[ $# -eq 4 ]] || die "stream DEV LEDGER ROUND COUNT"; cmd_stream "$@" ;;
  writer-ready) shift; [[ $# -eq 2 ]] || die "writer-ready TAG SECONDS"; cmd_writer_ready "$@" ;;
  writer-stop) shift; [[ $# -eq 1 ]] || die "writer-stop TAG"; cmd_writer_stop "$@" ;;
  verify) shift; [[ $# -ge 2 ]] || die "verify DEV LEDGER [ROUND...]"; cmd_verify "$@" ;;
  drop-control) shift; [[ $# -eq 2 ]] || die "drop-control DEV LEDGER"; cmd_drop_control "$@" ;;
  no-ack) shift; [[ $# -eq 3 ]] || die "no-ack DEV LEDGER SECONDS"; cmd_no_ack "$@" ;;
  held) shift; cmd_held "$@" ;;
  wait-role) shift; [[ $# -eq 4 ]] || die "wait-role STATUS ROLE EPOCH SECONDS"; cmd_wait_role "$@" ;;
  client-check) shift; cmd_client_check ;;
  dev) shift; [[ $# -eq 3 ]] || die "dev NQN ADDR SECONDS"; cmd_dev "$@" ;;
  connect) shift; [[ $# -eq 4 ]] || die "connect NQN ADDR PORT SECONDS"; cmd_connect "$@" ;;
  install) shift; [[ $# -eq 3 ]] || die "install PKG TAG MASTER_IP"; cmd_install "$@" ;;
  spec) shift; [[ $# -eq 7 ]] || die "spec TAG RF N1 N2 N3 DPORT CPORT"; cmd_spec "$@" ;;
  unit) shift; [[ $# -ge 4 ]] || die "unit TAG ROLE -- EXEC ARG..."; cmd_unit "$@" ;;
  start) shift; [[ $# -eq 2 ]] || die "start TAG ROLE"; cmd_start "$@" ;;
  cleanup) shift; [[ $# -eq 1 ]] || die "cleanup TAG"; cmd_cleanup "$@" ;;
  purge) shift; [[ $# -eq 1 ]] || die "purge TAG"; cmd_purge "$@" ;;
  *) die "usage: r4.sh write|stream|writer-ready|writer-stop|verify|drop-control|no-ack|held|wait-role|client-check|dev|connect|install|spec|unit|start|cleanup|purge ..." ;;
esac
