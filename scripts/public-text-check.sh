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
pat='(claude|codex)0[0-9]|\btp0[0-9]\b|192\.168\.[0-9]+\.[0-9]+|[Vv]:[/\\]+share|share[/\\]+rdma[/\\]|diskstation'
rc=0
git grep -I -n -i -E "$pat" "$tree" -- . \
	':(exclude)scripts/public-text-check.sh' ':(exclude)scripts/tests/test_public_text_check.sh' || rc=$?
case $rc in
0) echo "public-text: internal names found (above); replace them with roles, documentation addresses or placeholders" >&2; exit 1 ;;
1) echo "public-text: clean"; exit 0 ;;
*) echo "public-text: git grep failed ($rc)" >&2; exit 2 ;;
esac
