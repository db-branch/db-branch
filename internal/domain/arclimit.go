package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// ArcMinBytes is the smallest ARC cap an administrator may set.
const ArcMinBytes = 64 << 20

// ArcReserve is the memory an ARC cap must leave free: 1 GiB or 10% of
// physical memory, whichever is larger.
func ArcReserve(mem uint64) uint64 {
	return max(mem/10, 1<<30)
}

// ArcLimit is the cluster setting zfs.arc_max: a share of each node's
// physical memory ("50%") or a size ("8GB"). The zero value is "unset", which
// keeps the value the node was initialised with.
type ArcLimit struct {
	percent float64
	bytes   uint64
}

const arcExamples = `use a percentage of memory such as "50%" or a size such as "8GB" or "512MB"`

// Empty reports whether the limit is unset.
func (l ArcLimit) Empty() bool { return l.percent == 0 && l.bytes == 0 }

// ParseArcLimit reads "", "<n>%" or "<n><unit>" (KB, MB, GB, TB; binary
// multiples, with or without the trailing B or an "i").
func ParseArcLimit(s string) (ArcLimit, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ArcLimit{}, nil
	}
	bad := func(why string) (ArcLimit, error) {
		return ArcLimit{}, fmt.Errorf("zfs.arc_max %q: %s; %s", s, why, arcExamples)
	}
	if num, ok := strings.CutSuffix(s, "%"); ok {
		p, err := parsePositive(num)
		if err != nil {
			return bad("not a number before %")
		}
		if p > 100 {
			return bad("more than 100%")
		}
		return ArcLimit{percent: p}, nil
	}
	lower := strings.ToLower(s)
	units := []struct {
		suffix string
		mult   float64
	}{{"tib", 1 << 40}, {"tb", 1 << 40}, {"t", 1 << 40}, {"gib", 1 << 30}, {"gb", 1 << 30}, {"g", 1 << 30},
		{"mib", 1 << 20}, {"mb", 1 << 20}, {"m", 1 << 20}, {"kib", 1 << 10}, {"kb", 1 << 10}, {"k", 1 << 10}}
	for _, u := range units {
		if num, ok := strings.CutSuffix(lower, u.suffix); ok {
			n, err := parsePositive(num)
			if err != nil {
				return bad("not a number before the unit")
			}
			return ArcLimit{bytes: uint64(n * u.mult)}, nil
		}
	}
	return bad("missing or unknown unit")
}

// parsePositive reads a plain decimal greater than zero (no exponent, sign
// or "inf").
func parsePositive(s string) (float64, error) {
	s = strings.TrimSpace(s)
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return 0, fmt.Errorf("not a number")
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("not a positive number")
	}
	return f, nil
}

// Resolve returns the cap in bytes on a node with mem bytes of physical
// memory, or an error when it is below ArcMinBytes or leaves less than
// ArcReserve(mem) free.
func (l ArcLimit) Resolve(mem uint64) (uint64, error) {
	if l.Empty() {
		return 0, fmt.Errorf("zfs.arc_max is not set")
	}
	v := l.bytes
	if l.percent > 0 {
		v = uint64(float64(mem) * l.percent / 100)
	}
	if v < ArcMinBytes {
		return 0, fmt.Errorf("zfs.arc_max is %d MB here; it must be at least 64 MB", v>>20)
	}
	if top := mem - min(mem, ArcReserve(mem)); v > top {
		return 0, fmt.Errorf("zfs.arc_max is %d MB here; it must be at most %d MB (this node's %d MB of memory minus a reserve of %d MB for the system and branch servers)",
			v>>20, top>>20, mem>>20, ArcReserve(mem)>>20)
	}
	return v, nil
}
