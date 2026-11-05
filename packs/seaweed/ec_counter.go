package seaweed

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/pingqiu/sw-test-runner/inventory"
)

const ecMetric = "SeaweedFS_volumeServer_ec_reconstructed_interval_total"

var ecMetricSample = regexp.MustCompile(`^` + ecMetric + `\{result="(success|failure)"\} ([^ ]+)$`)

type ecCounters map[string][2]float64

func ecScrape(h inventory.Host, snap ecSnapshot, dead map[string]bool, label string, run func(string, string) (string, error)) (ecCounters, error) {
	cmd := `check() {
test "$(cat /proc/sys/kernel/random/boot_id)" = "$boot"
test "$(awk '{print $22}' /proc/$pid/stat)" = "$start"
s=$(sudo -n ss -Hltnp "sport = :$port") || return
case "$s" in *"pid=$pid,"*) ;; *) return 1;; esac
}; `
	want := map[string]bool{}
	for _, v := range snap.Holders {
		if dead[v.Job] {
			continue
		}
		i, err := strconv.Atoi(strings.TrimPrefix(v.Job, "h"))
		if err != nil || i < 0 || i >= 14 {
			return nil, errors.New("EC metrics holder invalid")
		}
		want[v.Job] = true
		cmd += fmt.Sprintf("pid=%d; start=%d; boot=%s; port=%d; check; m=$(curl -fsS --max-time 2 http://127.0.0.1:$port/metrics) || exit 1; printf '\\n# EC_HOLDER %s\\n%%s\\n' \"$m\"; check; ", v.PID, v.Start, cloudQuote(v.Boot), servicePort(h, "volume_http")+20000+i, v.Job)
	}
	raw, err := run("ec-metrics-"+label, cmd)
	if err != nil {
		return nil, err
	}
	values, seen, holder := ecCounters{}, map[string]bool{}, ""
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "# EC_HOLDER ") {
			holder = strings.TrimPrefix(line, "# EC_HOLDER ")
			if !want[holder] || values[holder] != [2]float64{} || seen[holder] {
				return nil, errors.New("EC metrics foreign/duplicate holder")
			}
			seen[holder], values[holder] = true, [2]float64{}
		} else if strings.HasPrefix(line, ecMetric) {
			m := ecMetricSample.FindStringSubmatch(line)
			if len(m) != 3 || holder == "" || seen[holder+line[:strings.Index(line, "}")+1]] {
				return nil, errors.New("EC metrics malformed/duplicate sample")
			}
			seen[holder+line[:strings.Index(line, "}")+1]] = true
			v, e := strconv.ParseFloat(m[2], 64)
			if e != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, errors.New("EC metrics nonfinite/negative counter")
			}
			x, i := values[holder], 0
			if m[1] == "failure" {
				i = 1
			}
			x[i] = v
			values[holder] = x
		}
	}
	if len(values) != len(want) {
		return nil, errors.New("EC metrics missing holder")
	}
	return values, nil
}

func ecCounterDelta(before, after ecCounters) (float64, float64, error) {
	var s, f float64
	if len(before) != len(after) {
		return 0, 0, errors.New("EC counter cohort changed")
	}
	for k, b := range before {
		a, ok := after[k]
		if !ok || a[0] < b[0] || a[1] < b[1] {
			return 0, 0, errors.New("EC counter reset/missing holder")
		}
		s, f = s+a[0]-b[0], f+a[1]-b[1]
	}
	return s, f, nil
}
