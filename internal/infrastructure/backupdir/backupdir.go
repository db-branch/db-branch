// Package backupdir keeps backup files in a directory only root reads.
package backupdir

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/db-branch/db-branch/internal/domain"
)

// forbiddenExact lists root and shared top-level directories that must never
// be used directly as a backup directory.
var forbiddenExact = map[string]bool{
	"/": true, "/home": true, "/media": true, "/mnt": true, "/opt": true,
	"/srv": true, "/tmp": true, "/var": true, "/var/backups": true,
	"/var/lib": true, "/var/log": true, "/var/tmp": true,
	"/var/cache": true, "/var/spool": true,
	"/private": true, "/private/tmp": true, "/private/var": true,
	"/private/var/backups": true, "/private/var/cache": true, "/private/var/spool": true,
	"/private/var/lib": true, "/private/var/log": true, "/private/var/tmp": true,
}

// forbiddenPrefixes lists OS/runtime trees that must never contain backups.
var forbiddenPrefixes = []string{
	"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32",
	"/proc", "/root", "/run", "/sbin", "/sys", "/usr",
	"/var/lock", "/var/run", "/private/etc", "/private/var/lock", "/private/var/run",
}

// ValidatePath rejects empty, relative, traversal, or dangerous system paths
// for backup.dir.
func ValidatePath(p string) error {
	if p == "" || strings.TrimSpace(p) != p || strings.ContainsRune(p, 0) {
		return fmt.Errorf("backup directory %q is not a valid path", p)
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("backup directory %q must be an absolute path", p)
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return fmt.Errorf("backup directory %q must not contain '..'", p)
	}
	clean := filepath.Clean(p)
	if forbiddenExact[clean] {
		return fmt.Errorf("backup directory %q is a protected system directory", clean)
	}
	for _, prefix := range forbiddenPrefixes {
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			return fmt.Errorf("backup directory %q is inside a protected system tree (%s)", clean, prefix)
		}
	}
	return nil
}

// checkResolved resolves symlinks along the deepest existing ancestor of p and
// validates the resulting path so a symlink cannot redirect into a system dir.
func checkResolved(p string) error {
	cur := filepath.Clean(p)
	var rest []string
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			slices.Reverse(rest)
			return ValidatePath(filepath.Join(append([]string{resolved}, rest...)...))
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return err
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, `/\`)
}

func openValidatedRoot(path string, create bool) (*os.Root, error) {
	if err := ValidatePath(path); err != nil {
		return nil, err
	}
	clean := filepath.Clean(path)
	if err := checkResolved(clean); err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(filepath.Dir(clean), 0o700); err != nil {
			return nil, err
		}
		if err := os.Mkdir(clean, 0o700); err == nil {
			_ = os.Chmod(clean, 0o700)
		} else if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	fi, err := os.Lstat(clean)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return nil, fmt.Errorf("backup directory %q is not a real directory", clean)
	}
	return os.OpenRoot(clean)
}

// Dir implements backup.Dir.
type Dir struct{ Path string }

// file renames its temporary file into place when closed, so a backup is
// never seen half written.
type file struct {
	*os.File
	root  *os.Root
	tmp   string
	final string
}

func (f file) Close() error {
	err := f.File.Close()
	if err != nil {
		_ = f.root.Close()
		return err
	}
	err = f.root.Rename(f.tmp, f.final)
	return errors.Join(err, f.root.Close())
}

func (d Dir) Create(name string) (io.WriteCloser, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid backup name %q", name)
	}
	root, err := openValidatedRoot(d.Path, true)
	if err != nil {
		return nil, err
	}
	tmp := "." + name + ".tmp"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return file{File: f, root: root, tmp: tmp, final: name}, nil
}

// List returns the backups (*.json.gz), newest first.
func (d Dir) List() ([]domain.BackupFile, error) {
	root, err := openValidatedRoot(d.Path, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	var out []domain.BackupFile
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json.gz") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, domain.BackupFile{Name: e.Name(), Size: fi.Size(), TakenAt: fi.ModTime().UTC()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TakenAt.After(out[j].TakenAt) })
	return out, nil
}

func (d Dir) Remove(name string) error {
	if !validName(name) {
		return fmt.Errorf("backup %q: %w", name, domain.ErrNotFound)
	}
	root, err := openValidatedRoot(d.Path, false)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(name)
}

func (d Dir) Open(name string) (io.ReadCloser, error) {
	if !validName(name) {
		return nil, fmt.Errorf("backup %q: %w", name, domain.ErrNotFound)
	}
	root, err := openValidatedRoot(d.Path, false)
	if err == nil {
		defer root.Close()
		var fi fs.FileInfo
		if fi, err = root.Lstat(name); err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("backup %q must not be a symbolic link", name)
			}
			var f *os.File
			// O_NONBLOCK prevents blocking if name is a FIFO/device before f.Stat checks it.
			if f, err = root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
				if sfi, serr := f.Stat(); serr != nil || !sfi.Mode().IsRegular() {
					_ = f.Close()
					return nil, fmt.Errorf("backup %q is not a regular file", name)
				}
				return f, nil
			}
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("backup %s is not on this node: %w", name, domain.ErrNotFound)
	}
	return nil, err
}
