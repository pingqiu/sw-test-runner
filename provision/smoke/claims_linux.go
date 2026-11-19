//go:build linux

package smoke

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// procStart is field 22 (starttime) of /proc/<pid>/stat: with the pid it
// names one process, so a reused pid is not taken for the claim's holder.
func procStart(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // comm may contain spaces and parentheses
	if i < 0 {
		return "", errors.New("malformed /proc stat")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return "", errors.New("short /proc stat")
	}
	return f[19], nil // fields after comm start at 3 (state); starttime is 22
}
