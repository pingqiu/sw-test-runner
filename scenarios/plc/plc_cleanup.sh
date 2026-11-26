#!/usr/bin/env bash
# plc_cleanup.sh DIR TOKEN ARTIFACTS: remove this invocation's run directory only once everything that decided
# the run is collected. For each of evidence/, capture/ and lout/ that exists under DIR, ARTIFACTS/plc-<name>.tgz
# must exist, read as a gzip tar, and list exactly the files DIR/<name> holds. Otherwise DIR is kept (exit 1,
# reason printed): a failed or partial collection never deletes judgment evidence.
set -uf -o pipefail
d=$1 tok=$2 art=$3
here=$(cd "$(dirname "$0")" && pwd)
[ -e "$d" ] || { echo "cleanup: $d is already gone"; exit 0; }
bash "$here/plc_owned.sh" "$d" "$tok" || exit 1
for sub in evidence capture lout; do
  [ -d "$d/$sub" ] || continue
  a="$art/plc-$sub.tgz"
  [ -f "$a" ] || { echo "cleanup: keeping $d: $sub/ was not collected ($a missing)" >&2; exit 1; }
  got=$(tar -tzf - < "$a" | grep -v '/$' | sed 's#^\./##' | LC_ALL=C sort) || { echo "cleanup: keeping $d: $a does not read" >&2; exit 1; }
  want=$(cd "$d" && find "$sub" -type f | LC_ALL=C sort)
  [ "$got" = "$want" ] || { echo "cleanup: keeping $d: $a does not hold exactly $sub/" >&2; exit 1; }
done
rm -rf -- "$d"
echo "cleanup: removed $d (evidence, capture and lout collected)"
