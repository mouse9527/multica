package passwordauth

import (
	"crypto/sha256"
	"net"
	"sync"
	"time"
)

// Account lockout: after LockThreshold consecutive failures the account is
// locked for LockBase, doubling with every further failure up to LockMax. The
// count only goes back to zero on a successful login or a new password, so an
// attacker who waits out a lock gets one guess before a longer one.
const (
	LockThreshold = 5
	LockBase      = time.Minute
	LockMax       = time.Hour
)

// LockDuration is how long the account is locked after its failedCount-th
// consecutive failure; zero below LockThreshold.
func LockDuration(failedCount int32) time.Duration {
	if failedCount < LockThreshold {
		return 0
	}
	d := LockBase
	for i := int32(LockThreshold); i < failedCount && d < LockMax; i++ {
		d *= 2
	}
	return min(d, LockMax)
}

// Per-IP limit on login attempts, kept in process memory because the
// deployment has no Redis. Each replica counts on its own, so with N replicas
// an address gets up to N times the limit.
const (
	IPLimit  = 10
	IPWindow = time.Minute
)

// maxTrackedKeys bounds each in-memory table. Entries expire on their own
// (IPWindow for addresses, phantomTTL for emails); this only matters under a
// flood of distinct keys.
const maxTrackedKeys = 10000

// ipLimiter is a fixed-window counter per client address. IPv6 addresses are
// counted per /64, the block one subscriber usually holds.
type ipLimiter struct {
	mu      sync.Mutex
	windows map[string]ipWindow
}

type ipWindow struct {
	start time.Time
	count int
}

func newIPLimiter() *ipLimiter {
	return &ipLimiter{windows: make(map[string]ipWindow)}
}

// allow counts one attempt from ip and reports whether it is within the
// limit. When the table is full of live windows a new address is let through
// uncounted: refusing it would let a flood of addresses lock everyone out,
// and the account lockout still applies.
func (l *ipLimiter) allow(ip string, now time.Time) bool {
	key := ipKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) >= IPWindow {
		if !ok && len(l.windows) >= maxTrackedKeys {
			for k, old := range l.windows {
				if now.Sub(old.start) >= IPWindow {
					delete(l.windows, k)
				}
			}
			if len(l.windows) >= maxTrackedKeys {
				return true
			}
		}
		w = ipWindow{start: now}
	}
	w.count++
	l.windows[key] = w
	return w.count <= IPLimit
}

func ipKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() != nil {
		return ip
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String()
}

// phantomTTL is how long failures against an email without a password are
// remembered. Real accounts keep their count until a successful login, so
// after this long without attempts the two cases can differ again; an
// attacker would have to pause for a day per probe.
const phantomTTL = 24 * time.Hour

// phantomLocks applies the account lockout to emails that have no password
// (no such user, or a user who never set one). Without it, LockThreshold
// wrong guesses would lock a real account but never a missing one, and the
// lock response would tell which emails have passwords. Kept in memory: it
// protects nothing, it only has to look the same, and it is lost on restart.
type phantomLocks struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]phantomEntry
}

type phantomEntry struct {
	failed      int32
	lockedUntil time.Time
	lastFailure time.Time
}

func newPhantomLocks() *phantomLocks {
	return &phantomLocks{entries: make(map[[sha256.Size]byte]phantomEntry)}
}

// The table is keyed by a digest so it does not hold what attackers typed.
func phantomKey(email string) [sha256.Size]byte {
	return sha256.Sum256([]byte(email))
}

func (p *phantomLocks) locked(email string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[phantomKey(email)]
	return ok && now.Before(e.lockedUntil)
}

func (p *phantomLocks) recordFailure(email string, now time.Time) {
	key := phantomKey(email)
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[key]
	if ok && now.Sub(e.lastFailure) >= phantomTTL {
		e = phantomEntry{}
	}
	if !ok && len(p.entries) >= maxTrackedKeys {
		for k, old := range p.entries {
			if now.Sub(old.lastFailure) >= phantomTTL {
				delete(p.entries, k)
			}
		}
		// Still full: drop an arbitrary entry. Losing one only makes that
		// email unlock early.
		for k := range p.entries {
			if len(p.entries) < maxTrackedKeys {
				break
			}
			delete(p.entries, k)
		}
	}
	e.failed++
	e.lastFailure = now
	if d := LockDuration(e.failed); d > 0 {
		e.lockedUntil = now.Add(d)
	}
	p.entries[key] = e
}
