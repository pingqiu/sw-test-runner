#!/usr/bin/env bash
# plc_settle.sh EVIDENCE CLASS CLOUD CONFIG PYTHON [AZ]: prove a power-loss run's cloud teardown. Runs from an
# ALWAYS phase, so a run that timed out or was cancelled is still verified. Nothing to settle (exit 0, recorded)
# for a rehearsal, or when cloud_run never started (no EVIDENCE/started.txt). Otherwise, all of these must hold:
#   - the config's variables file is the one it pins (variables_sha256), and it names subscription_id and
#     resource_group_name: the independent listing uses exactly those, never the ambient Azure login;
#   - `seaweed-cloud verify --config` exits 0 and its JSON has remaining_resource_ids and create_unresolved as
#     lists, both empty (a verify output without them is a refusal, not "nothing left");
#   - the resource group exists in that subscription and lists no resources.
set -uf -o pipefail
e=$1 class=$2 cloud=$3 config=$4 py=$5 az=${6:-az}
fail() { echo "settle: $*" | tee -a "$e/settle.txt" >&2; exit 1; }
if [ "$class" = rehearsal ] || [ ! -f "$e/started.txt" ]; then
  echo "settle: no cloud run to settle ($class)" > "$e/settle.txt"
  exit 0
fi
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
"$cloud" verify --config "$config" --json > "$e/verify.json" 2>> "$e/settle.log"
vrc=$?; printf '%s\n' "$vrc" > "$e/verify.exit"
"$az" group exists --subscription "$sub" --name "$rg" --only-show-errors > "$e/group-exists.txt" 2>> "$e/settle.log"
grc=$?
"$az" resource list --subscription "$sub" --resource-group "$rg" --output json --only-show-errors > "$e/resources-after.json" 2>> "$e/settle.log"
arc=$?; printf '%s\n' "$arc" > "$e/resources-after.exit"
[ "$vrc" = 0 ] || fail "seaweed-cloud verify exited $vrc"
[ "$grc" = 0 ] && [ "$(tr -d '\r\n ' < "$e/group-exists.txt")" = true ] || fail "resource group $rg does not exist in the run's subscription"
[ "$arc" = 0 ] || fail 'independent resource listing failed'
"$py" -I - "$e/verify.json" "$e/resources-after.json" <<'PY' || fail 'resources remain, or verify output lacks its keys'
import json, sys
v = json.load(open(sys.argv[1])); left = json.load(open(sys.argv[2]))
keys = ('remaining_resource_ids', 'create_unresolved')
if not all(isinstance(v.get(k), list) for k in keys):
    sys.exit('verify.json lacks %s as lists' % ' / '.join(keys))
if any(v[k] for k in keys) or not isinstance(left, list) or left:
    sys.exit('REMAINING: verify %r, listed %r' % ({k: v[k] for k in keys}, left))
PY
echo "settle: teardown verified in $rg" > "$e/settle.txt"
