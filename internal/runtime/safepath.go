package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// joinUnderRoot joins rel onto root and verifies the lexical result stays
// inside root. rel is interpreted as rootfs-relative whether it is
// container-absolute ("/etc/x") or relative ("etc/x").
func joinUnderRoot(root, rel string) (string, error) {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "/")
	joined := filepath.Join(root, filepath.FromSlash(rel))
	cleanRoot := filepath.Clean(root)
	if joined != cleanRoot &&
		!strings.HasPrefix(joined+string(filepath.Separator), cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the image rootfs", rel)
	}
	return joined, nil
}

// containedWithin reports whether path is guaranteed to resolve inside root.
// It walks up to the nearest existing ancestor, resolves symlinks with
// EvalSymlinks, and verifies the result stays under root — catching escapes
// through symlinked intermediate directories that a purely lexical prefix
// check cannot see. (Kept in sync with the same helper in internal/registry.)
func containedWithin(root, path string) bool {
	cleanRoot := filepath.Clean(root)
	prefix := cleanRoot + string(filepath.Separator)
	cur := filepath.Clean(path)
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return false
			}
			r := filepath.Clean(resolved)
			return r == cleanRoot || strings.HasPrefix(r, prefix)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

// writeRootfsFileNoFollow writes data to root/rel without following a symlink
// planted at the destination or in its parent directories — a malicious image
// could otherwise redirect the write (e.g. etc/resolv.conf → host /etc/resolv.conf).
func writeRootfsFileNoFollow(root, rel string, data []byte, perm os.FileMode) {
	p, err := joinUnderRoot(root, rel)
	if err != nil {
		return
	}
	if !containedWithin(root, filepath.Dir(p)) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return
	}
	// Remove an existing symlink at the destination so WriteFile cannot
	// follow it out of the rootfs.
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(p)
	}
	_ = os.WriteFile(p, data, perm)
}
