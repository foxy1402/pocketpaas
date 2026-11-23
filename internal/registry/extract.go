package registry

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Extraction safety limits. Layers are decompressed while streaming, so a
// small gzip/zstd archive can expand to hundreds of GB ("tar bomb") — cap
// both the per-file and cumulative extracted size, and require a minimum
// amount of free disk space before starting.
const (
	maxExtractFileBytes  = 4 << 30   // 4 GiB for a single file
	maxExtractTotalBytes = 16 << 30  // 16 GiB for the flattened image
	minFreeDiskBytes     = 512 << 20 // refuse to extract with less free space
)

// Extract writes the image's flattened filesystem to destDir.
// It sends progress messages to the progress channel (non-blocking) and
// aborts early when ctx is cancelled (e.g. the pull timeout fires).
// Layers are streamed through the (optional, digest-verified) disk cache —
// cache hits skip the download entirely; cache misses download into the
// cache first, then extract from the cached file.
func Extract(ctx context.Context, img v1.Image, destDir string, cache *LayerCache, progress chan<- string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}

	if free, err := diskFreeBytes(destDir); err == nil && free < minFreeDiskBytes {
		return fmt.Errorf("insufficient disk space: %d MiB free, need at least %d MiB",
			free>>20, minFreeDiskBytes>>20)
	}

	layers, err := img.Layers()
	if err != nil {
		return err
	}

	cleanDest := filepath.Clean(destDir) + string(filepath.Separator)

	// total tracks cumulative extracted bytes across all layers.
	var total int64
	for i, layer := range layers {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("extract cancelled: %w", err)
		}
		if cache.Has(layer) {
			sendProgress(progress, fmt.Sprintf("Layer %d/%d: cached — skipping download", i+1, len(layers)))
		} else {
			sendProgress(progress, fmt.Sprintf("Layer %d/%d: downloading…", i+1, len(layers)))
		}
		rc, err := cache.fetch(ctx, layer)
		if err != nil {
			return fmt.Errorf("fetch layer %d: %w", i+1, err)
		}
		sendProgress(progress, fmt.Sprintf("Extracting layer %d/%d …", i+1, len(layers)))
		if err := extractTar(ctx, rc, destDir, cleanDest, &total); err != nil {
			rc.Close()
			return fmt.Errorf("extract layer %d: %w", i+1, err)
		}
		rc.Close()
	}

	sendProgress(progress, "Extraction complete.")
	return nil
}

func extractTar(ctx context.Context, r io.Reader, destDir, cleanDest string, total *int64) error {
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Strip leading slashes and clean to prevent path traversal.
		// filepath.Join(destDir, "/bin/sh") returns /bin/sh on Linux — we must sanitize first.
		name := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "/")
		rel := filepath.Clean(name)
		if rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}

		// Opaque whiteout: delete the entire directory.
		if filepath.Base(rel) == ".wh..wh..opq" {
			target := filepath.Join(destDir, filepath.Dir(rel))
			removeContained(target, cleanDest)
			continue
		}

		// Standard whiteout: delete a specific file/dir.
		if strings.HasPrefix(filepath.Base(rel), ".wh.") {
			target := filepath.Join(destDir, filepath.Dir(rel),
				strings.TrimPrefix(filepath.Base(rel), ".wh."))
			removeContained(target, cleanDest)
			continue
		}

		dest := filepath.Join(destDir, rel)

		// Ensure the resolved path is under destDir (guard against any remaining traversal).
		if !strings.HasPrefix(filepath.Clean(dest)+string(filepath.Separator), cleanDest) &&
			filepath.Clean(dest) != filepath.Clean(destDir) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			// A symlink planted at dest by an earlier entry must not be followed.
			removeEscapingSymlink(dest, cleanDest)
			if !containedWithin(dest, cleanDest) {
				log.Printf("extract: skipping directory escaping rootfs: %s", rel)
				continue
			}
			if err := os.MkdirAll(dest, os.FileMode(hdr.Mode)|0111); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			parent := filepath.Dir(dest)
			if !containedWithin(parent, cleanDest) {
				log.Printf("extract: skipping file escaping rootfs: %s", rel)
				continue
			}
			if hdr.Size > maxExtractFileBytes {
				return fmt.Errorf("file %s exceeds the %d GiB per-file limit (%d bytes)",
					rel, maxExtractFileBytes>>30, hdr.Size)
			}
			if err := os.MkdirAll(parent, 0755); err != nil {
				return err
			}
			// If dest is an existing symlink (planted by an earlier layer),
			// OpenFile would follow it out of the rootfs — remove it first.
			if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				_ = os.Remove(dest)
			}
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)|0200)
			if err != nil {
				return err
			}
			budget := int64(maxExtractFileBytes)
			if rem := int64(maxExtractTotalBytes) - *total; rem < budget {
				budget = rem
			}
			written, err := copyWithCtx(ctx, f, tr, budget)
			*total += written
			if err != nil {
				f.Close()
				return err
			}
			f.Close()
			// os.OpenFile ignores the perm argument when the file already
			// exists (a later layer overwriting an earlier one). Explicitly
			// set the mode so the execute bit from the tar header is preserved.
			if err := os.Chmod(dest, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// The link target is stored verbatim: absolute targets are
			// legitimate inside container images (e.g. /bin/sh → /usr/bin/sh)
			// and are only dangerous if followed on the host side. Every
			// create/write/remove/chmod above and every runtime write
			// (ensureChrootDNS / prune) re-verifies containment through
			// EvalSymlinks, so planted symlinks can never be traversed.
			parent := filepath.Dir(dest)
			if !containedWithin(parent, cleanDest) {
				log.Printf("extract: skipping symlink escaping rootfs: %s", rel)
				continue
			}
			if err := os.MkdirAll(parent, 0755); err != nil {
				return err
			}
			if fi, err := os.Lstat(dest); err == nil && !fi.IsDir() {
				os.Remove(dest)
			}
			if err := os.Symlink(hdr.Linkname, dest); err != nil {
				// Non-fatal: broken symlinks are common in images; log and continue.
				log.Printf("extract: symlink %s: %v", rel, err)
			}
		case tar.TypeLink:
			if err := ctx.Err(); err != nil {
				return err
			}
			// Hard links must be within destDir, verified through symlinks:
			// both the source and the destination's parent directory.
			link := filepath.Join(destDir, filepath.Clean(strings.TrimPrefix(
				filepath.ToSlash(hdr.Linkname), "/")))
			if !containedWithin(link, cleanDest) || !containedWithin(filepath.Dir(dest), cleanDest) {
				log.Printf("extract: skipping hard link escaping rootfs: %s -> %s", rel, hdr.Linkname)
				continue
			}
			if fi, err := os.Lstat(dest); err == nil && !fi.IsDir() {
				os.Remove(dest)
			}
			if err := os.Link(link, dest); err != nil {
				log.Printf("extract: hard link %s: %v", rel, err)
			}
		}
	}
	return nil
}

// containedWithin reports whether path is guaranteed to resolve inside the
// extraction root. It walks up to the nearest existing ancestor, resolves
// symlinks with EvalSymlinks, and verifies the result stays under the root —
// catching escapes through symlinked intermediate directories that the purely
// lexical prefix check cannot see.
func containedWithin(path, cleanDest string) bool {
	root := strings.TrimSuffix(cleanDest, string(filepath.Separator))
	cur := filepath.Clean(path)
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return false
			}
			r := filepath.Clean(resolved)
			return r == root || strings.HasPrefix(r+string(filepath.Separator), cleanDest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

// removeContained removes target only if it (through any symlinks) resolves
// inside the extraction root. Whiteout entries are attacker-controlled, so
// removal must be containment-checked like every other filesystem operation.
func removeContained(target, cleanDest string) {
	if !containedWithin(filepath.Dir(target), cleanDest) {
		log.Printf("extract: refusing whiteout removal outside rootfs: %s", target)
		return
	}
	// RemoveAll does not follow the final symlink component, so after the
	// parent check this cannot remove host files.
	os.RemoveAll(target)
}

// removeEscapingSymlink removes dest if it is a symlink whose target escapes
// the extraction root, so that a following MkdirAll doesn't operate through it.
func removeEscapingSymlink(dest, cleanDest string) {
	fi, err := os.Lstat(dest)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return
	}
	if !containedWithin(dest, cleanDest) {
		os.Remove(dest)
	}
}

// copyWithCtx copies src to dst in chunks, aborting when ctx is cancelled or
// when more than maxBytes have been written (tar-bomb protection).
// It returns the number of bytes written.
func copyWithCtx(ctx context.Context, dst io.Writer, src io.Reader, maxBytes int64) (int64, error) {
	buf := make([]byte, 64*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if written+int64(n) > maxBytes {
				n = int(maxBytes - written)
				if n < 0 {
					n = 0
				}
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return written, werr
				}
				written += int64(n)
				return written, fmt.Errorf("extracted data exceeds the size limit (%d bytes)", maxBytes)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return written, werr
			}
			written += int64(n)
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, rerr
		}
	}
}
