package registry

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Credentials holds optional registry authentication.
type Credentials struct {
	Username string
	Password string
}

// Pull fetches an OCI image by reference. Sends progress messages to the
// progress channel (non-blocking — messages are dropped if the channel is full).
// The caller owns the channel and must close it.
// ctx is used to cancel/timeout the remote HTTP calls.
func Pull(ctx context.Context, imageRef string, creds *Credentials, progress chan<- string) (v1.Image, error) {
	sendProgress(progress, fmt.Sprintf("Resolving %s …", imageRef))

	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parse image ref %q: %w", imageRef, err)
	}

	opts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	}
	if creds != nil && creds.Username != "" {
		auth := authn.FromConfig(authn.AuthConfig{
			Username: creds.Username,
			Password: creds.Password,
		})
		opts = []remote.Option{
			remote.WithContext(ctx),
			remote.WithAuth(auth),
		}
	}

	sendProgress(progress, "Fetching image manifest …")
	img, err := remote.Image(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("fetch image: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("list layers: %w", err)
	}
	sendProgress(progress, fmt.Sprintf("Image has %d layer(s). Starting download …", len(layers)))

	return img, nil
}

// ImageConfig extracts the default entrypoint, cmd, working directory, and first
// exposed TCP port from the image config. port is 0 when none is declared.
func ImageConfig(img v1.Image) (entrypoint []string, cmd []string, workDir string, port int, err error) {
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, nil, "", 0, fmt.Errorf("read image config: %w", err)
	}
	return cfg.Config.Entrypoint, cfg.Config.Cmd, cfg.Config.WorkingDir, pickPort(cfg.Config.ExposedPorts), nil
}

// pickPort selects the most likely web-service port from the image's ExposedPorts
// map (keys are "port/proto", e.g. "8080/tcp"). Returns 0 when none is declared.
func pickPort(ports map[string]struct{}) int {
	preferred := []int{8080, 3000, 80, 5000, 4000, 8000, 8888, 9000}
	seen := make(map[int]bool, len(ports))
	for k := range ports {
		p, _, _ := strings.Cut(k, "/")
		if n, err := strconv.Atoi(p); err == nil {
			seen[n] = true
		}
	}
	for _, p := range preferred {
		if seen[p] {
			return p
		}
	}
	// fallback: lowest numeric port found (sorted for determinism).
	allPorts := make([]int, 0, len(seen))
	for p := range seen {
		allPorts = append(allPorts, p)
	}
	sort.Ints(allPorts)
	if len(allPorts) > 0 {
		return allPorts[0]
	}
	return 0
}

// sendProgress sends msg to ch without blocking.
// If the channel buffer is full (e.g. SSE client disconnected), the message is dropped.
func sendProgress(ch chan<- string, msg string) {
	select {
	case ch <- msg:
	default:
	}
}
