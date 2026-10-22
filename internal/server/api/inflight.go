package api

import "sync"

// inFlightLimiter counts each user's requests in flight against limit:
// questions to the assistant, tool calls over MCP.
type inFlightLimiter struct {
	limit    int
	mu       sync.Mutex
	inFlight map[string]int
}

func (l *inFlightLimiter) acquire(user string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight[user] >= l.limit {
		return false
	}
	if l.inFlight == nil {
		l.inFlight = map[string]int{}
	}
	l.inFlight[user]++
	return true
}

func (l *inFlightLimiter) release(user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight[user]--; l.inFlight[user] <= 0 {
		delete(l.inFlight, user)
	}
}
