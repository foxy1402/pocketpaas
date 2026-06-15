package registry

import (
	"fmt"

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
// progress channel. The caller owns the channel and must close it.
func Pull(imageRef string, creds *Credentials, progress chan<- string) (v1.Image, error) {
	progress <- fmt.Sprintf("Resolving %s …", imageRef)

	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parse image ref %q: %w", imageRef, err)
	}

	opts := []remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)}
	if creds != nil && creds.Username != "" {
		auth := authn.FromConfig(authn.AuthConfig{
			Username: creds.Username,
			Password: creds.Password,
		})
		opts = []remote.Option{remote.WithAuth(auth)}
	}

	progress <- "Fetching image manifest …"
	img, err := remote.Image(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("fetch image: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("list layers: %w", err)
	}
	progress <- fmt.Sprintf("Image has %d layer(s). Downloading …", len(layers))

	return img, nil
}

// ImageConfig extracts the default entrypoint, cmd, and working directory
// from the image config.
func ImageConfig(img v1.Image) (entrypoint []string, cmd []string, workDir string, err error) {
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, nil, "", fmt.Errorf("read image config: %w", err)
	}
	return cfg.Config.Entrypoint, cfg.Config.Cmd, cfg.Config.WorkingDir, nil
}
