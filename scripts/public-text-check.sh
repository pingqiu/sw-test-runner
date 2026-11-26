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
# Allowlist, for now: files whose lab addresses something actually dials
# (scenarios, suites, the example scenarios, the CI wrapper scripts' defaults,
# the controller's dashboard default, the docs site's wiki links). They are
# private RFC 1918 addresses, not secrets, and become required run
# parameters in a later change. Everywhere else a lab address is refused.
lab_allow=('scenarios/**' 'suites/**' 'examples/v3-testops/scenarios/**' 'scripts/run-*-ci.sh'
	'scripts/testops-ci-worker.sh' 'cmd/testops-controller/main.go' 'mkdocs.yml')
self=(':(exclude)scripts/public-text-check.sh' ':(exclude)scripts/tests/test_public_text_check.sh')
excl=()
for p in "${lab_allow[@]}"; do excl+=(":(exclude,glob)$p"); done

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
scan "$names|$lab" . "${self[@]}" "${excl[@]}" || rc=$?
[ "$rc" -le 1 ] || exit 2
[ "$rc" -eq 0 ] && hit=1
rc=0
# The two phase-20 suites write their evidence to the lab share (save_to):
# that one share path is allowed there, for now, with the addresses.
share_allow=('suites/phase20-t6-stage0.yaml' 'suites/phase20-t6-stage1.yaml')
names_but_share='(claude|codex)0[0-9]|\btp0[0-9]\b|share[/\]+rdma[/\]|diskstation'
allow=()
for p in "${lab_allow[@]}"; do allow+=(":(glob)$p"); done
for p in "${share_allow[@]}"; do allow+=(":(exclude)$p"); done
scan "$names" "${allow[@]}" || rc=$?
[ "$rc" -le 1 ] || exit 2
[ "$rc" -eq 0 ] && hit=1
rc=0
scan "$names_but_share" "${share_allow[@]}" || rc=$?
[ "$rc" -le 1 ] || exit 2
[ "$rc" -eq 0 ] && hit=1

if [ "$hit" -eq 1 ]; then
	echo "public-text: internal names found (above); replace them with roles, documentation addresses or placeholders" >&2
	exit 1
fi
echo "public-text: clean"
