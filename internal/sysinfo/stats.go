// Package sysinfo collects lightweight host resource stats (CPU %, RAM MB).
// Reads are always platform-specific; see stats_linux.go / stats_other.go.
package sysinfo

import "sync"

// Stats holds a sampled resource snapshot.
type Stats struct {
	CPUPercent float64
	RAMUsedMB  int64
	RAMTotalMB int64
}

// Sampler accumulates successive CPU readings to compute delta-based CPU%.
// Call Update() on a regular interval (e.g. every 5 s) from a goroutine.
type Sampler struct {
	mu        sync.Mutex
	prevTotal uint64
	prevIdle  uint64
	current   Stats
}

// Update reads the OS stats and refreshes the cached Stats value.
// On a transient read failure the previous sample is kept — overwriting prev
// values with zeros would freeze the CPU metric at its last known value.
func (s *Sampler) Update() {
	total, idle, cpuErr := readCPUStats()
	totalMB, usedMB, _ := readMemStats()

	s.mu.Lock()
	defer s.mu.Unlock()

	if cpuErr == nil {
		if s.prevTotal > 0 && total > s.prevTotal {
			deltaTotal := total - s.prevTotal
			deltaIdle := idle - s.prevIdle
			if deltaTotal > 0 {
				s.current.CPUPercent = float64(deltaTotal-deltaIdle) / float64(deltaTotal) * 100
				if s.current.CPUPercent < 0 {
					s.current.CPUPercent = 0
				}
				if s.current.CPUPercent > 100 {
					s.current.CPUPercent = 100
				}
			}
		}
		s.prevTotal = total
		s.prevIdle = idle
	}

	if totalMB > 0 {
		s.current.RAMTotalMB = totalMB
		s.current.RAMUsedMB = usedMB
	}
}

// Get returns the last sampled Stats (never blocks).
func (s *Sampler) Get() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}
