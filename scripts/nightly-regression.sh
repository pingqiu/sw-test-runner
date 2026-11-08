#!/usr/bin/env bash
# Workstation stages inbox/{input.json,mono.tar.gz,runner.tar,native.tar}; no credentials here.
set -euo pipefail
script_dir=$(cd -- "$(dirname -- "$0")" && pwd)
if [[ ${1:-} == cell ]]; then
  work=$2 out=$3 cell=$4 sha=$5 image=$6
  name=$7; echo "$name" > "$out/container"
  echo container > "$out/phase"
  docker run -d --pull=never --name "$name" --label "seaweed.nightly=$name" --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock -v /sys/fs/cgroup:/sys/fs/cgroup:rw -v "$work:/input:ro" "$image" /sbin/init
  docker exec "$name" sh -ec 'mkdir /root/controller; cd /root/controller; ln -s /input/artifacts artifacts; install -d -m700 -o testdev -g testdev /home/testdev/.ssh; ssh-keygen -q -t ed25519 -N "" -f key; cat key.pub >> /home/testdev/.ssh/authorized_keys; chown testdev:testdev /home/testdev/.ssh/authorized_keys; chmod 600 /home/testdev/.ssh/authorized_keys; cp /etc/ssh/ssh_host_ed25519_key.pub host.pub'
  echo prepare > "$out/phase"
  docker exec "$name" python3 /input/runner/scripts/nightly-regression.py prepare /root/controller /input/runner/inventory/testdata/http.requested.json "$cell" "$sha"
  timeout -k5 30 docker exec "$name" sh -ec 'until systemctl is-active --quiet ssh; do sleep 1; done'
  echo cli > "$out/phase"
  docker exec "$name" /input/runner/bin/seaweed-cloud direct --inventory /root/controller/inventory.json --config /root/controller/config.json --out /root/controller/result --timeout 25m
  exit
fi
root=$(realpath -m -- "${1:?usage: nightly-regression.sh ROOT}")
mkdir -p "$root"; exec 9>"$root/lock"; flock -n 9 || exit 0
[[ ! -e $root/cleanup-required ]]
today=$(date -u +%F); sha=$(jq -er .mono "$root/inbox/input.json")
[[ $sha =~ ^[0-9a-f]{40}$ && $(jq -r .night "$root/inbox/input.json") == "$today" ]]
if [[ -f $root/last ]] && grep -qxE "$sha|$today" "$root/last"; then exit 0; fi
image=$(jq -er .image "$root/inbox/input.json")
out="$root/results/$sha"; work="$root/work"; mkdir -p "$out"
[[ ! -e $work && ! -L $work && ! -f $out/TABLE.md ]]
cp "$root/inbox/input.json" "$out/input.json"
printf '%s\n%s\n' "$sha" "$today" > "$root/last.tmp"; mv "$root/last.tmp" "$root/last"
cells=(ec-go ec-rust ec-2a master-writes volume-restart c1 c2 c4 c5 ec-reconstruction-go ec-reconstruction-rust)
stage=build; current=; failed=0; mkdir -p "$work" "$root/cache"; : > "$out/rows"
dispose() {
  local name=$1 left
  left=$(timeout -k5 30 docker ps -a --filter "name=^/$name$" --format '{{.Names}}' 2>> "$out/cleanup.log") || return 1
  [[ -n $left ]] || return 0
  left=$(timeout -k5 30 docker inspect --format '{{index .Config.Labels "seaweed.nightly"}}' "$name" 2>> "$out/cleanup.log") || return 1
  [[ $left == "$name" ]] || return 1
  timeout -k5 90 docker rm -f "$name" >> "$out/cleanup.log" 2>&1 || return 1
  left=$(timeout -k5 30 docker ps -a --filter "name=^/$name$" --format '{{.Names}}' 2>> "$out/cleanup.log") || return 1
  [[ -z $left ]]
}
finish() {
  local rc=$?; trap - EXIT; set +e
  if [[ -n $current ]]; then dispose "$current" || { touch "$root/cleanup-required"; rc=1; }; fi
  for cell in "${cells[@]}"; do
    grep -q "^| $cell |" "$out/rows" || printf '| %s | NOT_RUN | %s | — |\n' "$cell" "$stage: exit $rc" >> "$out/rows"
  done
  if [[ ! -f $root/cleanup-required ]]; then timeout -k5 90 rm -rf -- "$work" || rc=1; [[ ! -e $work ]] || rc=1; fi
  { printf 'Mono %s; overall job completion is in EXIT.\n\n| Selector | Verdict | First failing row | Wall s |\n|---|---|---|---:|\n' "$sha"; cat "$out/rows"; } > "$out/TABLE.md"
  (cd "$out"; timeout -k5 90 bash -o pipefail -c 'find . -type f ! -name SHA256SUMS ! -name EXIT -print0 | sort -z | xargs -0 sha256sum') > "$root/sums.tmp" || rc=1
  mv "$root/sums.tmp" "$out/SHA256SUMS" || rc=1
  echo "$rc" > "$out/EXIT"
  exit "$rc"
}
trap finish EXIT; trap 'exit 143' TERM INT
sha256sum "$root/inbox/mono.tar.gz" "$root/inbox/runner.tar" "$root/inbox/native.tar" > "$out/sources.sha256"
stage=ENVIRONMENT; echo ENVIRONMENT > "$out/CLASSIFICATION"
docker_root=$(timeout -k5 30 docker info --format '{{.DockerRootDir}}' 2>"$out/disk-preflight.stderr") || exit 78
printf '%s\n' "$docker_root" > "$out/docker-root"
[[ $docker_root == /* ]] || exit 78
timeout -k5 30 df -P -- /data/nvme "$docker_root" > "$out/disk-preflight.txt" 2>> "$out/disk-preflight.stderr" || exit 78
awk 'NR>1 {n++; if ($5 !~ /^[0-9]+%$/ || $5+0>85) bad=1} END {exit !(n==2 && !bad)}' "$out/disk-preflight.txt" || exit 78
timeout -k5 30 docker image inspect "$image" --format '{{.Id}}' > "$out/image-id" 2> "$out/image-inspect.stderr" || exit 78
rm "$out/CLASSIFICATION"; stage=build
for source in product runner; do mkdir "$work/$source"; done
timeout -k5 90 tar -xzf "$root/inbox/mono.tar.gz" --strip-components=1 -C "$work/product"
timeout -k5 90 tar -xf "$root/inbox/runner.tar" -C "$work/runner"
export GOCACHE="$root/cache/go" GOMODCACHE="$root/cache/mod" GOPATH="$root/cache/gopath" GOTOOLCHAIN=local CGO_ENABLED=0 GOFLAGS='-p=2 -mod=readonly' GOMAXPROCS=2
export CARGO_HOME="$root/cache/cargo" CARGO_TARGET_DIR="$work/target" CARGO_BUILD_JOBS=4 TMPDIR="$work/tmp" GOTMPDIR="$work/tmp"
mkdir -p "$work/artifacts" "$TMPDIR"
timeout -k10 900 make -C "$work/runner" build > "$out/build-controller.log" 2>&1
(cd "$work/product/enterprise"; timeout -k10 900 go build -tags=5BytesOffset -o "$work/artifacts/weed" ./weed) > "$out/build-go.log" 2>&1
(cd "$work/product/enterprise/seaweed-volume"; timeout -k30 2400 cargo build --release --locked --bin weed-volume) > "$out/build-rust.log" 2>&1
cp "$work/target/release/weed-volume" "$work/artifacts/weed-volume"
cp "$root/inbox/native.tar" "$work/artifacts/native.tar"
sha256sum "$work/artifacts/"* "$work/runner/bin/seaweed-cloud" > "$out/artifacts.sha256"
for cell in "${cells[@]}"; do
  stage=$cell; dir="$out/$cell"; mkdir "$dir"; start=$SECONDS; rc=0
  current="sw-reg-${sha:0:12}-$(date -u +%s)-$$-$cell"
  timeout -k5 1650 bash "$script_dir/nightly-regression.sh" cell "$work" "$dir" "$cell" "$sha" "$image" "$current" > "$dir/stdout" 2> "$dir/stderr" || rc=$?
  failure=$(cat "$dir/phase")
  for item in result inventory.json; do timeout -k5 90 docker cp "$current:/root/controller/$item" "$dir/$item" >> "$dir/collect.log" 2>&1 || { if [[ $rc == 0 ]]; then failure=collect; rc=1; fi; }; done
  timeout -k5 30 docker exec "$current" sh -ec 'ps -eo pid,ppid,args; ss -Hltn' > "$dir/processes-ports.txt" 2>&1 || { if [[ $rc == 0 ]]; then failure=observation; rc=1; fi; }
  cleanup=0; dispose "$current" || { cleanup=1; touch "$root/cleanup-required"; }; current=
  if [[ $cleanup != 0 && $rc == 0 ]]; then failure=cleanup; fi
  verdict=PASS; row=—; [[ $rc == 0 && $cleanup == 0 ]] || { verdict=FAIL; row="$failure: exit $rc, cleanup $cleanup"; failed=1; }
  printf '| %s | %s | %s | %s |\n' "$cell" "$verdict" "$row" "$((SECONDS-start))" >> "$out/rows"
  [[ $cleanup == 0 ]] || exit 1
done
exit "$failed"
