// Package artifact downloads runtime archives and keeps digest-verified copies.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

var (
	// ErrDigestMismatch is returned when downloaded bytes do not match the pinned SHA-256.
	ErrDigestMismatch = errors.New("sha256 mismatch")
	// ErrSizeMismatch is returned when the download is larger or smaller than declared.
	ErrSizeMismatch = errors.New("size mismatch")
	// ErrUseURL is returned by Fetch to download from the artifact's URL
	// instead (in a cluster: on the leader).
	ErrUseURL = errors.New("download from the artifact url")
)

// Cache stores artifacts under Dir, named by their SHA-256.
type Cache struct {
	Dir    string
	Client *http.Client
	// Fetch, when set, supplies the bytes instead of the artifact's URL (in a
	// cluster: the leader). They are verified against the pinned size and
	// digest all the same.
	Fetch func(ctx context.Context, a domain.Artifact) (io.ReadCloser, error)
}

var _ domain.ArtifactCache = Cache{}

// Get returns the path of a verified local copy of a, downloading it if missing or corrupt.
func (c Cache) Get(ctx context.Context, a domain.Artifact) (string, error) {
	if !strings.HasPrefix(a.URL, "https://") {
		return "", fmt.Errorf("refusing non-https artifact url %q", a.URL)
	}
	path := filepath.Join(c.Dir, a.SHA256)
	if sum, err := fileDigest(path); err == nil && sum == a.SHA256 {
		return path, nil
	}
	if err := os.MkdirAll(c.Dir, 0o750); err != nil {
		return "", err
	}
	if err := c.download(ctx, a, path); err != nil {
		return "", err
	}
	return path, nil
}

// open returns the artifact's bytes from Fetch or its URL.
func (c Cache) open(ctx context.Context, a domain.Artifact) (io.ReadCloser, error) {
	if c.Fetch != nil {
		rc, err := c.Fetch(ctx, a)
		if !errors.Is(err, ErrUseURL) {
			return rc, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", a.URL, resp.Status)
	}
	return resp.Body, nil
}

func (c Cache) download(ctx context.Context, a domain.Artifact, dst string) error {
	src, err := c.open(ctx, a)
	if err != nil {
		return err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(c.Dir, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	var body io.Reader = src
	if a.Size > 0 {
		body = io.LimitReader(src, a.Size+1)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("GET %s: %w", a.URL, err)
	}
	if a.Size > 0 && n != a.Size {
		return fmt.Errorf("GET %s: %w: got %d bytes, want %d", a.URL, ErrSizeMismatch, n, a.Size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != a.SHA256 {
		return fmt.Errorf("GET %s: %w: got %s", a.URL, ErrDigestMismatch, sum)
	}
	return os.Rename(tmp.Name(), dst)
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
