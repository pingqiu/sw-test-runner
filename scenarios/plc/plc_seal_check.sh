#!/usr/bin/env bash
# plc_seal_check.sh EVIDENCE CLASS PACKET SEALS CONFIG PRODUCER TOOLS HEAD PYTHON: the seal check of a power-loss run.
# Always writes EVIDENCE/packet.seal, EVIDENCE/seal.txt (sealed=yes|no) and EVIDENCE/tools.sha256 (the sha256
# of every executed tool: this script, plc_judge.py, plc_gate.sh, plc_settle.sh, and the producer files).
# A rehearsal stops there (never counted). dry/counted require, before any cloud call: the packet's SHA256SUMS
# hash is an `s <sha>` line in SEALS; no UNSEALED; every packet file verifies; drill/plc-run-pins.json pins
# CONFIG's sha256, PRODUCER's SHA256SUMS hash, TOOLS' plc-tools.sha256 hash, and HEAD (the product commit);
# every producer file and every tool verifies. Any miss exits 1.
set -euf -o pipefail
e=$1 class=$2 packet=$3 seals=$4 config=$5 producer=$6 tools=$7 head=$8 py=$9
fail() { echo "seal_check: $*" >&2; exit 1; }
[ -f "$packet/SHA256SUMS" ] && [ -f "$packet/powerloss.py" ] && [ -f "$packet/drill/powerloss-fs.json" ] \
  || fail "not a power-loss packet: $packet"
seal=$(sha256sum < "$packet/SHA256SUMS" | cut -c1-64)
printf '%s\n' "$seal" > "$e/packet.seal"
{ (cd "$tools" && sha256sum plc_seal_check.sh plc_judge.py plc_gate.sh plc_settle.sh) 2>/dev/null || true
  [ -f "$producer/SHA256SUMS" ] && sed 's/^/producer: /' "$producer/SHA256SUMS" || true; } > "$e/tools.sha256"
sealed=no
if [ -n "$seals" ] && [ -f "$seals" ] && grep -qE "^s $seal( |\$)" "$seals"; then sealed=yes; fi
[ -e "$packet/UNSEALED" ] && sealed=no
printf 'sealed=%s\n' "$sealed" > "$e/seal.txt"
if [ "$class" = rehearsal ]; then
  echo "rehearsal: packet seal $seal sealed=$sealed (never counted)"
  exit 0
fi
[ "$sealed" = yes ] || fail "packet $seal is not an approved seal, or carries UNSEALED"
(cd "$packet" && sha256sum -c --quiet SHA256SUMS) > "$e/packet-verify.txt" 2>&1 || fail 'packet files do not verify (packet-verify.txt)'
pins="$packet/drill/plc-run-pins.json"
[ -f "$pins" ] || fail 'sealed packet has no drill/plc-run-pins.json'
# One JSON reader; a key that is absent or not of its shape reads as empty and fails below.
pin() { "$py" -I -c 'import json,re,sys
v = json.load(open(sys.argv[1])).get(sys.argv[2], "")
shape = {"product_head": "[0-9a-f]{40}"}.get(sys.argv[2], "[0-9a-f]{64}")
sys.stdout.write(v if isinstance(v, str) and re.fullmatch(shape, v) else "")' "$pins" "$1"; }
match() { # match NAME PIN GOT
  [ -n "$2" ] && [ "$3" = "$2" ] || fail "$1 $3 is not the sealed pin ${2:-<none>}"
}
[ -f "$config" ] || fail "cloud_config missing: $config"
match 'cloud config sha256' "$(pin config_sha256)" "$(sha256sum < "$config" | cut -c1-64)"
[ -f "$producer/SHA256SUMS" ] || fail "producer has no SHA256SUMS: $producer"
match 'producer SHA256SUMS' "$(pin producer_sha256)" "$(sha256sum < "$producer/SHA256SUMS" | cut -c1-64)"
(cd "$producer" && sha256sum -c --quiet SHA256SUMS) > "$e/producer-verify.txt" 2>&1 || fail 'producer files do not verify (producer-verify.txt)'
[ -f "$tools/plc-tools.sha256" ] || fail "tools have no plc-tools.sha256: $tools"
match 'tools manifest' "$(pin tools_sha256)" "$(sha256sum < "$tools/plc-tools.sha256" | cut -c1-64)"
(cd "$tools" && sha256sum -c --quiet plc-tools.sha256) > "$e/tools-verify.txt" 2>&1 || fail 'tools do not verify (tools-verify.txt)'
match 'product head' "$(pin product_head)" "$head"
echo "seal $seal approved; config, producer, tools and product pins ok; class=$class"
