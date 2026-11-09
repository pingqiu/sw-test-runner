#!/bin/bash
# Stage 1 mount soak rehearsal on a build host: real weed, services and mounts each in their own network namespace, so
# the partition's DROP rule never touches the host. usage: mount_soak_rehearse.sh WEED WORKER_DIR ROOT A B C E1 E2 DUAL
set -u
WEED=$1 WORKER=$2 ROOT=$3 A=$4 B=$5 C=$6 E1=$7 E2=$8 DUAL=$9
SVC=swms-svc MNT=swms-mnt SIP=10.77.0.1 MIP=10.77.0.2
cleanup() {
  sudo -n pkill -f "$ROOT/mnt" 2>/dev/null
  sudo -n ip netns pids $MNT 2>/dev/null | xargs -r sudo -n kill -9 2>/dev/null
  sudo -n ip netns pids $SVC 2>/dev/null | xargs -r sudo -n kill -9 2>/dev/null
  grep -F "$ROOT/mnt" /proc/mounts | awk '{print $2}' | xargs -r -n1 sudo -n umount -l 2>/dev/null
  sudo -n ip netns del $MNT 2>/dev/null; sudo -n ip netns del $SVC 2>/dev/null
  # weed also listens on /tmp Unix sockets, which network namespaces do not isolate: one rehearsal at a time.
  rm -f /tmp/seaweedfs-filer-grpc-28888.sock /tmp/seaweedfs-master-grpc-29333.sock /tmp/seaweedfs-volume-grpc-28080.sock
}
trap cleanup EXIT
cleanup
mkdir -p "$ROOT/data" "$ROOT/mnt"
sudo -n ip netns add $SVC; sudo -n ip netns add $MNT
sudo -n ip link add swms0 type veth peer name swms1
sudo -n ip link set swms0 netns $SVC; sudo -n ip link set swms1 netns $MNT
sudo -n ip netns exec $SVC sh -c "ip addr add $SIP/24 dev swms0; ip link set swms0 up; ip link set lo up"
sudo -n ip netns exec $MNT sh -c "ip addr add $MIP/24 dev swms1; ip link set swms1 up; ip link set lo up"
sudo -n ip netns exec $SVC sudo -n -u testdev "$WEED" server -ip=$SIP -dir="$ROOT/data" -filer -master.port=19333 -volume.port=18080 \
  -filer.port=18888 -volume.max=24 -master.volumeSizeLimitMB=32 > "$ROOT/server.log" 2>&1 &
for i in $(seq 60); do sudo -n ip netns exec $MNT curl -fs -m 2 http://$SIP:18888/ >/dev/null 2>&1 && break; sleep 1; done
CFG=$(python3 -c "import json,sys;print(json.dumps(dict(root='$ROOT',weed='$WEED',filer='$SIP:18888',filer_path='/mountsoak',volume_host='$SIP',volume_port=18080,phases_s=[$A,$B,$C],episodes_s=[$E1,$E2],iptables=['sudo','-n','ip','netns','exec','$MNT'],mounts=4,dual_append_s=$DUAL,write_budget_bytes=150*1024*1024)))")
sudo -n ip netns exec $MNT sudo -n -u testdev python3 "$WORKER/mount_soak.py" --config "$CFG" > "$ROOT/load.out" 2> "$ROOT/load.err"
echo "load rc=$?"; tail -n 3 "$ROOT/load.out" | cut -c1-300; tail -n 3 "$ROOT/load.err" | cut -c1-300
python3 "$WORKER/mount_assess.py" --root "$ROOT" | python3 -c "
import sys,json
v=json.load(sys.stdin); print('STATUS',v['status'],'cleanup',v['cleanup'])
for k,r in v['rows'].items(): print(' ',k,r['outcome'],r['reason'][:3])
print('gates',v['gate_reasons'][:8]); print('proven',v['violations_proven'])"
