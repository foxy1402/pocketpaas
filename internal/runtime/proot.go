package runtime

import (
	"os"
	"sync/atomic"
)

// prootEnabled tracks the last isolation tier before plain prefixing: proot is
// a ptrace-based tracer that rewrites path-resolving syscalls, so the app sees
// its rootfs as "/" and runs as a fake uid 0 with no privileges at all. It is
// the only tier that still gives a real rootfs view to hardened sandboxes that
// drop every capability, deny chroot(2) and deny clone(CLONE_NEWUSER).
//
// Shared across platforms so process.go can reference it unconditionally; only
// the Linux build can ever set it.
var prootEnabled atomic.Bool

// prootBin is installed by the pocketpaas image (Debian runtime stage).
const prootBin = "/usr/bin/proot"

// prootBinds lists host paths that must stay visible inside the proot guest.
// Without them most images break immediately: /dev/null and friends, /proc for
// runtime introspection, /sys for hardware info, and the resolver files for DNS.
func prootBinds() []string {
	candidates := []string{"/dev", "/proc", "/sys", "/etc/resolv.conf", "/etc/hosts", "/etc/localtime"}
	binds := make([]string, 0, len(candidates))
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			binds = append(binds, p)
		}
	}
	return binds
}
