//go:build linux

package sysinfo

import (
	"bufio"
	"fmt"
	"os"
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

func readMemStats() (totalMB, usedMB int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	var memTotal, memAvailable int64
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
		case "MemAvailable":
			memAvailable = val
		}
		if memTotal > 0 && memAvailable > 0 {
			break
		}
	}
	if memTotal == 0 {
		return 0, 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
	}
	totalMB = memTotal / 1024
	usedMB = (memTotal - memAvailable) / 1024
	return totalMB, usedMB, nil
}
