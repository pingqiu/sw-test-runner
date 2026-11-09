package seaweed

// The two read-only RPCs below use the field numbers in mono 49daaba1's
// weed/pb/master.proto. The HTTP volume status and volume.list text omit the
// serving EC generation; file presence must not stand in for that heartbeat.
import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
)

type maintenanceMasterShard struct {
	Endpoint   string `json:"endpoint"`
	VID        string `json:"vid"`
	Collection string `json:"collection"`
	Bits       uint64 `json:"bits"`
	Generation uint64 `json:"generation"`
	EncodeNS   string `json:"encode_ns"`
	Pending    bool   `json:"pending"`
	Protocol   uint64 `json:"protocol"`
}
type maintenanceLicense struct {
	Valid      bool   `json:"valid"`
	ID         string `json:"id"`
	Expiry     string `json:"expiry"`
	CapacityTB uint64 `json:"capacity_tb"`
}
type maintenanceWire struct{}

func (maintenanceWire) Name() string { return "proto" }
func (maintenanceWire) Marshal(v any) ([]byte, error) {
	p, ok := v.(*[]byte)
	if !ok {
		return nil, errors.New("maintenance RPC request type")
	}
	return *p, nil
}
func (maintenanceWire) Unmarshal(b []byte, v any) error {
	p, ok := v.(*[]byte)
	if !ok || len(b) > 2<<20 {
		return errors.New("maintenance RPC response type/cap")
	}
	*p = append((*p)[:0], b...)
	return nil
}

type maintenancePBField struct {
	n      protowire.Number
	kind   protowire.Type
	bytes  []byte
	number uint64
}

func maintenancePB(b []byte) ([]maintenancePBField, error) {
	var fields []maintenancePBField
	for len(b) > 0 {
		n, k, s := protowire.ConsumeTag(b)
		if s < 0 {
			return nil, errors.New("malformed maintenance protobuf tag")
		}
		b = b[s:]
		f := maintenancePBField{n: n, kind: k}
		switch k {
		case protowire.BytesType:
			f.bytes, s = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			f.number, s = protowire.ConsumeVarint(b)
		default:
			s = protowire.ConsumeFieldValue(n, k, b)
		}
		if s < 0 {
			return nil, errors.New("malformed maintenance protobuf value")
		}
		b = b[s:]
		fields = append(fields, f)
	}
	return fields, nil
}
func maintenancePBSingle(fields []maintenancePBField, n protowire.Number, k protowire.Type) (maintenancePBField, error) {
	f := maintenancePBField{n: n, kind: k}
	seen := false
	for _, x := range fields {
		if x.n == n {
			if seen || x.kind != k {
				return f, errors.New("ambiguous maintenance protobuf scalar")
			}
			f = x
			seen = true
		}
	}
	return f, nil
}
func maintenancePBBytes(fields []maintenancePBField, n protowire.Number) ([]byte, error) {
	f, e := maintenancePBSingle(fields, n, protowire.BytesType)
	return f.bytes, e
}
func maintenancePBNumber(fields []maintenancePBField, n protowire.Number) (uint64, error) {
	f, e := maintenancePBSingle(fields, n, protowire.VarintType)
	return f.number, e
}
func maintenanceMasterRPC(ctx context.Context, address, method string, req []byte) ([]byte, error) {
	if until, ok := ctx.Deadline(); !ok || time.Until(until) < 15*time.Second {
		return nil, errors.New("maintenance master observation cannot fit 15s maximum")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, e := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		return nil, e
	}
	defer conn.Close()
	var out []byte
	prefix := "/master_pb.Seaweed/"
	switch method {
	case "GetMasterConfiguration", "VolumeList":
	case "VolumeEcShardsInfo":
		prefix = "/volume_server_pb.VolumeServer/"
	default:
		return nil, errors.New("unadmitted maintenance RPC")
	}
	e = conn.Invoke(ctx, prefix+method, &req, &out, grpc.ForceCodec(maintenanceWire{}), grpc.MaxCallRecvMsgSize(2<<20))
	return out, e
}

func maintenanceCheckHolder(ctx context.Context, address string, s maintenanceScope) error {
	v, e := strconv.ParseUint(s.VID, 10, 32)
	if e != nil || v == 0 {
		return errors.New("invalid serving EC VID")
	}
	raw, e := maintenanceMasterRPC(ctx, address, "VolumeEcShardsInfo", protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), v))
	if e != nil {
		return e
	}
	f, e := maintenancePB(raw)
	if e != nil {
		return e
	}
	active, e1 := maintenancePBNumber(f, 100)
	low, e2 := maintenancePBNumber(f, 101)
	cfg, e3 := maintenancePBBytes(f, 10)
	if e = errors.Join(e1, e2, e3); e != nil {
		return e
	}
	config, e := maintenancePB(cfg)
	if e != nil {
		return e
	}
	encode, e := maintenancePBNumber(config, 3)
	if e != nil {
		return e
	}
	if active != uint64(s.Generation) || low != active || strconv.FormatUint(encode, 10) != s.EncodeNS {
		return errors.New("holder serving EC generation differs from files")
	}
	seen := map[int]bool{}
	for _, field := range f {
		if field.n != 1 {
			continue
		}
		shard, e := maintenancePB(field.bytes)
		if e != nil {
			return e
		}
		id, e1 := maintenancePBNumber(shard, 1)
		vid, e2 := maintenancePBNumber(shard, 4)
		collection, e3 := maintenancePBBytes(shard, 3)
		if e = errors.Join(e1, e2, e3); e != nil {
			return e
		}
		if id >= 14 || vid != v || string(collection) != s.Collection || seen[int(id)] {
			return errors.New("foreign/duplicate serving EC shard")
		}
		seen[int(id)] = true
	}
	if len(seen) != len(s.Shards) {
		return errors.New("serving EC shard set differs from files")
	}
	for _, id := range s.Shards {
		if !seen[id] {
			return errors.New("unmounted EC shard file")
		}
	}
	return nil
}
func maintenanceReadLicense(ctx context.Context, address string) (maintenanceLicense, error) {
	var l maintenanceLicense
	raw, e := maintenanceMasterRPC(ctx, address, "GetMasterConfiguration", nil)
	if e != nil {
		return l, e
	}
	f, e := maintenancePB(raw)
	if e != nil {
		return l, e
	}
	raw, e = maintenancePBBytes(f, 11)
	if e != nil {
		return l, e
	}
	f, e = maintenancePB(raw)
	if e != nil {
		return l, e
	}
	v, e := maintenancePBNumber(f, 1)
	if e != nil || v > 1 {
		return l, errors.New("invalid master license validity")
	}
	l.Valid = v == 1
	id, e1 := maintenancePBBytes(f, 6)
	expiry, e2 := maintenancePBBytes(f, 4)
	capacity, e3 := maintenancePBNumber(f, 5)
	l.ID, l.Expiry, l.CapacityTB = string(id), string(expiry), capacity
	if e = errors.Join(e1, e2, e3); e != nil {
		return l, e
	}
	if l.ID == "" || l.CapacityTB == 0 {
		return l, errors.New("master license identity/capacity absent")
	}
	if _, e = time.Parse("2006-01-02", l.Expiry); e != nil {
		return l, errors.New("master license expiry unreadable")
	}
	return l, nil
}
func maintenanceReadShards(ctx context.Context, address, collection, vid string) ([]maintenanceMasterShard, error) {
	v, e := strconv.ParseUint(vid, 10, 32)
	if e != nil || v == 0 {
		return nil, errors.New("invalid EC observation VID")
	}
	req := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte(collection))
	req = protowire.AppendVarint(protowire.AppendTag(req, 2, protowire.VarintType), v)
	raw, e := maintenanceMasterRPC(ctx, address, "VolumeList", req)
	if e != nil {
		return nil, e
	}
	f, e := maintenancePB(raw)
	if e != nil {
		return nil, e
	}
	top, e := maintenancePBBytes(f, 1)
	if e != nil || len(top) == 0 {
		return nil, errors.New("EC master topology absent")
	}
	var out []maintenanceMasterShard
	// topology -> data center -> rack -> data node; all repeated field 2.
	var walk func([]byte, int) error
	walk = func(raw []byte, depth int) error {
		fields, e := maintenancePB(raw)
		if e != nil {
			return e
		}
		if depth < 3 {
			for _, f := range fields {
				if f.n == 2 {
					if f.kind != protowire.BytesType {
						return errors.New("invalid EC topology child")
					}
					if e = walk(f.bytes, depth+1); e != nil {
						return e
					}
				}
			}
			return nil
		}
		endpoint, e := maintenancePBBytes(fields, 4)
		if e != nil {
			return e
		}
		if len(endpoint) == 0 {
			endpoint, e = maintenancePBBytes(fields, 1)
			if e != nil {
				return e
			}
		}
		for _, disk := range fields {
			if disk.n != 2 {
				continue
			}
			entry, e := maintenancePB(disk.bytes)
			if e != nil {
				return e
			}
			body, e := maintenancePBBytes(entry, 2)
			if e != nil {
				return e
			}
			di, e := maintenancePB(body)
			if e != nil {
				return e
			}
			for _, shard := range di {
				if shard.n != 7 {
					continue
				}
				sf, e := maintenancePB(shard.bytes)
				if e != nil {
					return e
				}
				id, e1 := maintenancePBNumber(sf, 1)
				name, e2 := maintenancePBBytes(sf, 2)
				bits, e3 := maintenancePBNumber(sf, 3)
				generation, e4 := maintenancePBNumber(sf, 10)
				pending, e5 := maintenancePBNumber(sf, 11)
				protocol, e6 := maintenancePBNumber(sf, 13)
				encode, e7 := maintenancePBNumber(sf, 14)
				if e = errors.Join(e1, e2, e3, e4, e5, e6, e7); e != nil {
					return e
				}
				if id != v || string(name) != collection {
					return errors.New("foreign EC topology row")
				}
				if len(endpoint) == 0 || bits == 0 || bits >= 1<<14 || pending > 1 || encode == 0 {
					return errors.New("incomplete EC heartbeat identity")
				}
				out = append(out, maintenanceMasterShard{string(endpoint), vid, collection, bits, generation, strconv.FormatUint(encode, 10), pending == 1, protocol})
			}
		}
		return nil
	}
	if e = walk(top, 0); e != nil {
		return nil, fmt.Errorf("EC heartbeat: %w", e)
	}
	return out, nil
}
