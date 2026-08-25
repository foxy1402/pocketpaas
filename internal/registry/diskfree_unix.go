//go:build !windows

package registry

import "syscall"

// diskFreeBytes returns the number of bytes available to unprivileged users
// on the filesystem containing path.
func diskFreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
