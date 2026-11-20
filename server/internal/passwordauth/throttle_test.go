package passwordauth

import (
	"fmt"
	"testing"
	"time"
)

func TestEnabled(t *testing.T) {
	for value, want := range map[string]bool{
		"": false, "false": false, "0": false, "yes": false,
		"true": true, "TRUE": true, " 1 ": true,
	} {
		got := Enabled(func(name string) string {
			if name != EnvEnabled {
				t.Fatalf("read %s", name)
			}
			return value
		})
		if got != want {
			t.Errorf("Enabled(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestIPLimitEnabled(t *testing.T) {
	for value, want := range map[string]bool{
		"": true, "1": true, "true": true, "20": true, "off": true,
		"0": false, "false": false, " FALSE ": false,
	} {
		got := IPLimitEnabled(func(name string) string {
			if name != EnvIPLimit {
				t.Fatalf("read %s", name)
			}
			return value
		})
		if got != want {
			t.Errorf("IPLimitEnabled(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestLockDuration(t *testing.T) {
	for n, want := range map[int32]time.Duration{
		0:  0,
		4:  0,
		5:  time.Minute,
		6:  2 * time.Minute,
		7:  4 * time.Minute,
		10: 32 * time.Minute,
		11: time.Hour,
		50: time.Hour,
	} {
		if got := LockDuration(n); got != want {
			t.Errorf("LockDuration(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestIPLimiter(t *testing.T) {
	l := newIPLimiter()
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < IPLimit; i++ {
		if !l.allow("203.0.113.7", now) {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	if l.allow("203.0.113.7", now) {
		t.Fatal("attempt over the limit allowed")
	}
	if !l.allow("203.0.113.8", now) {
		t.Fatal("another address was limited")
	}
	if !l.allow("203.0.113.7", now.Add(IPWindow)) {
		t.Fatal("limit did not reset after the window")
	}

	// One /64 shares a budget.
	for i := 0; i < IPLimit; i++ {
		l.allow(fmt.Sprintf("2001:db8:1:2::%x", i), now)
	}
	if l.allow("2001:db8:1:2:ffff::1", now) {
		t.Fatal("addresses of one /64 were counted apart")
	}
	if !l.allow("2001:db8:1:3::1", now) {
		t.Fatal("another /64 was limited")
	}
}

func TestIPLimiterFullTable(t *testing.T) {
	l := newIPLimiter()
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < maxTrackedKeys; i++ {
		l.allow(fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&0xff, i&0xff), now)
	}
	// Full of live windows: a new address passes uncounted.
	if !l.allow("198.51.100.1", now) || len(l.windows) != maxTrackedKeys {
		t.Fatalf("full table: windows=%d", len(l.windows))
	}
	// Expired windows make room.
	if !l.allow("198.51.100.1", now.Add(IPWindow)) || len(l.windows) != 1 {
		t.Fatalf("after expiry: windows=%d", len(l.windows))
	}
}

func TestPhantomLocks(t *testing.T) {
	p := newPhantomLocks()
	now := time.Unix(1_700_000_000, 0)
	const email = "nobody@example.test"
	for i := 0; i < LockThreshold; i++ {
		if p.locked(email, now) {
			t.Fatalf("locked after %d failures", i)
		}
		p.recordFailure(email, now)
	}
	if !p.locked(email, now) || p.locked("other@example.test", now) {
		t.Fatal("lock not applied to exactly the failing email")
	}
	now = now.Add(LockDuration(LockThreshold))
	if p.locked(email, now) {
		t.Fatal("lock did not expire")
	}
	p.recordFailure(email, now)
	if !p.locked(email, now.Add(LockDuration(LockThreshold+1)-time.Second)) {
		t.Fatal("second lock did not double")
	}
	// Forgotten after phantomTTL without failures.
	now = now.Add(phantomTTL)
	p.recordFailure(email, now)
	if p.locked(email, now) {
		t.Fatal("count survived phantomTTL")
	}
}
