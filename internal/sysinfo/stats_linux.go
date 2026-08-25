//go:build linux

package sysinfo

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func readCPUStats() (total, idle uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, fmt.Errorf("empty /proc/stat")
	}
	line := sc.Text()

	var tag string
	var user, nice, sys, idleT, iowait, irq, softirq, steal uint64
	if _, err = fmt.Sscanf(line, "%s %d %d %d %d %d %d %d %d",
		&tag, &user, &nice, &sys, &idleT, &iowait, &irq, &softirq, &steal); err != nil {
		return 0, 0, err
	}
	total = user + nice + sys + idleT + iowait + irq + softirq + steal
	idle = idleT + iowait
	return total, idle, nil
}

// readMemStats reports memory usage from cgroup limits when running inside a
// container (so the dashboard shows the CONTAINER's allocation, not the
// host's), falling back to /proc/meminfo otherwise.
func readMemStats() (totalMB, usedMB int64, err error) {
	if total, used, ok := readCgroupMem(); ok {
		return total, used, nil
	}
	return readProcMeminfo()
}

// readCgroupMem reads cgroup v2 (memory.max/memory.current) or v1
// (memory.limit_in_bytes/memory.usage_in_bytes) limits.
// Values near the addressable-space ceiling mean "no limit" and are ignored.
func readCgroupMem() (totalMB, usedMB int64, ok bool) {
	// cgroup v2
	if total, used, ok2 := readInt64Pair(
		"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.current"); ok2 {
		return total, used, true
	}
	// cgroup v1
	if total, used, ok2 := readInt64Pair(
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
		"/sys/fs/cgroup/memory/memory.usage_in_bytes"); ok2 {
		return total, used, true
	}
	return 0, 0, false
}

func readInt64Pair(totalPath, usedPath string) (totalMB, usedMB int64, ok bool) {
	total, err := readCgroupFileInt(totalPath)
	if err != nil || total <= 0 || total >= 1<<50 { // ~10^15 B = effectively unlimited
		return 0, 0, false
	}
	used, err := readCgroupFileInt(usedPath)
	if err != nil || used < 0 {
		return 0, 0, false
	}
	if used > total {
		used = total
	}
	return total / (1024 * 1024), used / (1024 * 1024), true
}

func readCgroupFileInt(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return 0, fmt.Errorf("unlimited")
	}
	return strconv.ParseInt(s, 10, 64)
}

func readProcMeminfo() (totalMB, usedMB int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	var memTotal, memFree, memAvailable, buffers, cached int64
	var hasAvailable bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		var key string
		var val int64
		if _, e := fmt.Sscanf(line, "%s %d", &key, &val); e != nil {
			continue
		}
		key = strings.TrimSuffix(key, ":")
		switch key {
		case "MemTotal":
			memTotal = val
		case "MemFree":
			memFree = val
		case "MemAvailable":
			memAvailable = val
			hasAvailable = true
		case "Buffers":
			buffers = val
		case "Cached":
			cached = val
		}
	}
	if memTotal == 0 {
		return 0, 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
	}
	totalMB = memTotal / 1024
	if hasAvailable {
		usedMB = (memTotal - memAvailable) / 1024
	} else {
		// Pre-3.14 kernels lack MemAvailable; approximate with MemFree + Buffers + Cached.
		usedMB = (memTotal - memFree - buffers - cached) / 1024
	}
	if usedMB < 0 {
		usedMB = 0
	}
	return totalMB, usedMB, nil
}
