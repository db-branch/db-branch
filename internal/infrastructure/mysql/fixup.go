// Package mysql holds MySQL-specific infrastructure.
package mysql

import (
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
)

// SystemLibDirs returns where Debian/Ubuntu install shared libraries for arch.
func SystemLibDirs(arch string) []string {
	multiarch := map[string]string{"amd64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu"}[arch]
	return []string{"/usr/lib/" + multiarch, "/lib/" + multiarch, "/usr/lib"}
}

// FixLibaio makes libaio.so.1 resolvable for the upstream mysqld binary.
//
// Ubuntu 24.04 and Debian 13 ship the library as libaio.so.1t64 only. The
// upstream binary looks in $ORIGIN/../lib/private first, so a symlink there
// fixes it without touching the system.
func FixLibaio(runtimeDir string, libDirs []string) error {
	private := filepath.Join(runtimeDir, "lib", "private")
	if exists(filepath.Join(private, "libaio.so.1")) {
		return nil
	}
	for _, d := range libDirs {
		if exists(filepath.Join(d, "libaio.so.1")) {
			return nil
		}
	}
	for _, d := range libDirs {
		t64 := filepath.Join(d, "libaio.so.1t64")
		if !exists(t64) {
			continue
		}
		if err := os.MkdirAll(private, 0o755); err != nil {
			return err
		}
		return os.Symlink(t64, filepath.Join(private, "libaio.so.1"))
	}
	return fmt.Errorf("libaio not found in %v: install libaio1t64 (or libaio1 on older releases)", libDirs)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ncursesSuccessor maps the ncurses 5 libraries older MySQL clients (8.0)
// link against to the ncurses 6 ones current distributions ship. The
// client only uses them for terminal handling, which is compatible.
var ncursesSuccessor = map[string]string{
	"libncurses.so.5": "libncurses.so.6",
	"libtinfo.so.5":   "libtinfo.so.6",
}

// FixNcurses links the ncurses 5 libraries in needed to their version 6
// successors in lib/private, when the system lacks them.
func FixNcurses(runtimeDir string, needed, libDirs []string) error {
	private := filepath.Join(runtimeDir, "lib", "private")
	for _, lib := range needed {
		next, old := ncursesSuccessor[lib]
		if !old || inAny(lib, append([]string{private}, libDirs...)) {
			continue
		}
		target := ""
		for _, d := range libDirs {
			if exists(filepath.Join(d, next)) {
				target = filepath.Join(d, next)
				break
			}
		}
		if target == "" {
			return fmt.Errorf("%s not found in %v (needed as %s): install libncurses6", next, libDirs, lib)
		}
		if err := os.MkdirAll(private, 0o755); err != nil {
			return err
		}
		if err := os.Symlink(target, filepath.Join(private, lib)); err != nil {
			return err
		}
	}
	return nil
}

func inAny(lib string, dirs []string) bool {
	for _, d := range dirs {
		if exists(filepath.Join(d, lib)) {
			return true
		}
	}
	return false
}

// NeededLibs lists the shared libraries an ELF executable links against.
func NeededLibs(path string) ([]string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ImportedLibraries()
}

// Fixup prepares an unpacked MySQL runtime for this system: libaio under
// its t64 name, and ncurses 5 for 8.0 clients.
func Fixup(libDirs []string, needed func(binary string) ([]string, error)) func(dir string) error {
	return func(dir string) error {
		if err := FixLibaio(dir, libDirs); err != nil {
			return err
		}
		var libs []string
		for _, bin := range []string{"mysql", "mysqladmin", "mysqldump"} {
			l, _ := needed(filepath.Join(dir, "bin", bin))
			libs = append(libs, l...)
		}
		return FixNcurses(dir, libs, libDirs)
	}
}
