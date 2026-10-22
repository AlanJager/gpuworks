package api

import (
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

func TestLoginThrottleBoundsAccountAndClientAndExpires(t *testing.T) {
	now := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	for _, kind := range []string{"account", "client"} {
		t.Run(kind, func(t *testing.T) {
			var limiter loginThrottle
			keys := func(i int) (string, string) {
				if kind == "account" {
					return fmt.Sprintf("192.0.2.%d", i), "admin"
				}
				return "192.0.2.1", fmt.Sprintf("name-%d", i)
			}
			for i := 0; i < loginFailureAllowance; i++ {
				ip, user := keys(i)
				finish, wait := limiter.begin(now, ip, user)
				if wait != 0 {
					t.Fatalf("attempt %d wait=%s", i, wait)
				}
				finish(false, now)
			}
			ip, user := keys(20)
			if _, wait := limiter.begin(now, ip, user); wait != time.Second {
				t.Fatalf("initial lock wait=%s", wait)
			}
			// Rejected probes neither lengthen a lock nor create entries.
			before := len(limiter.entries)
			if _, wait := limiter.begin(now.Add(time.Second/2), ip, user); wait != time.Second/2 {
				t.Fatalf("rejected lock wait=%s", wait)
			}
			if len(limiter.entries) != before {
				t.Fatal("rejected attempt allocated records")
			}
			finish, wait := limiter.begin(now.Add(time.Second), ip, user)
			if wait != 0 {
				t.Fatalf("lock failed to expire: %s", wait)
			}
			finish(false, now.Add(time.Second))
			if _, wait := limiter.begin(now.Add(time.Second), ip, user); wait != 2*time.Second {
				t.Fatalf("second lock wait=%s", wait)
			}
			// Successful authentication clears a previous failure history.
			finish, wait = limiter.begin(now.Add(3*time.Second), ip, user)
			if wait != 0 {
				t.Fatalf("second lock failed to expire: %s", wait)
			}
			finish(true, now.Add(3*time.Second))
			finish, wait = limiter.begin(now.Add(3*time.Second), ip, user)
			if wait != 0 {
				t.Fatalf("success did not reset lock: %s", wait)
			}
			finish(false, now.Add(3*time.Second))
			// Idle histories recover automatically without extending permanent locks.
			finish, wait = limiter.begin(now.Add(loginThrottleRetention+time.Minute), ip, user)
			if wait != 0 {
				t.Fatalf("idle lock wait=%s", wait)
			}
			finish(false, now.Add(loginThrottleRetention+time.Minute))
			if len(limiter.entries) != 2 {
				t.Fatalf("expired records remain: %d", len(limiter.entries))
			}
		})
	}
}

func TestLoginThrottleBoundsConcurrentPasswordWork(t *testing.T) {
	var limiter loginThrottle
	now := time.Now()
	var allowed atomic.Int32
	var wg sync.WaitGroup
	release := make(chan struct{})
	reserved := make(chan struct{}, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			finish, wait := limiter.begin(now, "192.0.2.1", "admin")
			reserved <- struct{}{}
			if wait == 0 {
				allowed.Add(1)
				<-release
				finish(false, now)
			}
		}()
	}
	for i := 0; i < 100; i++ {
		<-reserved
	}
	close(release)
	wg.Wait()
	if allowed.Load() != loginFailureAllowance {
		t.Fatalf("concurrent password checks=%d", allowed.Load())
	}
	if _, wait := limiter.begin(now, "192.0.2.1", "admin"); wait <= 0 {
		t.Fatal("failed concurrent attempts did not lock")
	}
}

func TestLoginThrottleHasBoundedStorageAndBackoff(t *testing.T) {
	var limiter loginThrottle
	now := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	for i := 0; i < loginThrottleCapacity/2; i++ {
		finish, wait := limiter.begin(now, fmt.Sprint(i), fmt.Sprint(i))
		if wait != 0 {
			t.Fatalf("unexpected table exhaustion at %d", i)
		}
		finish(false, now)
	}
	if len(limiter.entries) != loginThrottleCapacity {
		t.Fatalf("record count=%d", len(limiter.entries))
	}
	if _, wait := limiter.begin(now, "new", "new"); wait <= 0 {
		t.Fatal("full table accepted new identities")
	}
	finish, wait := limiter.begin(now.Add(loginThrottleRetention), "new", "new")
	if wait != 0 {
		t.Fatalf("full table failed to expire: %s", wait)
	}
	finish(false, now.Add(loginThrottleRetention))
	var backoff loginThrottle
	for i := 0; i < 25; i++ {
		finish, wait := backoff.begin(now, "ip", "name")
		if wait != 0 {
			t.Fatalf("expired backoff wait=%s", wait)
		}
		finish(false, now)
		if i < loginFailureAllowance-1 {
			continue
		}
		_, wait = backoff.begin(now, "ip", "name")
		if wait > loginThrottleMaxBackoff {
			t.Fatalf("unbounded backoff=%s", wait)
		}
		now = now.Add(wait)
	}
}

func TestLoginClientIPUsesTrustedProxyChain(t *testing.T) {
	proxies, _ := auth.NewProxyTrust("127.0.0.1,192.0.2.10", nil)
	a := apiHandlers{sessionPolicy: auth.SessionPolicy{Proxies: proxies}}
	for _, tc := range []struct{ peer, xff, want string }{
		{"198.51.100.7:4000", "203.0.113.1", "198.51.100.7"},
		{"127.0.0.1:4000", "203.0.113.1, 198.51.100.7", "198.51.100.7"},
		{"127.0.0.1:4000", "198.51.100.7, 192.0.2.10", "198.51.100.7"},
		{"127.0.0.1:4000", "198.51.100.7, bad", "127.0.0.1"},
		{"127.0.0.1:4000", "", "127.0.0.1"},
		{"[::ffff:198.51.100.7]:4000", "", "198.51.100.7"},
	} {
		req := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
		req.RemoteAddr = tc.peer
		req.Header.Set("X-Forwarded-For", tc.xff)
		if got := a.loginClientIP(req); got != tc.want {
			t.Errorf("peer=%s xff=%q got=%s want=%s", tc.peer, tc.xff, got, tc.want)
		}
	}
}

func TestSuccessfulAccountCannotResetClientSprayHistory(t *testing.T) {
	var limiter loginThrottle
	now := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	for i := 0; i < loginFailureAllowance-1; i++ {
		finish, wait := limiter.begin(now, "attacker", fmt.Sprintf("victim-%d", i))
		if wait != 0 {
			t.Fatal(wait)
		}
		finish(false, now)
		finish, wait = limiter.begin(now, "attacker", "known-account")
		if wait != 0 {
			t.Fatal(wait)
		}
		finish(true, now)
	}
	finish, wait := limiter.begin(now, "attacker", "last-victim")
	if wait != 0 {
		t.Fatal(wait)
	}
	finish(false, now)
	if _, wait := limiter.begin(now, "attacker", "another-victim"); wait <= 0 {
		t.Fatal("known-account success erased shared client failures")
	}
}
