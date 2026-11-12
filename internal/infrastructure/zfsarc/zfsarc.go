// Package zfsarc caps the ZFS ARC so it does not starve the database servers.
package zfsarc

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds the system paths (overridable in tests).
type Config struct {
	MemInfo  string // /proc/meminfo
	Param    string // /sys/module/zfs/parameters/zfs_arc_max
	Modprobe string // /etc/modprobe.d
}

// System is the real system.
var System = Config{
	MemInfo:  "/proc/meminfo",
	Param:    "/sys/module/zfs/parameters/zfs_arc_max",
	Modprobe: "/etc/modprobe.d",
}

const confName = "db-branch-zfs.conf"

// DefaultMax is a quarter of physical memory.
func (c Config) DefaultMax() (uint64, error) {
	total, err := c.MemTotal()
	return total / 4, err
}

// MemTotal is the physical memory in bytes.
func (c Config) MemTotal() (uint64, error) {
	f, err := os.Open(c.MemInfo)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb * 1024, nil
		}
	}
	return 0, fmt.Errorf("%s: MemTotal not found", c.MemInfo)
}

// Conflict returns the modprobe file, other than db-branch's own, that sets
// zfs_arc_max ("" if none).
func (c Config) Conflict() string {
	entries, err := os.ReadDir(c.Modprobe)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.Name() == confName {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(c.Modprobe, e.Name()))
		if strings.Contains(string(b), "zfs_arc_max") {
			return filepath.Join(c.Modprobe, e.Name())
		}
	}
	return ""
}

// Current is the cap the running kernel uses (0: ZFS's own default).
func (c Config) Current() (uint64, error) {
	b, err := os.ReadFile(c.Param)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// Set makes max the cap now and on future boots, whatever other modprobe
// files say (the caller reports Conflict).
func (c Config) Set(max uint64) error {
	value := strconv.FormatUint(max, 10)
	if err := os.WriteFile(c.Param, []byte(value+"\n"), 0o644); err != nil {
		return fmt.Errorf("set zfs_arc_max: %w", err)
	}
	return c.persist(value)
}

func (c Config) persist(value string) error {
	conf := "# Written by db-branch (`db-branch init`, or the cluster setting zfs.arc_max): keep the ARC from starving the database servers.\noptions zfs zfs_arc_max=" + value + "\n"
	return os.WriteFile(filepath.Join(c.Modprobe, confName), []byte(conf), 0o644)
}

// Apply sets zfs_arc_max now and on future boots. If another modprobe file
// already sets it, the administrator's choice wins and nothing is changed.
func (c Config) Apply(max uint64) (string, error) {
	if other := c.Conflict(); other != "" {
		return fmt.Sprintf("ZFS ARC limit left unchanged: already set in %s", other), nil
	}
	if _, err := os.ReadDir(c.Modprobe); err != nil {
		return "", err
	}
	if err := c.Set(max); err != nil {
		return "", err
	}
	return fmt.Sprintf("ZFS ARC limited to %.1f GiB", float64(max)/(1<<30)), nil
}
