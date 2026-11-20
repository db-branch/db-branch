package domain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ErrUnsafeURL is returned for a download URL that could make the node reach
// something inside its own network (SSRF).
var ErrUnsafeURL = errors.New("unsafe URL")

// Resolver looks up the addresses of a host name.
type Resolver func(ctx context.Context, host string) ([]net.IP, error)

// blockedV4 are the IPv4 ranges that are not on the public internet.
var blockedV4 = mustCIDRs(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
)

// publicV6 is the IPv6 space assigned for global unicast (2000::/3). Everything
// else (loopback, link-local, unique-local, IPv4-compatible and -translated
// forms, multicast, reserved space) is not public by construction.
var publicV6 = mustCIDRs("2000::/3")

// blockedV6 are the special-purpose ranges inside 2000::/3: embedded IPv4
// (NAT64, 6to4, Teredo), benchmarking, documentation and the ORCHID ranges.
var blockedV6 = mustCIDRs("2001::/32", "2001:2::/48", "2001:10::/28", "2001:20::/28", "2001:db8::/32", "2002::/16", "3fff::/20")

func mustCIDRs(list ...string) []*net.IPNet {
	out := make([]*net.IPNet, len(list))
	for i, c := range list {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out[i] = n
	}
	return out
}

func inAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// IsPublicIP reports whether ip is an address on the public internet. It is an
// allow-list for IPv6 (global unicast only) so a form nobody thought of is refused.
func IsPublicIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil { // also IPv4 written as ::ffff:a.b.c.d
		return !inAny(v4, blockedV4)
	}
	if len(ip) != net.IPv6len {
		return false
	}
	return inAny(ip, publicV6) && !inAny(ip, blockedV6)
}

// ValidateCatalogURL accepts only an https URL, without credentials, whose host
// is on the public internet: an address literal, or a name that resolves only
// to public addresses. A download must also connect only to such addresses
// when it runs (see artifact.PublicOnly): a name can resolve differently later.
func ValidateCatalogURL(ctx context.Context, raw string, resolve Resolver) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w %q: %s", ErrUnsafeURL, raw, fmt.Sprintf(format, a...))
	}
	if hasControl(raw) || strings.ContainsAny(raw, " \\") {
		return bad("contains a space, backslash or control character")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return bad("%v", err)
	}
	if u.Scheme != "https" {
		return bad("only https is allowed")
	}
	if u.User != nil {
		return bad("credentials in the URL")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return bad("no host")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return bad("port %q", p)
		}
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return bad("localhost")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !IsPublicIP(ip) {
			return bad("%s is not a public address", ip)
		}
		return nil
	}
	if resolve == nil {
		return bad("cannot resolve %s: no resolver", host)
	}
	ips, err := resolve(ctx, host)
	if err != nil {
		return bad("cannot resolve %s: %v", host, err)
	}
	if len(ips) == 0 {
		return bad("%s resolves to nothing", host)
	}
	for _, ip := range ips {
		if !IsPublicIP(ip) {
			return bad("%s resolves to %s, which is not a public address", host, ip)
		}
	}
	return nil
}
