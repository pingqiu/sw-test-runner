package seaweed

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
)

func maintenancePBTestBytes(n protowire.Number, b []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, n, protowire.BytesType), b)
}
func maintenancePBTestNumber(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}
func TestMaintenanceMasterReadOnlyRPC(t *testing.T) {
	license := maintenancePBTestNumber(1, 1)
	license = append(license, maintenancePBTestBytes(4, []byte("2026-12-09"))...)
	license = append(license, maintenancePBTestBytes(6, []byte("DEFAULT-ENT-25TB"))...)
	license = append(license, maintenancePBTestNumber(5, 25)...)
	shard := maintenancePBTestNumber(1, 47)
	for n, s := range map[protowire.Number]string{2: "own", 14: ""} {
		if n == 2 {
			shard = append(shard, maintenancePBTestBytes(n, []byte(s))...)
		}
	}
	for n, v := range map[protowire.Number]uint64{3: 1, 10: 2, 13: 1, 14: 1234} {
		shard = append(shard, maintenancePBTestNumber(n, v)...)
	}
	disk := maintenancePBTestBytes(7, shard)
	entry := append(maintenancePBTestBytes(1, []byte("ssd")), maintenancePBTestBytes(2, disk)...)
	node := append(maintenancePBTestBytes(4, []byte("127.0.0.1:2345")), maintenancePBTestBytes(2, entry)...)
	top := maintenancePBTestBytes(2, maintenancePBTestBytes(2, maintenancePBTestBytes(2, node)))
	var response []byte
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	server := grpc.NewServer(grpc.ForceServerCodec(maintenanceWire{}))
	method := func(name string) grpc.MethodDesc {
		return grpc.MethodDesc{MethodName: name, Handler: func(_ any, _ context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			var raw []byte
			if e := decode(&raw); e != nil {
				return nil, e
			}
			if name == "VolumeList" {
				want := append(maintenancePBTestBytes(1, []byte("own")), maintenancePBTestNumber(2, 47)...)
				if string(raw) != string(want) {
					t.Error("unfiltered EC observation request")
				}
			}
			copy := append([]byte(nil), response...)
			return &copy, nil
		}}
	}
	server.RegisterService(&grpc.ServiceDesc{ServiceName: "master_pb.Seaweed", HandlerType: (*interface{})(nil), Methods: []grpc.MethodDesc{method("GetMasterConfiguration"), method("VolumeList")}}, struct{}{})
	server.RegisterService(&grpc.ServiceDesc{ServiceName: "volume_server_pb.VolumeServer", HandlerType: (*interface{})(nil), Methods: []grpc.MethodDesc{method("VolumeEcShardsInfo")}}, struct{}{})
	go server.Serve(listener)
	defer server.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response = maintenancePBTestBytes(11, license)
	l, e := maintenanceReadLicense(ctx, listener.Addr().String())
	if e != nil || !l.Valid || l.ID != "DEFAULT-ENT-25TB" || l.Expiry != "2026-12-09" || l.CapacityTB != 25 {
		t.Fatalf("license %+v %v", l, e)
	}
	response = maintenancePBTestBytes(1, top)
	rows, e := maintenanceReadShards(ctx, listener.Addr().String(), "own", "47")
	if e != nil || len(rows) != 1 || rows[0].Generation != 2 || rows[0].EncodeNS != "1234" || rows[0].Endpoint != "127.0.0.1:2345" {
		t.Fatalf("heartbeat %+v %v", rows, e)
	}
	for _, bad := range [][]byte{{0x0a, 0xff}, append(response, response...), maintenancePBTestBytes(1, nil)} {
		response = bad
		if _, e = maintenanceReadShards(ctx, listener.Addr().String(), "own", "47"); e == nil {
			t.Fatalf("ambiguous/empty heartbeat accepted: %x", bad)
		}
	}
	info := append(maintenancePBTestNumber(1, 0), maintenancePBTestNumber(4, 47)...)
	info = append(info, maintenancePBTestBytes(3, []byte("own"))...)
	good := maintenancePBTestBytes(1, info)
	good = append(good, maintenancePBTestBytes(10, maintenancePBTestNumber(3, 1234))...)
	good = append(good, maintenancePBTestNumber(100, 2)...)
	good = append(good, maintenancePBTestNumber(101, 2)...)
	scope := maintenanceScope{maintenanceSubject: maintenanceSubject{VID: "47", Collection: "own", Generation: 2, EncodeNS: "1234", Shards: []int{0}}}
	response = good
	if e = maintenanceCheckHolder(ctx, listener.Addr().String(), scope); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []maintenanceScope{
		{maintenanceSubject: maintenanceSubject{VID: "47", Collection: "own", Generation: 1, EncodeNS: "1234", Shards: []int{0}}},
		{maintenanceSubject: maintenanceSubject{VID: "47", Collection: "own", Generation: 2, EncodeNS: "old", Shards: []int{0}}},
		{maintenanceSubject: maintenanceSubject{VID: "47", Collection: "own", Generation: 2, EncodeNS: "1234", Shards: []int{1}}},
	} {
		if e = maintenanceCheckHolder(ctx, listener.Addr().String(), bad); e == nil {
			t.Fatalf("foreign serving identity accepted: %+v", bad)
		}
	}
}
