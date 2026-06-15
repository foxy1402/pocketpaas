package registry

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Extract writes the image's flattened filesystem to destDir.
// It sends progress messages to the progress channel (non-blocking).
func Extract(img v1.Image, destDir string, progress chan<- string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return err
	}

	cleanDest := filepath.Clean(destDir) + string(filepath.Separator)

	for i, layer := range layers {
		sendProgress(progress, fmt.Sprintf("Downloading layer %d/%d …", i+1, len(layers)))
		rc, err := layer.Uncompressed()
		if err != nil {
			return fmt.Errorf("uncompress layer %d: %w", i+1, err)
		}
		sendProgress(progress, fmt.Sprintf("Extracting layer %d/%d …", i+1, len(layers)))
		if err := extractTar(rc, destDir, cleanDest); err != nil {
			rc.Close()
			return fmt.Errorf("extract layer %d: %w", i+1, err)
		}
		rc.Close()
	}

	sendProgress(progress, "Extraction complete.")
	return nil
}

func extractTar(r io.Reader, destDir, cleanDest string) error {
	tr := tar.NewReader(r)
	for {
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
			os.RemoveAll(filepath.Join(destDir, filepath.Dir(rel)))
			continue
		}

		// Standard whiteout: delete a specific file/dir.
		if strings.HasPrefix(filepath.Base(rel), ".wh.") {
			target := filepath.Join(destDir, filepath.Dir(rel),
				strings.TrimPrefix(filepath.Base(rel), ".wh."))
			os.RemoveAll(target)
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
			if err := os.MkdirAll(dest, os.FileMode(hdr.Mode)|0111); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)|0200)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			os.Remove(dest)
			// Symlinks point inside the container; don't resolve against the host.
			if err := os.Symlink(hdr.Linkname, dest); err != nil {
				// Non-fatal: broken symlinks are common in images; log and continue.
				_ = err
			}
		case tar.TypeLink:
			// Hard links must be within destDir.
			link := filepath.Join(destDir, filepath.Clean(strings.TrimPrefix(
				filepath.ToSlash(hdr.Linkname), "/")))
			if strings.HasPrefix(filepath.Clean(link)+string(filepath.Separator), cleanDest) {
				os.Remove(dest)
				if err := os.Link(link, dest); err != nil {
					_ = err
				}
			}
		}
	}
	return nil
}
