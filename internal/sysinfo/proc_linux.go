//go:build linux

package sysinfo

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

var clkTck = func() int64 {
	v, err := unix.Sysconf(unix.SC_CLK_TCK)
	if err != nil || v <= 0 {
		return 100
	}
	return v
}()

// ClkTck returns the number of CPU scheduler ticks per second (typically 100).
func ClkTck() int64 { return clkTck }

// ReadProcStat returns total CPU ticks (utime+stime) and RSS in KB for pid.
// Both values are zero if the process is gone or /proc is unavailable.
func ReadProcStat(pid int) (ticks uint64, rssKB int64, err error) {
	// ── CPU ticks from /proc/{pid}/stat ──────────────────────────────────────
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	// The second field "(comm)" may contain spaces and ')'; find the last ')'.
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return 0, 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	// Fields after ')' (0-based):  state(0) ppid(1) … utime(11) stime(12)
	tail := strings.Fields(string(data[end+1:]))
	if len(tail) < 13 {
		return 0, 0, fmt.Errorf("too few fields in /proc/%d/stat", pid)
	}
	utime, _ := strconv.ParseUint(tail[11], 10, 64)
	stime, _ := strconv.ParseUint(tail[12], 10, 64)
	ticks = utime + stime

	// ── RSS from /proc/{pid}/status ───────────────────────────────────────────
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return ticks, 0, nil // CPU ok, RAM unavailable
	}
	for _, line := range strings.SplitAfter(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				rssKB, _ = strconv.ParseInt(parts[1], 10, 64)
			}
			break
		}
	}
	return ticks, rssKB, nil
}
