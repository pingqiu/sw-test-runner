#!/usr/bin/env bash
# Self-test of scripts/public-text-check.sh: a scratch repository holding one
# file per case; each internal name must be caught, and the placeholders that
# replace them must pass.
set -euo pipefail
check=$(cd "$(dirname "$0")/.." && pwd)/public-text-check.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0

# expect WANT_EXIT NAME CONTENT
expect() {
	local want=$1 name=$2 content=$3 got=0
	rm -rf "$tmp/r"
	git init -q "$tmp/r"
	printf '%s\n' "$content" >"$tmp/r/file.md"
	git -C "$tmp/r" add file.md
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
expect 0 "roles and placeholders" "the reviewer and the lead; ssh 198.51.100.184; the run's share folder; /mnt/share/runs; tp010 is a model number"

if ((fail == 0)); then echo "public-text-check: 9 cases ok"; fi
exit "$fail"
