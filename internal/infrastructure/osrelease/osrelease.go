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

// Distro reads an os-release file and returns "<ID>-<VERSION_ID>", e.g.
// "ubuntu-26.04".
func Distro(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && !strings.HasPrefix(k, "#") {
			vals[k] = strings.ToLower(strings.Trim(v, `"'`))
		}
	}
	if vals["ID"] == "" || vals["VERSION_ID"] == "" {
		return "", fmt.Errorf("%s: ID or VERSION_ID missing", path)
	}
	return vals["ID"] + "-" + vals["VERSION_ID"], nil
}
