#!/bin/bash
# Builds the product binaries an m2-faults run takes as prebuilt_bin, on a host that may build.
# Usage: prebuild.sh <product checkout> <runner checkout> <out dir, must not exist>
# Writes OUT/blockmaster, blockvolume, sw-block, v3ctl, OUT/product.sha (the checkout's HEAD) and
# OUT/SHA256SUMS (one line per binary). The run's build phase refuses the directory unless every
# binary matches its line, so the binaries, their hashes and the commit travel as one unit.
set -eu
SRC=${1:?product checkout}; RUNNER=${2:?runner checkout}; OUT=${3:?out dir}
[ -f "$SRC/go.mod" ] || { echo "not a product checkout: $SRC" >&2; exit 2; }
V3CTL="$RUNNER/packs/v3block/v3ctl/main.go"
[ -f "$V3CTL" ] || { echo "not a runner checkout: $RUNNER" >&2; exit 2; }
[ -e "$OUT" ] && { echo "refused: $OUT exists" >&2; exit 2; }
mkdir -p "$OUT"
for b in blockmaster blockvolume sw-block; do (cd "$SRC" && go build -o "$OUT/$b" ./cmd/$b); done
d="$SRC/.runner-v3ctl-$$"; mkdir "$d"; tail -n +2 "$V3CTL" > "$d/main.go"
(cd "$SRC" && go build -o "$OUT/v3ctl" "./$(basename "$d")"); rm -r "$d"
git -C "$SRC" rev-parse HEAD > "$OUT/product.sha"
(cd "$OUT" && for b in blockmaster blockvolume sw-block v3ctl; do printf '%s  %s\n' "$(sha256sum < "$b" | cut -c1-64)" "$b"; done > SHA256SUMS)
cat "$OUT/product.sha" "$OUT/SHA256SUMS"
