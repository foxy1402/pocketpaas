//go:build !linux

package sysinfo

// ClkTck returns the nominal tick rate (non-Linux stub).
func ClkTck() int64 { return 100 }

// ReadProcStat is a no-op on non-Linux platforms.
func ReadProcStat(_ int) (ticks uint64, rssKB int64, err error) {
	return 0, 0, nil
}
