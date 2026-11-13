// Package portreserve keeps the kernel from handing the branch ports to
// outgoing connections.
//
// Branch ports (default 40000-40999) lie inside Linux's ephemeral range
// (32768-60999). An outgoing connection of any process can take one of them
// as its local port, and a server that is then started on it fails with
// "Address already in use". Listing the range in
// net.ipv4.ip_local_reserved_ports stops that: the kernel no longer picks
// those ports by itself, and binding them explicitly still works.
package portreserve

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Path is the sysctl file (overridable in tests).
var Path = "/proc/sys/net/ipv4/ip_local_reserved_ports"

type span struct{ lo, hi int }

func parse(s string) ([]span, error) {
	var out []span
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		lo, hi, found := strings.Cut(f, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("reserved ports %q: %w", s, err)
		}
		b := a
		if found {
			if b, err = strconv.Atoi(hi); err != nil {
				return nil, fmt.Errorf("reserved ports %q: %w", s, err)
			}
		}
		out = append(out, span{a, b})
	}
	return out, nil
}

// merge sorts and joins overlapping or adjacent spans.
func merge(in []span) []span {
	sort.Slice(in, func(i, j int) bool { return in[i].lo < in[j].lo })
	var out []span
	for _, s := range in {
		if n := len(out); n > 0 && s.lo <= out[n-1].hi+1 {
			if s.hi > out[n-1].hi {
				out[n-1].hi = s.hi
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

func format(in []span) string {
	parts := make([]string, len(in))
	for i, s := range in {
		if s.lo == s.hi {
			parts[i] = strconv.Itoa(s.lo)
		} else {
			parts[i] = fmt.Sprintf("%d-%d", s.lo, s.hi)
		}
	}
	return strings.Join(parts, ",")
}

// Reserve adds min-max to the reserved ports, keeping what is already
// reserved. It reports whether it changed anything.
func Reserve(min, max int) (bool, error) {
	if min <= 0 || max < min || max > 65535 {
		return false, fmt.Errorf("port range %d-%d is not valid", min, max)
	}
	b, err := os.ReadFile(Path)
	if err != nil {
		return false, err
	}
	have, err := parse(string(b))
	if err != nil {
		return false, err
	}
	have = merge(have)
	for _, s := range have {
		if s.lo <= min && max <= s.hi {
			return false, nil
		}
	}
	want := format(merge(append(have, span{min, max})))
	if err := os.WriteFile(Path, []byte(want+"\n"), 0o644); err != nil {
		return false, err
	}
	return true, nil
}
