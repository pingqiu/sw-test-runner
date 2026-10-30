package series

import (
	"math"
	"path"
	"strings"

	"github.com/pingqiu/sw-test-runner/packs/resources/gomemory"
)

const pinned = "SeaweedFS_rdma_range_pinned_bytes"

var retained = []string{"SeaweedFS_rdma_range_retained_bytes", "SeaweedFS_rdma_range_retained_sources"}
var pools = []string{pinned, "SeaweedFS_rdma_cold_pool_bytes", "SeaweedFS_rdma_cold_pool_slots"}

func storage(s Server) (float64, bool) {
	var total, cache float64
	nc := 0
	valid := true
	if s.Data == nil {
		valid = false
	}
	for n, p := range s.Data {
		v, ok := number(p)
		if !ok {
			valid = false
		}
		total += v
		if strings.HasPrefix(path.Base(n), "lmcache_") {
			cache += v
			nc++
		}
	}
	dir, ok := number(s.VolumeDir)
	aux, good := number(s.Auxiliary)
	return total, valid && ok && good && nc <= 8 && cache <= 256*gomemory.MiB && len(s.Data) <= 24 && total <= 768*gomemory.MiB && dir <= 960*gomemory.MiB && aux <= 128*gomemory.MiB
}
func bounds(in Input) map[string]Check {
	disk := Check{Status: "FAIL"}
	budget := Check{Status: "NOT_OBSERVED"}
	var pinStates []string
	all := len(in.Samples) > 0
	for _, sample := range in.Samples {
		s := sample.Server
		data, ok := storage(s)
		all = all && ok
		if disk.MaxData == nil {
			disk.MaxData = new(float64)
			disk.MaxAux = new(float64)
		}
		*disk.MaxData = math.Max(*disk.MaxData, data)
		aux, good := number(s.Auxiliary)
		if good {
			*disk.MaxAux = math.Max(*disk.MaxAux, aux)
		}
		pin, ok := number(s.Gauges[pinned])
		observed := ok && len(s.Budgets) > 0
		var cap float64
		for _, b := range s.Budgets {
			v, valid := number(b.Bytes)
			observed = observed && valid
			cap = math.Max(cap, v)
		}
		state := "NOT_OBSERVED"
		if observed {
			state = "PASS"
			if pin > cap {
				state = "FAIL"
			}
		}
		pinStates = append(pinStates, state)
	}
	if all {
		disk.Status = "PASS"
	}
	budget.Status = aggregate(pinStates)
	q := Check{Status: "PASS"}
	names := append(append([]string{}, retained...), pools...)
	for _, n := range names {
		base, b := number(in.Final.Baseline.RDMA[n])
		end, e := number(in.Final.RDMA[n])
		if !b || !e {
			q.Status = "FAIL"
			break
		}
		if n == retained[0] || n == retained[1] {
			if base != 0 || end != 0 {
				q.Status = "FAIL"
			}
		} else if base != end {
			q.Status = "FAIL"
		}
	}
	return map[string]Check{"server_storage_bounds": disk, "reported_pin_budget": budget, "quiescence_baseline_return": q}
}
