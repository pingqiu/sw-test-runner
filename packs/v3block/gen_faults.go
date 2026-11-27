package v3block

// The block side of the M2 seeded fault schedule. The schedule itself (a
// pure function of seed, ops, processes, rate and restart bound, and its
// checker) is generic: randomrun.NewFaultSchedule and CheckFaultSchedule.
// The block names the processes a schedule may kill: the master and the
// volume servers (so the master and a volume server are never down
// together: one process down at a time).

import (
	"strconv"
	"strings"

	"github.com/pingqiu/sw-test-runner/randomrun"
)

// DefaultRestartWithin is the default bound, in operations, between a kill
// and the restart of the same process.
const DefaultRestartWithin = randomrun.DefaultRestartWithin

// Fault event actions.
const (
	FaultKill    = randomrun.FaultKill
	FaultRestart = randomrun.FaultRestart
)

// FaultEvent is one scheduled process fault; FaultSchedule a run's schedule.
type (
	FaultEvent    = randomrun.FaultEvent
	FaultSchedule = randomrun.FaultSchedule
)

// ValidFaultProcess says whether p names a process the fault actions know:
// "blockmaster" or "blockvolume:<i>".
func ValidFaultProcess(p string) bool {
	if p == "blockmaster" {
		return true
	}
	n, ok := strings.CutPrefix(p, "blockvolume:")
	if !ok || n == "" {
		return false
	}
	i, err := strconv.Atoi(n)
	return err == nil && i >= 0 && strconv.Itoa(i) == n
}

// NewFaultSchedule derives the schedule over block processes.
func NewFaultSchedule(seed uint64, ops int, processes []string, rate float64, within int) (FaultSchedule, error) {
	return randomrun.NewFaultSchedule(seed, ops, processes, rate, within, ValidFaultProcess)
}

// CheckFaultSchedule judges a schedule over block processes.
func CheckFaultSchedule(events []FaultEvent, ops, within int) error {
	return randomrun.CheckFaultSchedule(events, ops, within, ValidFaultProcess)
}
