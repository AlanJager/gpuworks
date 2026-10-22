package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	loginFailureAllowance   = 5
	loginThrottleCapacity   = 8192
	loginThrottleRetention  = 30 * time.Minute
	loginThrottleMaxBackoff = 15 * time.Minute
)

type loginAttemptState struct {
	failures, inFlight    int
	blockedUntil, touched time.Time
}

// loginThrottle bounds password work, including concurrent attempts, by both
// client and account. Idle records expire; a full table fails closed rather
// than evicting a locked account or growing with attacker-controlled names.
// It is process-local: replicas require an upstream shared rate limiter.
type loginThrottle struct {
	mu        sync.Mutex
	entries   map[string]*loginAttemptState
	nextSweep time.Time
}

func (l *loginThrottle) begin(now time.Time, ip, username string) (func(bool, time.Time), time.Duration) {
	sum := sha256.Sum256([]byte(strings.TrimSpace(username)))
	keys := [2]string{"ip:" + ip, "user:" + hex.EncodeToString(sum[:])}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[string]*loginAttemptState)
	}
	if !now.Before(l.nextSweep) {
		for key, state := range l.entries {
			if state.inFlight == 0 && now.Sub(state.touched) >= loginThrottleRetention {
				delete(l.entries, key)
			}
		}
		l.nextSweep = now.Add(time.Minute)
	}
	var wait time.Duration
	missing := 0
	for _, key := range keys {
		state := l.entries[key]
		if state == nil {
			missing++
			continue
		}
		if state.inFlight == 0 && now.Sub(state.touched) >= loginThrottleRetention {
			*state = loginAttemptState{}
		}
		if delay := state.blockedUntil.Sub(now); delay > wait {
			wait = delay
		}
		if state.inFlight >= loginFailureAllowance ||
			(state.failures < loginFailureAllowance && state.inFlight+state.failures >= loginFailureAllowance) ||
			(state.failures >= loginFailureAllowance && state.inFlight > 0) {
			if wait < time.Second {
				wait = time.Second
			}
		}
	}
	if len(l.entries)+missing > loginThrottleCapacity && wait < time.Second {
		wait = time.Second
	}
	if wait > 0 {
		return nil, wait
	}
	states := [2]*loginAttemptState{}
	for i, key := range keys {
		state := l.entries[key]
		if state == nil {
			state = &loginAttemptState{}
			l.entries[key] = state
		}
		state.inFlight++
		state.touched = now
		states[i] = state
	}
	return func(success bool, completed time.Time) {
		l.mu.Lock()
		defer l.mu.Unlock()
		for i, state := range states {
			state.inFlight--
			if success {
				// A known account must not erase this client's failures on
				// other accounts and enable password spraying.
				if i == 1 {
					state.failures = 0
					state.blockedUntil = time.Time{}
				}
				continue
			}
			if state.failures < loginFailureAllowance+10 {
				state.failures++
			}
			if state.failures >= loginFailureAllowance {
				backoff := time.Second << uint(state.failures-loginFailureAllowance)
				if backoff > loginThrottleMaxBackoff {
					backoff = loginThrottleMaxBackoff
				}
				state.blockedUntil = completed.Add(backoff)
				state.touched = completed
			}
		}
	}, 0
}

func (a *apiHandlers) loginClientIP(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if a.sessionPolicy.Proxies.Peer(r) {
		// Walk from the nearest hop; taking the first XFF value would trust an
		// attacker-supplied prefix that the trusted edge merely appended to.
		hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop := strings.TrimSpace(hops[i])
			ip := net.ParseIP(hop)
			if ip == nil {
				break
			}
			peer = ip.String()
			if !a.sessionPolicy.Proxies.Listed(peer) {
				break
			}
		}
	}
	if ip := net.ParseIP(peer); ip != nil {
		return ip.String()
	}
	return "unknown"
}

func loginThrottled(w http.ResponseWriter, wait time.Duration) {
	seconds := int64((wait + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	w.Header().Set("Cache-Control", "no-store")
	writeProductError(w, http.StatusTooManyRequests, ErrorLoginThrottled, "too many login attempts; try again later", nil)
}
