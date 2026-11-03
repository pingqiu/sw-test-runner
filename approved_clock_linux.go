package testrunner

import "golang.org/x/sys/unix"

// authorizeMono reads the controller's CLOCK_MONOTONIC, the soak assessor's M01 clock domain.
func authorizeMono() (float64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9, nil
}
