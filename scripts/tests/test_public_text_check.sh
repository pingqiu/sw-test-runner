#!/usr/bin/env bash
# Self-test of scripts/public-text-check.sh: a scratch repository holding one
# file per case; each internal name must be caught, and the placeholders that
# replace them must pass. Lab addresses are allowlisted, for now, under
# scenarios/ and suites/ only.
set -euo pipefail
check=$(cd "$(dirname "$0")/.." && pwd)/public-text-check.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0
cases=0

# expect WANT_EXIT NAME CONTENT [PATH]
expect() {
	local want=$1 name=$2 content=$3 path=${4:-file.md} got=0
	cases=$((cases + 1))
	rm -rf "$tmp/r"
	git init -q "$tmp/r"
	mkdir -p "$tmp/r/$(dirname "$path")"
	printf '%s\n' "$content" >"$tmp/r/$path"
	git -C "$tmp/r" add "$path"
	git -C "$tmp/r" -c user.name=t -c user.email=t@example.invalid commit -q -m case
	(cd "$tmp/r" && bash "$check" HEAD >/dev/null 2>&1) || got=$?
	if [[ $got != "$want" ]]; then
		echo "FAIL $name: exit $got, want $want"
		fail=1
	fi
}

expect 1 "a seat name" "reviewed by codex01"
expect 1 "a capitalized seat name" "ruling by Claude05"
expect 1 "a lab host name" "the nightly runs on tp01"
expect 1 "a lab address" "ssh 192.168.1.184"
expect 1 "a Windows share path" 'see V:/share/runs'
expect 1 "a backslash share path" 'V:\share\runs\x'
expect 1 "an internal share folder" "/mnt/x/share/rdma/plan"
expect 1 "the NAS name" "copied to diskstation"
expect 0 "a CI wrapper's dialled default (allowlisted for now)" 'M2="${TESTOPS_M2:-192.168.1.184}"' scripts/run-block-ci.sh
expect 1 "a lab address in another script" "ssh 192.168.1.184" scripts/other.sh
expect 0 "the phase-20 suite's evidence share (allowlisted for now)" "  save_to: V:/share/sw-block-evidence/phase20-t6" suites/phase20-t6-stage0.yaml
expect 1 "a share path in another suite" "  save_to: V:/share/x" suites/other.yaml
expect 1 "a seat name in the allowlisted suite" "# codex01" suites/phase20-t6-stage1.yaml
expect 1 "a lab address in a NEW CI wrapper script (not allowlisted)" 'ssh user@192.168.1.184' scripts/run-new-ci.sh
expect 1 "another share path in the allowlisted phase-20 suite" "  save_to: V:/share/other/place" suites/phase20-t6-stage0.yaml
expect 0 "roles and placeholders" "the reviewer and the lead; ssh 198.51.100.184; the run's share folder; /mnt/share/runs; tp010 is a model number"
expect 0 "a lab address in a scenario (allowlisted for now)" "host: 192.168.1.184" scenarios/x.yaml
expect 0 "a lab address in a suite (allowlisted for now)" "host: 192.168.1.181" suites/x.yaml
expect 1 "a seat name in a scenario is still refused" "# reviewed by codex01" scenarios/x.yaml
expect 1 "a lab address outside scenarios/ and suites/" "ssh 192.168.1.184" docs/x.md

if ((fail == 0)); then echo "public-text-check: $cases cases ok"; fi
exit "$fail"
