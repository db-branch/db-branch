// Package mysql holds MySQL-specific infrastructure.
package mysql

import (
	"bytes"
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// wrapperMark starts the launcher WrapLegacy writes, so a second run knows it.
const wrapperMark = "# db-branch: launcher for a binary without an RPATH"

// HasRunpath reports whether an ELF executable names its own library
// directories (RPATH or RUNPATH). The 8.x tarballs do ($ORIGIN/../lib/private);
// the old 5.x ones do not.
func HasRunpath(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	for _, tag := range []elf.DynTag{elf.DT_RUNPATH, elf.DT_RPATH} {
		if v, err := f.DynString(tag); err == nil && len(v) > 0 {
			return true
		}
	}
	return false
}

// WrapLegacy makes the compat links in lib/private reachable for binaries that
// have no RPATH of their own (MySQL 5.x): such a binary that needs a library
// the links stand in for is renamed to <name>.real and replaced by a small
// launcher that puts lib/private on LD_LIBRARY_PATH and runs it.
func WrapLegacy(runtimeDir string, needed func(binary string) ([]string, error), hasRunpath func(binary string) bool) error {
	private := filepath.Join(runtimeDir, "lib", "private")
	links, err := os.ReadDir(private)
	if err != nil || len(links) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, l := range links {
		have[l.Name()] = true
	}
	bins, err := os.ReadDir(filepath.Join(runtimeDir, "bin"))
	if err != nil {
		return nil
	}
	for _, b := range bins {
		path := filepath.Join(runtimeDir, "bin", b.Name())
		if b.IsDir() || strings.HasSuffix(b.Name(), ".real") || isLauncher(path) || hasRunpath(path) {
			continue
		}
		libs, err := needed(path)
		if err != nil || !anyIn(libs, have) {
			continue
		}
		if err := os.Rename(path, path+".real"); err != nil {
			return err
		}
		script := wrapperMark + "\n" + `d=$(dirname "$(readlink -f "$0")")` + "\n" +
			`LD_LIBRARY_PATH="$d/../lib/private${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" exec "$d/` + b.Name() + `.real" "$@"` + "\n"
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func anyIn(libs []string, set map[string]bool) bool {
	for _, l := range libs {
		if set[l] {
			return true
		}
	}
	return false
}

func isLauncher(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 4096 {
		return false
	}
	return bytes.Contains(b, []byte(wrapperMark))
}

// DropDebug removes the debug server and plugins that the full (non-minimal)
// upstream tarballs carry: bin/*-debug and lib/plugin/debug. db-branch never
// runs them, and on arm64 (no minimal build) they take about 500 MB.
func DropDebug(runtimeDir string) error {
	drop := []string{filepath.Join(runtimeDir, "lib", "plugin", "debug")}
	entries, _ := os.ReadDir(filepath.Join(runtimeDir, "bin")) // no bin/: nothing to drop
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-debug") {
			drop = append(drop, filepath.Join(runtimeDir, "bin", e.Name()))
		}
	}
	for _, p := range drop {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// Fixup prepares an unpacked MySQL runtime for this system: no debug
// binaries, libaio under its t64 name, ncurses 5 for the clients of old
// versions, and launchers for the binaries of 5.x that cannot find those
// links by themselves.
func Fixup(libDirs []string, needed func(binary string) ([]string, error)) func(dir string) error {
	return func(dir string) error {
		if err := DropDebug(dir); err != nil {
			return err
		}
		if err := FixLibaio(dir, libDirs); err != nil {
			return err
		}
		var libs []string
		for _, bin := range []string{"mysql", "mysqladmin", "mysqldump"} {
			l, _ := needed(filepath.Join(dir, "bin", bin))
			libs = append(libs, l...)
		}
		if err := FixNcurses(dir, libs, libDirs); err != nil {
			return err
		}
		return WrapLegacy(dir, needed, HasRunpath)
	}
}
