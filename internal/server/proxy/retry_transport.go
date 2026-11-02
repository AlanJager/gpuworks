package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type tunnelOpenError struct{ err error }

func (e *tunnelOpenError) Error() string { return "open proxy tunnel: " + e.err.Error() }
func (e *tunnelOpenError) Unwrap() error { return e.err }

type routeAttempt struct {
	candidates   []candidate
	current      *lease
	tried        map[string]bool
	started      time.Time
	attempts     int
	modelName    string
	policy       string
	affinity     string
	deploymentID uint
	accountant   *Accountant
}

type retryTransport struct {
	base     http.RoundTripper
	balancer *balancer
	hub      Tunneler
	// awake reports whether a leased instance may still take the request
	// (Proxy.awake); nil takes every lease.
	awake func(*lease) bool
	guard *instanceGuard
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	a, _ := req.Context().Value(attemptKey).(*routeAttempt)
	if a == nil || a.current == nil {
		tgt, _ := req.Context().Value(targetKey).(target)
		return t.guard.roundTrip(t.base, req, candidate{instanceUUID: tgt.instanceUUID, port: tgt.port})
	}
	if a.started.IsZero() {
		a.started = time.Now()
	}
	for {
		resp, err := t.guard.roundTrip(t.base, req, a.current.c)
		if err == nil {
			return resp, nil
		}
		if a.current != nil {
			outcome := outcomeConnFailure
			if req.Context().Err() != nil || errors.Is(err, context.Canceled) {
				outcome = outcomeCanceled
			}
			a.current.release(outcome)
			a.current = nil
		}
		if !t.canRetry(req, a, err) {
			return nil, err
		}
		next := t.pickNext(a)
		if next == nil {
			return nil, err
		}
		a.current = next
		if a.accountant != nil {
			a.accountant.RecordRouted(a.deploymentID, next.c.instanceUUID)
		}
		if a.accountant != nil {
			reason := "no_response"
			var tunnelErr *tunnelOpenError
			if errors.As(err, &tunnelErr) {
				reason = "tunnel_open"
			}
			a.accountant.RecordRetry(a.deploymentID, reason)
		}
		a.tried[next.c.instanceUUID] = true
		a.attempts++
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			next.release(outcomeCanceled)
			a.current = nil
			return nil, bodyErr
		}
		clone := req.Clone(req.Context())
		clone.Body = body
		clone.ContentLength = req.ContentLength
		clone.GetBody = req.GetBody
		req = clone
	}
}

func (t *retryTransport) pickNext(a *routeAttempt) *lease {
	return leaseUntil(func(excluded map[string]bool) *lease {
		return t.balancer.pickBy(a.policy, a.candidates, a.affinity, excluded, t.hub.Online)
	}, a.tried, func(l *lease) bool {
		if l.c.modelName != a.modelName {
			return false
		}
		return t.awake == nil || t.awake(l)
	})
}

func (t *retryTransport) canRetry(req *http.Request, a *routeAttempt, err error) bool {
	if req.Context().Err() != nil || a.attempts >= retryMaxAttempts || req.GetBody == nil {
		return false
	}
	if a.started.IsZero() {
		return true
	}
	var tunnelErr *tunnelOpenError
	return !errors.Is(err, context.Canceled) && (errors.As(err, &tunnelErr) || time.Since(a.started) < retryWindow)
}

func (t *retryTransport) String() string { return fmt.Sprintf("retryTransport(%T)", t.base) }
