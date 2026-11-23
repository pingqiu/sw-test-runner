package hostloss

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	grantRE = regexp.MustCompile(`(?m)^forced move issued: volume (\S+) request (\S+): (\S+)@(\d+) on (\S+) -> (\S+)@(\d+) on (\S+) \(kept HEAD gen (\d+), remote LSN (\d+)\)$`)
	rangeRE = regexp.MustCompile(`(?m)^range (\d+) of (\d+): request (\S+), (\S+)@(\d+) -> epoch (\d+), lost after LSN (\d+), until (\S+) \(master clock\)$`)
)

// LostRange is the judged force-move grant: the kept LSN boundary L and the epochs.
type LostRange struct {
	L, OldEpoch, NewEpoch, Gen int64
	Until                      string // operator information only, never a criterion
}

// JudgeLostRange is d6-lost-range: the force-move grant output and the lost-range readout name ONE range of this
// request, with the SAME stored LostAfterLSN as the grant's kept remote LSN (never recomputed), and a new epoch above
// the old. request = "" accepts the grant's own request id.
func JudgeLostRange(grant, ranges, request string) (Row, *LostRange) {
	g := grantRE.FindAllStringSubmatch(grant, -1)
	rs := rangeRE.FindAllStringSubmatch(ranges, -1)
	if len(g) != 1 || len(rs) == 0 {
		return Row{"d6-lost-range", Fail, fmt.Sprintf("unparsable: %d grants, %d ranges", len(g), len(rs))}, nil
	}
	req, oe, ne, gen, kept := g[0][2], atoi(g[0][4]), atoi(g[0][7]), atoi(g[0][9]), atoi(g[0][10])
	var bad []string
	if request != "" && req != request {
		bad = append(bad, fmt.Sprintf("grant request %s != %s", req, request))
	}
	var mine [][]string
	for _, r := range rs {
		if r[3] == req {
			mine = append(mine, r)
		}
	}
	until := ""
	if len(mine) != 1 {
		bad = append(bad, fmt.Sprintf("%d ranges for request %s", len(mine), req))
	} else {
		r := mine[0]
		until = r[8]
		if atoi(r[7]) != kept {
			bad = append(bad, fmt.Sprintf("stored LostAfterLSN %s != grant kept remote LSN %d", r[7], kept))
		}
		if atoi(r[5]) != oe || atoi(r[6]) != ne {
			bad = append(bad, fmt.Sprintf("range epochs %s->%s != grant %d->%d", r[5], r[6], oe, ne))
		}
	}
	if ne <= oe {
		bad = append(bad, fmt.Sprintf("new epoch %d not above old %d", ne, oe))
	}
	if len(bad) > 0 {
		return Row{"d6-lost-range", Fail, strings.Join(bad, "; ")}, nil
	}
	lr := &LostRange{L: kept, OldEpoch: oe, NewEpoch: ne, Gen: gen, Until: until}
	return Row{"d6-lost-range", Pass, fmt.Sprintf("L=%d E=%d E2=%d gen=%d until=%s (operator info)", kept, oe, ne, gen, until)}, lr
}

func atoi(s string) int64 { v, _ := strconv.ParseInt(s, 10, 64); return v }

var (
	fenceRE   = regexp.MustCompile(`(?m)workspace lease: fenced volume=\S+ replica=\S+ epoch=\d+: (.*)$`)
	validRE   = regexp.MustCompile(`(?m)workspace lease: renewal failed after .*lease valid until (\S+)\): `)
	expiredRE = regexp.MustCompile(`lease expired at (\S+) without`)
)

const fencedStop = "blockvolume: stopping: the workspace lease guard fenced"

// rfc3339ns parses the product's UTC lease times (RFC 3339, Z, up to ns).
func rfc3339ns(s string) (int64, error) {
	s = strings.TrimRight(strings.TrimSpace(s), ".,")
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || !strings.HasSuffix(s, "Z") {
		return 0, fmt.Errorf("time %q", s)
	}
	return t.UnixNano(), nil
}

// JudgeStopBound is d9-stop-bound (partition reps): the old owner stopped serving by its lease bound. The bound U is
// the one the lease guard itself logged ("lease expired at U", else the last "lease valid until U"). PASS iff a fence
// line with an expiry cause exists (a "revoked by the master" cause means the partition was not effective), the
// blockvolume logged its fenced stop, and NO acknowledged write has a wall time after U (same host clock).
func JudgeStopBound(l *Ledger, journal string) Row {
	f := fenceRE.FindAllStringSubmatch(journal, -1)
	if len(f) == 0 {
		return Row{"d9-stop-bound", Fail, "no fence line: the old owner never fenced"}
	}
	cause := f[0][1]
	var bad []string
	if strings.Contains(cause, "revoked by the master") {
		bad = append(bad, `fence cause "revoked by the master": the partition was not effective`)
	}
	var U int64
	var err error
	if m := expiredRE.FindStringSubmatch(cause); m != nil {
		U, err = rfc3339ns(m[1])
	} else if v := validRE.FindAllStringSubmatch(journal, -1); len(v) > 0 {
		U, err = rfc3339ns(v[len(v)-1][1])
	} else {
		return Row{"d9-stop-bound", Fail, fmt.Sprintf("no lease bound logged (cause %q)", trunc(cause, 80))}
	}
	if err != nil {
		return Row{"d9-stop-bound", Fail, "unparsable bound: " + err.Error()}
	}
	if !strings.Contains(journal, fencedStop) {
		bad = append(bad, `no "stopping: the workspace lease guard fenced" line (blockvolume did not stop)`)
	}
	var late int
	var firstSeq, firstWall, lastAck int64
	for _, r := range l.BySeq {
		if r.Kind != 'A' {
			continue
		}
		if r.Wall > lastAck {
			lastAck = r.Wall
		}
		if r.Wall > U {
			if late == 0 || r.Seq < firstSeq {
				firstSeq = r.Seq
			}
			if late == 0 || r.Wall < firstWall {
				firstWall = r.Wall
			}
			late++
		}
	}
	if late > 0 {
		bad = append(bad, fmt.Sprintf("%d acknowledged write(s) after the lease bound (first seq %d, +%d ms)", late, firstSeq, (firstWall-U)/1_000_000))
	}
	if len(bad) > 0 {
		return Row{"d9-stop-bound", Fail, strings.Join(bad, "; ")}
	}
	return Row{"d9-stop-bound", Pass, fmt.Sprintf("bound=%d last_ack=%d margin_ms=%d cause=%q", U, lastAck, (U-lastAck)/1_000_000, trunc(cause, 60))}
}
