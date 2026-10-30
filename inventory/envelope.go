package inventory

import (
	"fmt"
	"time"
)

func validateEnvelope(d *Document) error {
	w, e := time.Parse(time.RFC3339Nano, d.Deadlines.Work)
	h, f := time.Parse(time.RFC3339Nano, d.Deadlines.Hard)
	if e != nil || f != nil || w.Format(time.RFC3339Nano) != d.Deadlines.Work || h.Format(time.RFC3339Nano) != d.Deadlines.Hard || w.Location() != time.UTC || h.Location() != time.UTC || d.Deadlines.CleanupReserveMS <= 0 || d.Deadlines.CleanupReserveMS > 60000 || w.Add(time.Duration(d.Deadlines.CleanupReserveMS)*time.Millisecond).After(h) {
		return fmt.Errorf("invalid original work/hard deadline or cleanup reserve")
	}
	for _, host := range d.Hosts {
		names, ports := map[string]bool{}, map[int]bool{}
		for _, s := range host.Services {
			if !oneOf(s.Name, "master_http", "master_grpc", "filer_http", "filer_grpc", "volume_http", "volume_grpc", "s3_http", "rdma", "rdma_control", "dc_control", "kv_coordinator") || names[s.Name] || ports[s.Port] || s.Port < 1 || s.Port > 65535 {
				return fmt.Errorf("invalid or colliding service port on %s", host.ID)
			}
			names[s.Name], ports[s.Port] = true, true
		}
		for _, role := range host.Roles {
			for _, name := range map[string][]string{"master": {"master_http", "master_grpc"}, "filer": {"filer_http", "filer_grpc"}, "volume": {"volume_http", "volume_grpc"}, "s3": {"s3_http"}}[role] {
				if !names[name] {
					return fmt.Errorf("%s requires explicit %s service", host.ID, name)
				}
			}
		}
		if d.Transport != "http" && has(host.Roles, "volume") && (!names["rdma"] || !names["rdma_control"] || d.Transport == "dc" && !names["dc_control"]) {
			return fmt.Errorf("RDMA volume requires explicit RDMA service port")
		}
	}
	return nil
}
func sameServices(a, b []Service) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s.Name] = s.Port
	}
	for _, s := range b {
		if m[s.Name] != s.Port {
			return false
		}
	}
	return true
}
