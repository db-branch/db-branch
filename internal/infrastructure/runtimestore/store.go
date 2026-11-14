// Package runtimestore keeps installed runtimes under <Dir>/<engine>/<version>/.
package runtimestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
)

// marker is written last, so a directory without it is not a finished install.
const marker = ".db-branch-runtime"

// Runner runs an external command.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Store implements domain.RuntimeStore on the local filesystem.
type Store struct {
	Dir string
	Run Runner
	// Fixups adapt an unpacked runtime to this system (e.g. library names) before it is
	// marked installed. Keyed by engine; optional.
	Fixups map[domain.Engine]func(dir string) error
}

var _ domain.RuntimeStore = Store{}

// Path is where ref is (or would be) installed. ref is validated, so the path stays under Dir.
func (s Store) Path(ref domain.RuntimeRef) string {
	return filepath.Join(s.Dir, string(ref.Engine), ref.Version)
}

// Has reports whether ref is fully installed. An unreadable path counts as
// not installed; Install then fails with the underlying filesystem error.
func (s Store) Has(_ context.Context, ref domain.RuntimeRef) (bool, error) {
	_, err := os.Stat(filepath.Join(s.Path(ref), marker))
	return err == nil, nil
}

// Install unpacks a tar.xz archive with a single top-level directory.
// It extracts into a staging directory and renames it into place, so a
// failed install never leaves a half-populated runtime behind.
func (s Store) Install(ctx context.Context, ref domain.RuntimeRef, archives []string) error {
	if ok, _ := s.Has(ctx, ref); ok {
		return fmt.Errorf("%s: %w", ref, domain.ErrRuntimeInstalled)
	}
	parent := filepath.Dir(s.Path(ref))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	staging := filepath.Join(parent, fmt.Sprintf(".staging-%d", rand.Int64()))
	if err := os.Mkdir(staging, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(staging) // no-op after a successful rename

	for _, archive := range archives {
		if err := s.unpack(ctx, archive, staging); err != nil {
			return fmt.Errorf("unpack %s: %w", ref, err)
		}
	}
	if fix := s.Fixups[ref.Engine]; fix != nil {
		if err := fix(staging); err != nil {
			return fmt.Errorf("prepare %s: %w", ref, err)
		}
	}
	if err := os.WriteFile(filepath.Join(staging, marker), []byte(ref.String()+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(staging, s.Path(ref)); err != nil {
		if ok, _ := s.Has(ctx, ref); ok {
			// Another install of the same version finished first.
			return fmt.Errorf("%s: %w", ref, domain.ErrRuntimeInstalled)
		}
		return err
	}
	return nil
}

// Remove deletes an installed runtime.
func (s Store) Remove(ctx context.Context, ref domain.RuntimeRef) error {
	if ok, _ := s.Has(ctx, ref); !ok {
		return fmt.Errorf("%s: %w", ref, domain.ErrRuntimeNotInstalled)
	}
	return os.RemoveAll(s.Path(ref))
}

// List returns installed runtimes sorted by engine and version string.
func (s Store) List(ctx context.Context) ([]domain.RuntimeRef, error) {
	engines, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var refs []domain.RuntimeRef
	for _, e := range engines {
		versions, err := os.ReadDir(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			continue
		}
		for _, v := range versions {
			ref, err := domain.ParseRuntimeRef(e.Name() + "@" + v.Name())
			if err != nil {
				continue
			}
			if ok, _ := s.Has(ctx, ref); ok {
				refs = append(refs, ref)
			}
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
	return refs, nil
}

// debMagic starts every .deb (an ar archive); gzipMagic every .tar.gz (the
// generic tarballs of MySQL 5.x).
const (
	debMagic  = "!<arch>\n"
	gzipMagic = "\x1f\x8b"
)

// unpack extracts a tar.xz or tar.gz (dropping its top-level directory, as upstream
// MySQL tarballs have one) or a .deb (its file tree as is) into dir.
func (s Store) unpack(ctx context.Context, archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	head := make([]byte, len(debMagic))
	n, _ := io.ReadFull(f, head)
	f.Close()
	if string(head[:n]) == debMagic {
		_, err = s.Run.Run(ctx, "dpkg-deb", "-x", archive, dir)
		return err
	}
	flag := "-xJf"
	if strings.HasPrefix(string(head[:n]), gzipMagic) {
		flag = "-xzf"
	}
	_, err = s.Run.Run(ctx, "tar", flag, archive, "-C", dir, "--strip-components=1", "--no-same-owner")
	return err
}
