package registry

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
)

// LayerCache is a digest-verified on-disk cache of compressed OCI layers.
//
// Why it exists: the pull path is fully streaming (network → tar → disk, no
// RAM buffering), so re-pulls are the expensive part — every config edit,
// failed deploy, or manual re-pull re-downloads every layer. Caching the
// COMPRESSED blobs (smaller than decompressed tarballs) makes those re-pulls
// skip the download entirely at layer granularity, which matters a lot on
// RAM- and bandwidth-constrained free-tier hosts.
//
// Safety properties (why this isn't cache.NewFilesystemCache, which stores
// truncated downloads without digest verification and never evicts):
//   - Blobs are written to a unique temp file and renamed into place ONLY
//     after the full stream passes digest verification — a file at the final
//     path is complete by construction, so cache hits need no re-verification.
//   - Interrupted downloads leave a temp file that Prune sweeps after an hour.
//   - Bounded size: Prune deletes oldest-mtime blobs past MaxBytes.
type LayerCache struct {
	Dir      string
	MaxBytes int64
}

var tmpCounter atomic.Int64

func (c *LayerCache) enabled() bool {
	return c != nil && c.Dir != ""
}

func (c *LayerCache) pathFor(dg v1.Hash) string {
	return filepath.Join(c.Dir, dg.Hex)
}

// Has reports whether a complete copy of the layer is cached.
func (c *LayerCache) Has(layer v1.Layer) bool {
	if !c.enabled() {
		return false
	}
	dg, err := layer.Digest()
	if err != nil {
		return false
	}
	_, err = os.Stat(c.pathFor(dg))
	return err == nil
}

// fetch returns a ReadCloser of the layer's COMPRESSED bytes (decompression
// is layered on top by the caller via decompressStream inside this method).
// A nil LayerCache or an unusable cache dir degrades to direct streaming.
func (c *LayerCache) fetch(ctx context.Context, layer v1.Layer) (io.ReadCloser, error) {
	if !c.enabled() {
		rc, err := layer.Compressed()
		if err != nil {
			return nil, err
		}
		return decompressStream(rc)
	}
	dg, err := layer.Digest()
	if err != nil {
		return nil, fmt.Errorf("layer digest: %w", err)
	}
	final := c.pathFor(dg)
	if f, err := os.Open(final); err == nil {
		return decompressStream(f)
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		log.Printf("layer cache unusable (%v) — streaming without cache", err)
		rc, err := layer.Compressed()
		if err != nil {
			return nil, err
		}
		return decompressStream(rc)
	}

	// Unique temp name per in-flight download: concurrent pulls (possibly of
	// the same layer by different apps) never interleave their writes.
	tmp := filepath.Join(c.Dir, fmt.Sprintf("tmp-%d-%d-%s", os.Getpid(), tmpCounter.Add(1), dg.Hex))
	f, err := os.Create(tmp)
	if err != nil {
		log.Printf("layer cache write failed (%v) — streaming without cache", err)
		rc, err := layer.Compressed()
		if err != nil {
			return nil, err
		}
		return decompressStream(rc)
	}

	rc, err := layer.Compressed()
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, err
	}
	ver := digest.Digest(dg.String()).Verifier()
	_, cerr := copyWithCtx(ctx, io.MultiWriter(f, ver), rc, math.MaxInt64)
	rc.Close()
	f.Close()
	if cerr != nil {
		os.Remove(tmp)
		return nil, cerr
	}
	if !ver.Verified() {
		os.Remove(tmp)
		return nil, fmt.Errorf("layer %s failed digest verification after download", dg.String())
	}
	if err := os.Rename(tmp, final); err != nil {
		// A concurrent pull of the same layer may have renamed its tmp into
		// place first. POSIX rename replaces it; on Windows the rename fails
		// while a reader holds the target open. Either way the blob that is
		// (or was) at `final` is digest-verified — complete by construction —
		// so use it instead of failing the whole extract.
		if _, statErr := os.Stat(final); statErr == nil {
			os.Remove(tmp)
			log.Printf("layer %s already cached by a concurrent pull — using it", dg.String())
		} else {
			os.Remove(tmp)
			return nil, fmt.Errorf("publish cached layer: %w", err)
		}
	}

	lf, err := os.Open(final)
	if err != nil {
		return nil, err
	}
	return decompressStream(lf)
}

// Prune deletes stale temp files and the oldest cached blobs until the cache
// fits in MaxBytes. Concurrent readers keep their open file handles on Linux;
// on Windows an in-use blob fails to delete and is simply skipped.
func (c *LayerCache) Prune() {
	if !c.enabled() || c.MaxBytes <= 0 {
		return
	}
	entries, err := os.ReadDir(c.Dir)
	if err != nil {
		return // cache dir absent — nothing to prune
	}
	var total int64
	type blob struct {
		name string
		size int64
		mod  time.Time
	}
	var blobs []blob
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "tmp-") {
			// In-flight downloads rename away within seconds; leftovers
			// older than an hour are dead weight from a crashed pull.
			if time.Since(info.ModTime()) > time.Hour {
				_ = os.Remove(filepath.Join(c.Dir, e.Name()))
			}
			continue
		}
		blobs = append(blobs, blob{e.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= c.MaxBytes {
		return
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].mod.Before(blobs[j].mod) })
	var removed int64
	for _, b := range blobs {
		if total <= c.MaxBytes {
			break
		}
		if err := os.Remove(filepath.Join(c.Dir, b.name)); err == nil {
			total -= b.size
			removed += b.size
		}
	}
	if removed > 0 {
		log.Printf("layer cache: pruned %d MiB (over the %d MiB cap)", removed>>20, c.MaxBytes>>20)
	}
}

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// decompressStream wraps rc in the decompressor indicated by its magic bytes
// (gzip / zstd / plain tar), mirroring go-containerregistry's own detection.
// Returns the decompressed stream; rc is closed when the result is closed.
func decompressStream(rc io.ReadCloser) (io.ReadCloser, error) {
	br := bufio.NewReader(rc)
	magic, err := br.Peek(len(zstdMagic))
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		rc.Close()
		return nil, fmt.Errorf("detect compression: %w", err)
	}
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("gzip layer: %w", err)
		}
		return &multiCloser{Reader: gz, closers: []func() error{rc.Close, gz.Close}}, nil
	case bytes.Equal(magic, zstdMagic):
		zr, err := zstd.NewReader(br)
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("zstd layer: %w", err)
		}
		return &multiCloser{Reader: zr, closers: []func() error{rc.Close, func() error { zr.Close(); return nil }}}, nil
	default:
		return &multiCloser{Reader: br, closers: []func() error{rc.Close}}, nil
	}
}

// multiCloser is an io.ReadCloser that closes several underlying resources.
type multiCloser struct {
	io.Reader
	closers []func() error
}

func (m *multiCloser) Close() error {
	var firstErr error
	for _, cl := range m.closers {
		if err := cl(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
