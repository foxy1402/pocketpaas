//go:build windows

package registry

// diskFreeBytes reports effectively-unlimited space on Windows, which
// disables the pre-extraction free-space check (dev-only platform; the size
// caps in extract.go still apply).
func diskFreeBytes(_ string) (uint64, error) {
	return 1 << 62, nil
}
