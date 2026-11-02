package seaweed

import "golang.org/x/sys/unix"

// controllerMono reads M01 CLOCK_MONOTONIC, the assessor's one clock domain.
func controllerMono() (float64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9, nil
}
