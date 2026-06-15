//go:build !linux

package sysinfo

// Non-Linux stubs — the app runs in Linux containers in production.
func readCPUStats() (uint64, uint64, error) { return 0, 0, nil }
func readMemStats() (int64, int64, error)   { return 0, 0, nil }
