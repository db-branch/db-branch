package httpapi

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// IPLimiter limits how often each client may do something: a sliding window
// kept in memory only, keyed by whatever string the caller passes (see
// ClientKey). Nothing is persisted or logged; a restart forgets everything.
// The zero value works.
type IPLimiter struct {
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu        sync.Mutex
	hits      map[string][]time.Time
	lastSweep time.Time
}

func (l *IPLimiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Allow records one use for key and reports true, unless key already used
// limit times within window; then it records nothing and says how long until
// the oldest use leaves the window. limit <= 0 means unlimited.
func (l *IPLimiter) Allow(key string, limit int, window time.Duration) (bool, time.Duration) {
	if limit <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now, window)
	uses := l.recent(key, now, window)
	if len(uses) >= limit {
		return false, uses[len(uses)-limit].Add(window).Sub(now)
	}
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	l.hits[key] = append(uses, now)
	return true, 0
}

// Refund takes back the latest use recorded for key (a request that was
// refused after all must not count).
func (l *IPLimiter) Refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	uses := l.hits[key]
	switch {
	case len(uses) == 0:
	case len(uses) == 1:
		delete(l.hits, key)
	default:
		l.hits[key] = uses[:len(uses)-1]
	}
}

// Len is how many clients are remembered.
func (l *IPLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}

// recent drops the uses of key that left the window and returns the rest.
func (l *IPLimiter) recent(key string, now time.Time, window time.Duration) []time.Time {
	uses := l.hits[key]
	i := 0
	for i < len(uses) && !uses[i].Add(window).After(now) {
		i++
	}
	uses = uses[i:]
	if len(uses) == 0 {
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = uses
	return uses
}

// sweep forgets every client whose uses all left the window, at most once
// per window, so memory follows the clients seen within the last hour.
func (l *IPLimiter) sweep(now time.Time, window time.Duration) {
	if now.Sub(l.lastSweep) < window/4 {
		return
	}
	l.lastSweep = now
	for key := range l.hits {
		l.recent(key, now, window)
	}
}

// ClientKey is the address a request came from, as the limiter's key: the
// IP without the port, IPv6 grouped by its /64 (one subscriber holds a whole
// /64, so counting single addresses would be no limit at all). Only the
// connection's own address counts; proxy headers such as X-Forwarded-For can
// be set by anyone and are never read.
func ClientKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return remoteAddr
	}
	ip = ip.Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}
