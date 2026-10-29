package gomemory

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

type value struct {
	wire uint64
	n    uint64
	b    []byte
}

func fields(data []byte) (map[uint64][]value, error) {
	out := map[uint64][]value{}
	take := func() (uint64, error) {
		v, n := binary.Uvarint(data)
		if n <= 0 {
			return 0, fmt.Errorf("truncated/oversized varint")
		}
		data = data[n:]
		return v, nil
	}
	for len(data) > 0 {
		key, e := take()
		if e != nil {
			return nil, e
		}
		num, wire := key>>3, key&7
		if num == 0 {
			return nil, fmt.Errorf("zero protobuf field")
		}
		v := value{wire: wire}
		switch wire {
		case 0:
			v.n, e = take()
			if e != nil {
				return nil, e
			}
		case 1, 2, 5:
			size := uint64(8)
			if wire == 5 {
				size = 4
			}
			if wire == 2 {
				size, e = take()
				if e != nil {
					return nil, e
				}
			}
			if size > uint64(len(data)) {
				return nil, fmt.Errorf("truncated protobuf field")
			}
			v.b = data[:int(size)]
			data = data[int(size):]
		default:
			return nil, fmt.Errorf("unsupported protobuf wire")
		}
		out[num] = append(out[num], v)
	}
	return out, nil
}
func firstInt(m map[uint64][]value, k uint64) (uint64, error) {
	v := m[k]
	if len(v) == 0 || v[0].wire != 0 {
		return 0, fmt.Errorf("missing integer field")
	}
	return v[0].n, nil
}

// InuseSpace reads canonical gzip Go pprof bytes, as in the frozen Python gate.
// The 64 MiB inflated framing limit bounds malformed evidence, not heap usage.
func InuseSpace(raw []byte) (int64, error) {
	r, e := gzip.NewReader(bytes.NewReader(raw))
	if e != nil {
		return 0, e
	}
	defer r.Close()
	const maxProfile = 64 * MiB
	data, e := io.ReadAll(io.LimitReader(r, maxProfile+1))
	if e != nil {
		return 0, e
	}
	if len(data) > maxProfile {
		return 0, fmt.Errorf("inflated profile exceeds 64 MiB")
	}
	p, e := fields(data)
	if e != nil {
		return 0, e
	}
	var strings []string
	for _, s := range p[6] {
		if s.wire != 2 || !utf8.Valid(s.b) {
			return 0, fmt.Errorf("invalid profile string")
		}
		strings = append(strings, string(s.b))
	}
	index := -1
	for i, v := range p[1] {
		if v.wire != 2 {
			return 0, fmt.Errorf("invalid value type")
		}
		m, e := fields(v.b)
		if e != nil {
			return 0, e
		}
		a, e := firstInt(m, 1)
		if e != nil {
			return 0, e
		}
		b, e := firstInt(m, 2)
		if e != nil {
			return 0, e
		}
		if a >= uint64(len(strings)) || b >= uint64(len(strings)) {
			return 0, fmt.Errorf("invalid string index")
		}
		if strings[a] == "inuse_space" && strings[b] == "bytes" {
			if index >= 0 {
				return 0, fmt.Errorf("ambiguous inuse_space type")
			}
			index = i
		}
	}
	if index < 0 || len(p[2]) == 0 {
		return 0, fmt.Errorf("missing inuse_space bytes/samples")
	}
	var total int64
	for _, sample := range p[2] {
		if sample.wire != 2 {
			return 0, fmt.Errorf("invalid sample")
		}
		m, e := fields(sample.b)
		if e != nil {
			return 0, e
		}
		var values []uint64
		for _, v := range m[2] {
			if v.wire == 0 {
				values = append(values, v.n)
				continue
			}
			if v.wire != 2 {
				return 0, fmt.Errorf("invalid sample values")
			}
			for b := v.b; len(b) > 0; {
				n, c := binary.Uvarint(b)
				if c <= 0 {
					return 0, fmt.Errorf("invalid packed varint")
				}
				values = append(values, n)
				b = b[c:]
			}
		}
		if len(values) != len(p[1]) || values[index] > math.MaxInt64 {
			return 0, fmt.Errorf("invalid sample vector")
		}
		n := int64(values[index])
		if n > math.MaxInt64-total {
			return 0, fmt.Errorf("heap sum overflow")
		}
		total += n
	}
	return total, nil
}
