#!/usr/bin/env bash
# Refuses internal names in the repository's tracked text. This repository is
# public: internal seat names, lab host names, lab network addresses and
# internal file-share paths do not belong in it. Use roles ("the reviewer"),
# documentation addresses (RFC 5737, e.g. 198.51.100.x) and placeholders
# ("the run's share folder", /mnt/share) instead.
#
# Usage: public-text-check.sh [TREE]   (default HEAD; any git tree-ish)
# Exit 0: clean. Exit 1: at least one hit (each printed). Exit 2: usage.
#
# The patterns live only here and in this check's own test; both files are
# excluded from the scan.
set -euo pipefail
tree=${1:-HEAD}
git rev-parse -q --verify "$tree^{tree}" >/dev/null || { echo "public-text: unknown tree $tree" >&2; exit 2; }
names='(claude|codex)0[0-9]|\btp0[0-9]\b|[Vv]:[/\\]+share|share[/\\]+rdma[/\\]|diskstation'
lab='192\.168\.[0-9]+\.[0-9]+'
# Allowlist, for now: scenarios/ and suites/ still name the lab machines by
# address (private RFC 1918 addresses, not secrets); they become run
# parameters in a later change. Everywhere else a lab address is refused.
self=(':(exclude)scripts/public-text-check.sh' ':(exclude)scripts/tests/test_public_text_check.sh')

# scan PATTERN PATHSPEC...: prints hits; returns 0 hit, 1 none, 2 error.
scan() {
	local pat=$1 rc=0
	shift
	git grep -I -n -i -E "$pat" "$tree" -- "$@" || rc=$?
	[ "$rc" -le 1 ] || { echo "public-text: git grep failed ($rc)" >&2; return 2; }
	return "$rc"
}

hit=0
rc=0
scan "$names|$lab" . "${self[@]}" ':(exclude)scenarios/**' ':(exclude)suites/**' || rc=$?
[ "$rc" -le 1 ] || exit 2
[ "$rc" -eq 0 ] && hit=1
rc=0
scan "$names" 'scenarios/**' 'suites/**' || rc=$?
[ "$rc" -le 1 ] || exit 2
[ "$rc" -eq 0 ] && hit=1

if [ "$hit" -eq 1 ]; then
	echo "public-text: internal names found (above); replace them with roles, documentation addresses or placeholders" >&2
	exit 1
fi
echo "public-text: clean"
