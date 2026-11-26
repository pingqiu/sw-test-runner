#!/usr/bin/env bash
# plc_settle.sh RUNEV OUT CLASS CLOUD CONFIG PYTHON TOKEN [AZ]: prove a power-loss run's cloud teardown.
# RUNEV is the run's evidence directory (cloud_run's markers); results go to OUT (RUNEV itself, or a
# subdirectory for the always pass). Runs as an ordinary phase and again as an always phase, so a run that
# timed out or was cancelled is still settled. In order:
#   1. nothing to settle (exit 0, recorded) for a rehearsal, or when this invocation launched no cloud child
#      (no RUNEV/launching);
#   2. terminal barrier: the cloud child must have exited. RUNEV/cloud.pid is the pid and start time of
#      plc_launch.sh, recorded before the child started; the launcher exits only after the child. Waits up to
#      PLC_SETTLE_WAIT seconds (default 600). Still running, or
#      launched with no recorded identity: UNKNOWN (exit 1) with the recovery handle; inventory is never
#      observed while the child can still create or delete;
#   3. admission: the config's output holds launch.json with this invocation's launch TOKEN (seaweed-cloud
#      writes it only after admission). Output still absent: not admitted, nothing of ours exists (exit 0,
#      recorded, nothing touched). Output present without our token: UNKNOWN (exit 1), untouched;
#   4. admitted: verify plus an independent listing in the subscription and resource group that the config's
#      pinned variables name. verify's Result must be the real contract: schema seaweed.cloud-smoke/v1, code
#      EMPTY, remaining_resource_ids an empty list, remaining_observed true, create_unresolved false
#      (booleans, not truthiness); the group must exist and list no resources.
set -uf -o pipefail
ev=$1 o=$2 class=$3 cloud=$4 config=$5 py=$6 token=$7 az=${8:-az}
wait_s=${PLC_SETTLE_WAIT:-600}
fail() { echo "settle: $*" | tee -a "$o/settle.txt" >&2; exit 1; }
done_() { echo "settle: $*" > "$o/settle.txt"; exit 0; }
[ "$class" = rehearsal ] && done_ 'no cloud run to settle (rehearsal)'
[ -f "$ev/launching" ] || done_ "no cloud run was launched by this invocation ($class)"

# 2. Terminal barrier.
alive() { local st f; read -r st < "/proc/$1/stat" 2>/dev/null || return 1; f=(${st##*) }); [ "${f[19]}" = "$2" ]; }
waited=0
while [ ! -s "$ev/cloud.pid" ] && [ "$waited" -lt 10 ]; do sleep 1; waited=$((waited + 1)); done
read -r pid start < "$ev/cloud.pid" 2>/dev/null \
  || fail "UNKNOWN: a cloud child was launched but recorded no identity; recovery: seaweed-cloud verify/destroy --config $config"
case "$pid$start" in *[!0-9]*|'') fail "UNKNOWN: cloud.pid is malformed; recovery: seaweed-cloud verify/destroy --config $config" ;; esac
waited=0
while alive "$pid" "$start" && [ "$waited" -lt "$wait_s" ]; do sleep 2; waited=$((waited + 2)); done
if alive "$pid" "$start"; then
  printf 'launcher_pid=%s launcher_start=%s cloud_pid=%s config=%s\n' "$pid" "$start" "$(cat "$ev/cloud.pid.child" 2>/dev/null)" "$config" > "$o/recovery-handle.txt"
  fail "UNKNOWN: cloud child (launcher pid $pid) still running after ${waited}s; not verified; recovery handle in recovery-handle.txt"
fi
echo "cloud child pid $pid terminal (waited ${waited}s)" > "$o/barrier.txt"

# 3. Admission.
out=$("$py" -I -c 'import json,sys; v=json.load(open(sys.argv[1])).get("output"); sys.stdout.write(v if isinstance(v, str) else "")' "$config") \
  && [ -n "$out" ] || fail 'config has no output'
if [ ! -e "$out" ]; then
  done_ "not admitted: $out was never created, so no cloud operation of this invocation exists"
fi
"$py" -I - "$out/launch.json" "$token" <<'PY' || fail "UNKNOWN: $out exists but holds no launch receipt for this invocation; untouched"
import json, sys
try:
    v = json.load(open(sys.argv[1]))
except (OSError, ValueError):
    sys.exit(1)
sys.exit(0 if v.get('schema') == 'seaweed.cloud-launch/v1' and v.get('launch_token') == sys.argv[2] else 1)
PY
echo "admitted: $out/launch.json carries this invocation's launch token" > "$o/admission.txt"

# 4. Verify, in the pinned scope.
scope=$("$py" -I - "$config" <<'PY'
import hashlib, json, sys
cfg = json.load(open(sys.argv[1]))
path, want = cfg.get('variables'), cfg.get('variables_sha256')
if not isinstance(path, str) or not isinstance(want, str):
    sys.exit('config names no variables file and pin')
raw = open(path, 'rb').read()
if hashlib.sha256(raw).hexdigest() != want:
    sys.exit('variables file is not the one the config pins')
v = json.loads(raw)
sub, rg = v.get('subscription_id'), v.get('resource_group_name')
if not (isinstance(sub, str) and sub and isinstance(rg, str) and rg):
    sys.exit('variables name no subscription_id or resource_group_name')
sys.stdout.write(sub + ' ' + rg)
PY
) || fail 'cannot derive the run scope from the pinned config'
sub=${scope% *} rg=${scope#* }
"$cloud" verify --config "$config" --json > "$o/verify.json" 2>> "$o/settle.log" < /dev/null
vrc=$?; printf '%s\n' "$vrc" > "$o/verify.exit"
"$az" group exists --subscription "$sub" --name "$rg" --only-show-errors > "$o/group-exists.txt" 2>> "$o/settle.log" < /dev/null
grc=$?
"$az" resource list --subscription "$sub" --resource-group "$rg" --output json --only-show-errors > "$o/resources-after.json" 2>> "$o/settle.log" < /dev/null
arc=$?; printf '%s\n' "$arc" > "$o/resources-after.exit"
[ "$vrc" = 0 ] || fail "seaweed-cloud verify exited $vrc"
[ "$grc" = 0 ] && [ "$(tr -d '\r\n ' < "$o/group-exists.txt")" = true ] || fail "resource group $rg does not exist in the run's subscription"
[ "$arc" = 0 ] || fail 'independent resource listing failed'
"$py" -I - "$o/verify.json" "$o/resources-after.json" <<'PY' || fail 'teardown not proven (verify Result or listing)'
import json, sys
v = json.load(open(sys.argv[1])); left = json.load(open(sys.argv[2]))
want = {'schema': 'seaweed.cloud-smoke/v1', 'code': 'EMPTY'}
bad = [k for k, x in want.items() if v.get(k) != x]
if not (isinstance(v.get('remaining_resource_ids'), list) and v['remaining_resource_ids'] == []):
    bad.append('remaining_resource_ids')
if v.get('remaining_observed') is not True:
    bad.append('remaining_observed')
if v.get('create_unresolved') is not False:
    bad.append('create_unresolved')
if bad:
    sys.exit('verify Result off its contract: %s' % ', '.join(bad))
if not isinstance(left, list) or left:
    sys.exit('REMAINING in the independent listing: %r' % (left,))
PY
echo "settle: teardown verified in $rg" > "$o/settle.txt"
