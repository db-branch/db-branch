// Package osrelease names the Linux distribution this node runs on, for
// picking the runtime builds that match its system libraries.
package osrelease

import (
	"fmt"
	"os"
	"strings"
)

// Path is where systemd-era distributions describe themselves.
const Path = "/etc/os-release"

// Release is what an os-release file says about the system.
type Release struct {
	// Distro is "<ID>-<VERSION_ID>", e.g. "ubuntu-26.04".
	Distro string
	// Pretty is PRETTY_NAME, e.g. "Ubuntu 26.04.1 LTS" (the distro key when
	// the file has none).
	Pretty string
}

// Read parses an os-release file.
func Read(path string) (Release, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Release{}, err
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && !strings.HasPrefix(k, "#") {
			vals[k] = strings.Trim(v, `"'`)
		}
	}
	id, ver := strings.ToLower(vals["ID"]), strings.ToLower(vals["VERSION_ID"])
	if id == "" || ver == "" {
		return Release{}, fmt.Errorf("%s: ID or VERSION_ID missing", path)
	}
	r := Release{Distro: id + "-" + ver, Pretty: vals["PRETTY_NAME"]}
	if r.Pretty == "" {
		r.Pretty = r.Distro
	}
	return r, nil
}

// Distro reads an os-release file and returns "<ID>-<VERSION_ID>", e.g.
// "ubuntu-26.04".
func Distro(path string) (string, error) {
	r, err := Read(path)
	return r.Distro, err
}
